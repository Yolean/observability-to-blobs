// dev-feed-backfill: ships to a developer feed's bucket the history that its
// collectors did not, because it was written before they ran
// (developer-feed/backfill/README.md):
//
//	dev-feed-backfill [--category <a,b>]           say what it would ship
//	dev-feed-backfill [--category <a,b>] --ship    ship it
//	dev-feed-backfill [--category <a,b>] --out <dir>
//	                                               write the objects there
//
// Each category keeps its own state, in the backfill/ directory of the
// agent's state, so a run takes on a category the feed has never had
// without shipping the others again. A category whose collector has run
// is shipped up to where the collector began, once, and then left to it;
// one whose collector has not run yet is shipped up to now, and the next
// run goes on from there. The state is saved only after every object is
// uploaded: a failed run ships its lines again, never loses one.
//
// The lines are the collectors' rows, with the source's own time as ts and
// the run in backfill, in objects under the hour of their first line, as
// the agent's are (HOST_MODE.md, Where it writes).
//
//go:generate cp ../../compactor/sigv4.go ../../compactor/sigv4_test.go .
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	// tsLayout is ts as the agent renders it: microseconds, UTC
	tsLayout = "2006-01-02T15:04:05.000000Z"
	// maxLine is the claude collector's buffer_max_size: tail skips longer lines
	maxLine = 16 << 20
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// settings are where the run reads, and where its objects go: the agent's
// own, from the environment or else the config file (lib/config.sh).
type settings struct {
	home, state, configDir   string
	bucket, endpoint, region string
	root, cluster, node      string
	bin                      string // where dev-feed-git and dev-feed-cmux are
}

func loadSettings() settings {
	home, _ := os.UserHomeDir()
	cfgDir := os.Getenv("OBSERVABILITY_HOST_CONFIG")
	if cfgDir == "" {
		cfgDir = filepath.Join(home, ".config", "observability-host", "v1")
	}
	cfg := readConfig(filepath.Join(cfgDir, "env"))
	state := os.Getenv("HOST_STATE_DIR")
	if state == "" {
		state = os.Getenv("OBSERVABILITY_HOST_STATE")
	}
	if state == "" {
		state = filepath.Join(home, ".local", "state", "observability-host", "v1")
	}
	bin := ""
	if exe, err := os.Executable(); err == nil {
		bin = filepath.Dir(exe)
	}
	return settings{
		home: home, state: state, configDir: cfgDir,
		bucket:   setting(cfg, "S3_BUCKET", ""),
		endpoint: setting(cfg, "S3_ENDPOINT", ""),
		region:   setting(cfg, "S3_REGION", "us-east-1"),
		root:     setting(cfg, "KEY_ROOT", "dev1"),
		cluster:  setting(cfg, "CLUSTER_NAME", ""),
		node:     setting(cfg, "NODE_NAME", ""),
		bin:      bin,
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: dev-feed-backfill [--category <a,b>] [--ship | --out <dir>]

Without --ship or --out it says what it would ship, and ships nothing.
Categories:
`)
	for _, c := range categories {
		fmt.Fprintf(w, "  %-18s %s\n", c.name, c.about)
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("dev-feed-backfill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }
	ship := fs.Bool("ship", false, "upload the objects, then record what was shipped")
	out := fs.String("out", "", "write the objects under this directory, and record nothing")
	only := fs.String("category", "", "these categories only, comma-separated")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || (*ship && *out != "") {
		usage(stderr)
		return 2
	}
	chosen, err := choose(*only)
	if err != nil {
		fmt.Fprintln(stderr, "dev-feed-backfill:", err)
		return 2
	}
	s := loadSettings()
	if s.cluster == "" || s.node == "" {
		fmt.Fprintln(stderr, "dev-feed-backfill: CLUSTER_NAME and NODE_NAME are not set, in the environment or the config (developer-feed/macos/install.sh)")
		return 2
	}
	var dest *bucket
	if *ship {
		if s.bucket == "" || s.endpoint == "" {
			fmt.Fprintln(stderr, "dev-feed-backfill: S3_BUCKET and S3_ENDPOINT are not set (developer-feed/cloudflare/setup-bucket.sh)")
			return 2
		}
		cred, err := credentials(s.configDir)
		if err != nil {
			fmt.Fprintln(stderr, "dev-feed-backfill:", err)
			return 1
		}
		dest = newBucket(s.endpoint, s.bucket, s.region, cred)
	}
	if err := backfillRun(s, chosen, dest, *out, time.Now(), stdout); err != nil {
		fmt.Fprintln(stderr, "dev-feed-backfill:", err)
		return 1
	}
	return 0
}

func choose(names string) ([]category, error) {
	if names == "" {
		return categories, nil
	}
	var cs []category
	for _, n := range strings.Split(names, ",") {
		found := false
		for _, c := range categories {
			if c.name == n {
				cs, found = append(cs, c), true
			}
		}
		if !found {
			return nil, fmt.Errorf("no category %q (dev-feed-backfill --help lists them)", n)
		}
	}
	return cs, nil
}

// backfill is one run: what it read, and what it will save once shipped.
type backfill struct {
	settings
	dir   string // the backfill's state
	runID string
	now   time.Time
	sp    *spool
	stats map[string]*stats
	saves []func() error
	tmp   string
	// read once a run
	live       map[string]map[string]liveFile
	startTimes []int64
}

type stats struct {
	rows        int
	bytes       int
	first, last time.Time
	notes       []string
}

func (st *stats) note(format string, a ...any) {
	st.notes = append(st.notes, fmt.Sprintf(format, a...))
}

// emit adds a row: the collector's fields, the machine's, the line's time
// and the run.
func (b *backfill) emit(cat string, ts time.Time, row map[string]any) error {
	row["cluster"], row["node"] = b.cluster, b.node
	row["ts"] = ts.UTC().Format(tsLayout)
	row["backfill"] = b.runID
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(row); err != nil {
		return err
	}
	line := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	st := b.stats[cat]
	st.rows++
	st.bytes += len(line)
	if st.first.IsZero() || ts.Before(st.first) {
		st.first = ts
	}
	if ts.After(st.last) {
		st.last = ts
	}
	return b.sp.add(ts, line)
}

func backfillRun(s settings, cats []category, dest *bucket, outDir string, now time.Time, w io.Writer) error {
	dir := filepath.Join(s.state, "backfill")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lock(filepath.Join(dir, "lock"))
	if err != nil {
		return err
	}
	defer unlock()
	tmp, err := os.MkdirTemp(dir, "run-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	b := &backfill{settings: s, dir: dir, runID: now.UTC().Format("20060102T150405Z"), now: now,
		sp: newSpool(filepath.Join(tmp, "spool")), stats: map[string]*stats{}, tmp: tmp, live: map[string]map[string]liveFile{}}

	for _, c := range cats {
		b.stats[c.name] = &stats{}
		save, err := c.collect(b)
		if err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
		if save != nil {
			b.saves = append(b.saves, save)
		}
	}

	to := "nowhere"
	switch {
	case dest != nil:
		to = fmt.Sprintf("s3://%s/%s/%s/", s.bucket, s.root, s.cluster)
	case outDir != "":
		to = outDir
	}
	fmt.Fprintf(w, "dev-feed-backfill: run %s, machine %s of %s, to %s\n", b.runID, s.node, s.cluster, to)
	for _, c := range cats {
		st := b.stats[c.name]
		span := ""
		if st.rows > 0 {
			span = fmt.Sprintf(", %s to %s", st.first.UTC().Format("2006-01-02 15:04"), st.last.UTC().Format("2006-01-02 15:04"))
		}
		fmt.Fprintf(w, "  %-18s %d lines, %s%s\n", c.name, st.rows, size(st.bytes), span)
		for _, n := range st.notes {
			fmt.Fprintf(w, "  %-18s   %s\n", "", n)
		}
	}

	var keys []string
	var objects, compressed int
	err = b.sp.objects(func(o object) error {
		objects++
		compressed += len(o.body)
		keys = append(keys, o.key)
		switch {
		case dest != nil:
			return dest.put(o.key, o.body)
		case outDir != "":
			p := filepath.Join(outDir, filepath.FromSlash(o.key))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			return os.WriteFile(p, o.body, 0o644)
		}
		return nil
	}, s.root, s.cluster, s.node)
	if err != nil {
		return fmt.Errorf("after %d objects: %w; nothing is recorded as shipped, and a run again ships it all again", objects, err)
	}
	fmt.Fprintf(w, "  %d objects, %s compressed\n", objects, size(compressed))
	if dest == nil {
		if outDir == "" {
			fmt.Fprintln(w, "nothing shipped: --ship uploads them, --out <dir> writes them there")
		} else {
			fmt.Fprintf(w, "written under %s; nothing recorded as shipped\n", outDir)
		}
		return nil
	}
	for _, save := range b.saves {
		if err := save(); err != nil {
			return fmt.Errorf("shipped, but not recorded: %w; a run again ships some lines twice", err)
		}
	}
	if len(keys) == 0 {
		fmt.Fprintln(w, "nothing to ship")
		return nil
	}
	// what each run wrote, for redacting a run later
	sort.Strings(keys)
	keysFile := filepath.Join(dir, "run-"+b.runID+".keys")
	if err := os.WriteFile(keysFile, []byte(strings.Join(keys, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(w, "shipped; the objects' keys are in %s\n", keysFile)
	return nil
}

func size(n int) string {
	switch {
	case n >= 10<<20:
		return fmt.Sprintf("%d MB", n>>20)
	case n >= 10<<10:
		return fmt.Sprintf("%d kB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// lock keeps a second run from reading the same state.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another dev-feed-backfill is running")
	}
	return func() { f.Close() }, nil
}

// loadJSON reads v from path; a missing file leaves v as it is.
func loadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// saveJSON writes v whole, then moves it into place.
func saveJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

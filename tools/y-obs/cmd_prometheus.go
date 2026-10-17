package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
)

const prometheusUsage = `Usage: y-obs prometheus [flags]

A local Prometheus over the bucket, for exploring metrics in its UI:
autocomplete, graphs, any PromQL. Prints its URL and opens it in the
browser.

- The window (--since, default 1h) is loaded at start, so its names
  autocomplete and its range is local; without --to it keeps loading
  what arrives.
- Older ranges load from the bucket when a query reaches them: zoom out
  in the UI. A query too broad for --max-objects or --max-samples comes
  back with a warning saying so.
- One instance per context and cluster: a repeated call reuses it.
  --stop ends it; --status lists the running ones.
- Its Targets page has one target, y-obs itself, whose yobs_* metrics
  say what was loaded and read.

Needs the Prometheus server: ystack's y-prometheus, prometheus on PATH, or
PROMETHEUS.

Examples:
  y-obs prometheus
  y-obs prometheus --cluster prod --since 6h
  y-obs prometheus --from '2026-10-11 08:00' --to '2026-10-11 10:00'
  y-obs prometheus --status
  y-obs prometheus --stop

Flags:
`

// promState is a running instance, as y-obs prometheus records it.
type promState struct {
	PID     int       `json:"pid"`
	URL     string    `json:"url"`
	Cluster string    `json:"cluster"`
	Context string    `json:"context"`
	Window  string    `json:"window"`
	Started time.Time `json:"started"`
	Log     string    `json:"log"`
	// Asked is the window flags it was started with, to tell a reuse
	// from a request for another window
	Asked string `json:"asked,omitempty"`
}

func promStateDir() string { return filepath.Join(cacheDir(), "prometheus") }

var unsafeKey = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func promKey(contextName, cluster string) string {
	if contextName == "" {
		contextName = "env"
	}
	return unsafeKey.ReplaceAllString(contextName+"-"+cluster, "_")
}

func readPromState(path string) (*promState, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var st promState
	if json.Unmarshal(b, &st) != nil || st.PID == 0 {
		return nil, false
	}
	return &st, st.alive()
}

// alive is the server process existing and its Prometheus answering ready.
func (st *promState) alive() bool {
	if syscall.Kill(st.PID, 0) != nil || st.URL == "" {
		return false
	}
	c := &http.Client{Timeout: 3 * time.Second}
	res, err := c.Get(st.URL + "/-/ready")
	if err != nil {
		return false
	}
	res.Body.Close()
	return res.StatusCode == http.StatusOK
}

func writePromState(path string, st *promState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(path, b, 0o600)
}

// lifecycleFlags are y-obs prometheus's own; the rest go to the server.
var lifecycleFlags = map[string]bool{"stop": true, "status": true, "no-open": true}

func serveArgs(args []string) []string {
	var out []string
	for _, a := range args {
		name := strings.TrimLeft(strings.SplitN(a, "=", 2)[0], "-")
		if strings.HasPrefix(a, "-") && lifecycleFlags[name] {
			continue
		}
		out = append(out, a)
	}
	return out
}

func cmdPrometheus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prometheus", flag.ContinueOnError)
	var sf sessionFlags
	sf.register(fs)
	fs.Int("port", 0, "Prometheus's port on 127.0.0.1; 0 picks a free one")
	fs.Duration("poll", 30*time.Second, "without --to, load what arrived in the bucket this often; 0 loads once")
	fs.Int("max-samples", 20000000, "refuse a query from the bucket returning more samples")
	stop := fs.Bool("stop", false, "stop the instance of this context and cluster")
	status := fs.Bool("status", false, "list the running instances")
	noOpen := fs.Bool("no-open", false, "print the URL without opening a browser")
	fs.Usage = func() { fmt.Fprint(fs.Output(), prometheusUsage); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return &exitError{code: exitOK}
		}
		return fail(exitUsage, "%v; run y-obs prometheus -h", err)
	}
	if fs.NArg() > 0 {
		return fail(exitUsage, "prometheus takes only flags; run y-obs prometheus -h")
	}
	if *status {
		return promStatus()
	}
	cfg, _, err := loadConfig()
	if err != nil {
		return err
	}
	contextName := contextName(sf.context, cfg)
	cluster, err := resolveCluster(ctx, &sf)
	if err != nil {
		return err
	}
	key := promKey(contextName, cluster)
	statePath := filepath.Join(promStateDir(), key+".json")
	st, alive := readPromState(statePath)
	if *stop {
		if st == nil || syscall.Kill(st.PID, 0) != nil {
			os.Remove(statePath)
			return fail(exitNoRows, "no running instance for %s", key)
		}
		syscall.Kill(st.PID, syscall.SIGTERM)
		for i := 0; i < 60 && syscall.Kill(st.PID, 0) == nil; i++ {
			time.Sleep(250 * time.Millisecond)
		}
		if syscall.Kill(st.PID, 0) == nil {
			syscall.Kill(st.PID, syscall.SIGKILL)
		}
		os.Remove(statePath)
		os.Remove(filepath.Join(promStateDir(), key+".ready"))
		os.Remove(filepath.Join(promStateDir(), key+".progress"))
		fmt.Fprintf(os.Stderr, "y-obs: stopped %s (%s); its log stays in %s\n", key, st.URL, st.Log)
		return nil
	}
	asked := windowFlags(fs)
	if alive && asked != "" && asked != st.Asked {
		was := st.Asked
		if was == "" {
			was = "started without window flags, the default --since 1h"
		}
		return fail(exitUsage, "an instance for %s runs with window %s (%s); y-obs prometheus --stop first, or call it without --since/--from/--to to reuse it",
			key, st.Window, was)
	}
	if alive {
		// still loading: show it, and return once loaded
		if err := waitLoaded(ctx, filepath.Join(promStateDir(), key+".progress"), nil, func(string) {}); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "y-obs: reusing %s for %s, loaded %s, started %s ago; older ranges load on query\n",
			st.URL, key, st.Window, time.Since(st.Started).Round(time.Second))
		fmt.Println(st.URL)
		openBrowser(st.URL, *noOpen)
		return nil
	}
	w, err := sf.window(time.Now())
	if err != nil {
		return fail(exitUsage, "%v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(promStateDir(), 0o700); err != nil {
		return err
	}
	ready := filepath.Join(promStateDir(), key+".ready")
	logPath := filepath.Join(promStateDir(), key+".log")
	os.Remove(ready)
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	sargs := append([]string{"prometheus-serve"}, serveArgs(args)...)
	if sf.cluster == "" {
		sargs = append(sargs, "--cluster", cluster)
	}
	progressPath := filepath.Join(promStateDir(), key+".progress")
	os.Remove(progressPath)
	sargs = append(sargs, "--ready-file", ready, "--progress-file", progressPath)
	cmd := exec.Command(exe, sargs...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	logf.Close()
	st = &promState{PID: cmd.Process.Pid, Cluster: cluster, Context: contextName, Window: w.String(), Started: time.Now().UTC(), Log: logPath, Asked: asked}
	if err := writePromState(statePath, st); err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	fmt.Fprintf(os.Stderr, "y-obs: starting Prometheus for %s, loading %s\n", key, w)
	opened := false
	err = waitLoaded(ctx, progressPath, exited, func(url string) {
		// names autocomplete: open the browser while the samples load
		st.URL = url
		writePromState(statePath, st)
		if !opened {
			opened = true
			openBrowser(url, *noOpen)
		}
	})
	if err != nil {
		if errors.Is(err, errServerExited) {
			os.Remove(statePath)
			tail, _ := os.ReadFile(logPath)
			lines := strings.Split(strings.TrimSpace(string(tail)), "\n")
			return fail(exitStore, "the server stopped: %s (log %s)", lines[len(lines)-1], logPath)
		}
		syscall.Kill(st.PID, syscall.SIGTERM)
		os.Remove(statePath)
		return err
	}
	if b, err := os.ReadFile(logPath); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "y-obs: loaded ") {
				fmt.Fprintln(os.Stderr, l)
			}
		}
	}
	fmt.Fprintf(os.Stderr, "y-obs: %s for %s; older ranges load on query; y-obs prometheus --stop ends it\n", st.URL, key)
	fmt.Println(st.URL)
	if !opened {
		openBrowser(st.URL, *noOpen)
	}
	return nil
}

var errServerExited = errors.New("the server exited")

// waitLoaded shows the server's progress until its window is loaded: one
// updating line on a terminal, a line per stage and about every two
// seconds otherwise. catalog is called with the URL once names
// autocomplete.
func waitLoaded(ctx context.Context, progressPath string, exited <-chan struct{}, catalog func(url string)) error {
	tty := isTerminal(os.Stderr)
	var last loadProgress
	var lastLine time.Time
	called := false
	defer func() {
		if tty {
			fmt.Fprint(os.Stderr, "\r\033[K")
		}
	}()
	for {
		var l loadProgress
		b, err := os.ReadFile(progressPath)
		if err == nil {
			json.Unmarshal(b, &l)
		} else if exited == nil {
			// a reused instance without progress (an older y-obs) is loaded
			return nil
		}
		if exited == nil && l.Stage == "loaded" && !called {
			// a reused instance that finished loading: nothing to show
			return nil
		}
		if l.URL != "" && !called && (l.Stage == "loading" || l.Stage == "loaded") {
			called = true
			if tty {
				fmt.Fprint(os.Stderr, "\r\033[K")
			}
			fmt.Fprintf(os.Stderr, "y-obs: %s: names autocomplete; loading the samples\n", l.URL)
			catalog(l.URL)
		}
		switch l.Stage {
		case "loaded":
			return nil
		case "failed":
			// the server's own exit code: 5 over budget, 4 nothing there
			code := l.Code
			if code == 0 {
				code = exitStore
			}
			return fail(code, "%s", l.Error)
		}
		if line := progressLine(l); line != "" {
			switch {
			case tty:
				fmt.Fprint(os.Stderr, "\r\033[K"+line)
			case l.Stage != last.Stage || time.Since(lastLine) > 2*time.Second && l.Pushed != last.Pushed:
				fmt.Fprintln(os.Stderr, line)
				lastLine = time.Now()
			}
		}
		last = l
		select {
		case <-exited:
			// the server may have written its failure just before exiting
			if b, err := os.ReadFile(progressPath); err == nil {
				var f loadProgress
				if json.Unmarshal(b, &f) == nil && f.Stage == "failed" {
					code := f.Code
					if code == 0 {
						code = exitStore
					}
					return fail(code, "%s", f.Error)
				}
			}
			return errServerExited
		case <-ctx.Done():
			return &exitError{code: exitStore, msg: "interrupted while loading; stopped"}
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func progressLine(l loadProgress) string {
	switch l.Stage {
	case "planning":
		return "y-obs: planning " + l.Window
	case "reading":
		return fmt.Sprintf("y-obs: reading %d objects", l.Objects)
	case "catalog":
		return fmt.Sprintf("y-obs: read %d samples of %d series; loading names", l.Samples, l.Series)
	case "loading":
		pct := 0
		if l.Samples > 0 {
			pct = 100 * l.Pushed / l.Samples
		}
		return fmt.Sprintf("y-obs: loading samples: %d of %d (%d%%)", l.Pushed, l.Samples, pct)
	}
	return ""
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// windowFlags are the window flags given, as written: "" when none was.
func windowFlags(fs *flag.FlagSet) string {
	var parts []string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "since", "from", "to":
			parts = append(parts, "--"+f.Name+"="+f.Value.String())
		}
	})
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// promInstance is a running instance and its record's name.
type promInstance struct {
	name string
	*promState
}

// promInstances are the running instances; stale records are dropped.
func promInstances() ([]promInstance, error) {
	entries, err := os.ReadDir(promStateDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []promInstance
	for _, name := range names {
		st, alive := readPromState(filepath.Join(promStateDir(), name))
		if !alive {
			os.Remove(filepath.Join(promStateDir(), name))
			continue
		}
		out = append(out, promInstance{strings.TrimSuffix(name, ".json"), st})
	}
	return out, nil
}

func promStatus() error {
	inst, err := promInstances()
	if err != nil {
		return err
	}
	fmt.Println("instance\turl\tcluster\tloaded\tstarted\tpid")
	for _, i := range inst {
		fmt.Printf("%s\t%s\t%s\t%s\t%s\t%d\n", i.name, i.URL, i.Cluster, i.Window, i.Started.Format(time.RFC3339), i.PID)
	}
	if len(inst) == 0 {
		return &exitError{code: exitNoRows}
	}
	return nil
}

// openBrowser opens url, unless told not to or there is no display; a
// failure only means the URL has to be opened by hand.
func openBrowser(url string, no bool) {
	if no {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err == nil {
		go cmd.Wait()
	}
}

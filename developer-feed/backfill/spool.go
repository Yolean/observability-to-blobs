package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	// maxObject is the uncompressed size of an object, at most, unless one
	// line is longer
	maxObject = 32 << 20
	// spoolMemory is how much the spool holds before it appends to its files
	spoolMemory = 64 << 20
)

// spool keeps a run's lines by the UTC hour of their time, on disk past
// spoolMemory: a whole backfill can be gigabytes. A line is kept as its
// time, in nanoseconds and fixed width, a tab, and the line.
type spool struct {
	dir   string
	bufs  map[string]*bytes.Buffer
	held  int
	hours map[string]bool
}

func newSpool(dir string) *spool {
	return &spool{dir: dir, bufs: map[string]*bytes.Buffer{}, hours: map[string]bool{}}
}

func (s *spool) add(ts time.Time, line []byte) error {
	h := ts.UTC().Format("2006-01-02T15")
	buf := s.bufs[h]
	if buf == nil {
		buf = &bytes.Buffer{}
		s.bufs[h] = buf
	}
	n := ts.UnixNano()
	if n < 0 {
		n = 0
	}
	fmt.Fprintf(buf, "%020d\t", n)
	buf.Write(line)
	buf.WriteByte('\n')
	s.held += len(line) + 22
	s.hours[h] = true
	if s.held > spoolMemory {
		return s.flush()
	}
	return nil
}

func (s *spool) flush() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	for h, buf := range s.bufs {
		f, err := os.OpenFile(filepath.Join(s.dir, h), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = f.Write(buf.Bytes())
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	s.bufs = map[string]*bytes.Buffer{}
	s.held = 0
	return nil
}

// object is one object of the run, compressed.
type object struct {
	key  string
	body []byte
}

// objects hands each object to fn, hour by hour: the hour's lines in the
// order of their time, cut where one more line would make it larger than
// maxObject.
func (s *spool) objects(fn func(object) error, root, cluster, node string) error {
	if err := s.flush(); err != nil {
		return err
	}
	hours := make([]string, 0, len(s.hours))
	for h := range s.hours {
		hours = append(hours, h)
	}
	sort.Strings(hours)
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return err
	}
	defer enc.Close()
	for _, h := range hours {
		b, err := os.ReadFile(filepath.Join(s.dir, h))
		if err != nil {
			return err
		}
		lines := bytes.SplitAfter(b, []byte("\n"))
		if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
			lines = lines[:n-1]
		}
		// the fixed-width time sorts as text; stable keeps a file's order
		sort.SliceStable(lines, func(i, j int) bool { return bytes.Compare(lines[i][:20], lines[j][:20]) < 0 })
		var raw []byte
		var first int64
		cut := func() error {
			if len(raw) == 0 {
				return nil
			}
			o := object{key: objectKey(root, cluster, node, time.Unix(0, first), raw), body: enc.EncodeAll(raw, nil)}
			raw = nil
			return fn(o)
		}
		for _, l := range lines {
			line := l[21:]
			if len(raw) > 0 && len(raw)+len(line) > maxObject {
				if err := cut(); err != nil {
					return err
				}
			}
			if len(raw) == 0 {
				first, _ = strconv.ParseInt(string(l[:20]), 10, 64)
			}
			raw = append(raw, line...)
		}
		if err := cut(); err != nil {
			return err
		}
		if err := os.Remove(filepath.Join(s.dir, h)); err != nil {
			return err
		}
	}
	return nil
}

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// objectKey is the agent's key (images/fluentbit/host/host.yaml,
// s3_key_format) for an object whose first line is at t: its date, hour,
// minute and second. Where the agent has 8 random characters, this has 8
// of the content's hash, in the same alphabet.
func objectKey(root, cluster, node string, t time.Time, raw []byte) string {
	t = t.UTC()
	sum := sha256.Sum256(raw)
	n := new(big.Int).SetBytes(sum[:])
	suffix := make([]byte, 8)
	m := new(big.Int)
	for i := range suffix {
		n.DivMod(n, big.NewInt(62), m)
		suffix[i] = base62[m.Int64()]
	}
	return fmt.Sprintf("%s/%s/%s/%s/%s/%s-%s.jsonl.zst", root, cluster, t.Format("2006-01-02"), t.Format("15"), node, t.Format("0405"), suffix)
}

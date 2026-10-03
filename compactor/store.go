package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Object is a key and its upload time.
type Object struct {
	Key      string
	Modified time.Time
}

// RawSource is where raw objects come from: it lists an hour's keys, hands
// DuckDB a path it reads each key at, and deletes keys once a committed
// fold took them (RAW_DELETE). A bucket is one; a local spool fed by an S3
// PutObject endpoint would be another.
type RawSource interface {
	List(ctx context.Context, prefix string) ([]Object, error)
	Dirs(ctx context.Context, prefix string) ([]string, error)
	URL(key string) string
	Delete(ctx context.Context, keys []string) error
}

// TierStore is where the hourly and daily tiers go. DuckDB reads and writes
// the paths URL returns, every write a COPY to a temporary key; the store
// lists, tells whether a key exists, prepares a key for a COPY, moves a
// checked temporary object to its key, and deletes temporary objects. It
// replaces an object whole or not at all, and deletes nothing but keys
// under a tier's _tmp/.
type TierStore interface {
	List(ctx context.Context, prefix string) ([]Object, error)
	Dirs(ctx context.Context, prefix string) ([]string, error)
	Stat(ctx context.Context, key string) (bool, error)
	URL(key string) string
	Prepare(key string) error
	Move(ctx context.Context, from, to string) error
	Delete(ctx context.Context, keys []string) error
}

// dirBucket is a bucket in a local directory, for tests and for running the
// compactor over a copy of a bucket. DuckDB reads and writes local paths the
// way it does s3:// ones, except that a COPY needs the directory to exist.
type dirBucket struct{ root string }

func (d dirBucket) URL(key string) string { return filepath.Join(d.root, filepath.FromSlash(key)) }

func (d dirBucket) Prepare(key string) error {
	return os.MkdirAll(filepath.Dir(d.URL(key)), 0o755)
}

// Move renames from to to, which replaces to atomically on one file system.
func (d dirBucket) Move(_ context.Context, from, to string) error {
	if err := d.Prepare(to); err != nil {
		return err
	}
	return os.Rename(d.URL(from), d.URL(to))
}

func (d dirBucket) Stat(_ context.Context, key string) (bool, error) {
	_, err := os.Stat(d.URL(key))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// List walks the directory the prefix names down to, and returns the keys
// that start with the prefix, sorted, as an S3 listing would.
func (d dirBucket) List(_ context.Context, prefix string) ([]Object, error) {
	dir := prefix
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		dir = dir[:i]
	} else {
		dir = ""
	}
	var out []Object
	err := filepath.WalkDir(d.URL(dir), func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if e.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(d.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		out = append(out, Object{Key: key, Modified: info.ModTime()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, err
}

// Dirs returns the "directories" right under prefix, which ends in "/":
// each a prefix ending in "/", sorted, as an S3 listing with delimiter "/"
// returns them.
func (d dirBucket) Dirs(_ context.Context, prefix string) ([]string, error) {
	entries, err := os.ReadDir(d.URL(prefix))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, prefix+e.Name()+"/")
		}
	}
	return out, err
}

func (d dirBucket) Delete(_ context.Context, keys []string) error {
	for _, k := range keys {
		if err := os.Remove(d.URL(k)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

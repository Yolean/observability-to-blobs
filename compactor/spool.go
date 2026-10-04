package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// spool is the raw source of RAW_SOURCE=spool: a local directory, normally
// a persistent volume, that the put endpoint (put.go) fills with the raw
// objects the agents upload, each under the key it was uploaded to. A fold
// lists an hour's keys here, DuckDB reads the files where they are, and
// once the hour's final fold has committed its files are deleted: the
// spool is a buffer between the agents and the fold, not storage.
//
// A file is written to _tmp/, synced, renamed to its key and its directory
// synced, so a key holds a whole object that survives a crash, or nothing.
type spool struct {
	root string
	// mu orders a put's new directories and rename against Delete, which
	// removes the directories it empties
	mu sync.Mutex
	// durable holds the directories whose entry in their parent this
	// process has synced: a directory that exists without being here (a
	// sync failed and so did its removal, or an earlier process made it)
	// has its parent synced before a put that needs it is acknowledged
	durable map[string]bool
	// syncFile and syncDir make a write durable, remove undoes one that
	// failed; tests replace them
	syncFile func(*os.File) error
	syncDir  func(path string) error
	remove   func(path string) error
}

// spoolTmp holds the writes in progress, on the spool's file system so
// that the rename to the key is atomic. No raw key starts with it.
const spoolTmp = "_tmp"

// openSpool makes root a spool. What _tmp/ holds is a write that a crash
// interrupted before its rename, never acknowledged; it goes.
func openSpool(root string) (*spool, error) {
	root = filepath.Clean(root)
	s := &spool{root: root, durable: map[string]bool{}, syncFile: (*os.File).Sync, syncDir: syncDir, remove: os.Remove}
	tmp := filepath.Join(root, spoolTmp)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, fmt.Errorf("spool %s: %w", root, err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return nil, fmt.Errorf("spool %s: %w", root, err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(tmp, e.Name())); err != nil {
			return nil, fmt.Errorf("spool %s: %w", root, err)
		}
	}
	return s, nil
}

func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *spool) dir() dirBucket { return dirBucket{root: s.root} }

func (s *spool) URL(key string) string { return s.dir().URL(key) }

func (s *spool) List(ctx context.Context, prefix string) ([]Object, error) {
	return s.dir().List(ctx, prefix)
}

func (s *spool) Dirs(ctx context.Context, prefix string) ([]string, error) {
	return s.dir().Dirs(ctx, prefix)
}

// Delete removes the files of keys, and then every directory of theirs
// that is left empty, up to the spool's root, so that the past pass does
// not list the hours of an emptied spool forever.
func (s *spool) Delete(_ context.Context, keys []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dirs := map[string]bool{}
	for _, k := range keys {
		p := s.URL(k)
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		dirs[filepath.Dir(p)] = true
	}
	for d := range dirs {
		// Remove refuses a directory that is not empty, which ends the walk
		for ; d != s.root && strings.HasPrefix(d, s.root+string(filepath.Separator)); d = filepath.Dir(d) {
			if err := os.Remove(d); err != nil {
				break
			}
			delete(s.durable, d)
		}
	}
	return nil
}

// spoolWrite is what a write did with its body.
type spoolWrite int

const (
	spoolStored        spoolWrite = iota
	spoolDuplicate                // the key held the same content: nothing written
	spoolConflict                 // the key held other content, which it keeps
	spoolAlreadyFolded            // the key's file is gone and its hour's sidecar lists it: nothing written
)

// errBody is a body that does not match what the request said of it: its
// length, its x-amz-content-sha256 or its Content-MD5.
type errBody struct{ code, msg string }

func (e *errBody) Error() string { return e.code + ": " + e.msg }

// write stores the size bytes of body at key, once they read whole and
// match sha256Hex and, when it is not nil, contentMD5. It returns once the
// file and every directory entry on its path are synced, with the MD5 of
// what the key holds. A key that exists keeps its content: the same
// content is a duplicate, other content a conflict, and neither is
// written; its directory is synced again before either returns, since the
// file may be one whose sync failed and whose removal did too. A key with
// no file that folded, when not nil, says was folded already is not
// written either: a fold deleted its file, and would delete it again
// unread.
func (s *spool) write(key string, body io.Reader, size int64, sha256Hex string, contentMD5 []byte, folded func() (bool, error)) ([]byte, spoolWrite, error) {
	f, err := os.CreateTemp(filepath.Join(s.root, spoolTmp), "*.tmp")
	if err != nil {
		return nil, 0, err
	}
	tmp := f.Name()
	renamed := false
	defer func() {
		if !renamed {
			os.Remove(tmp)
		}
	}()
	sum, md := sha256.New(), md5.New()
	n, err := io.Copy(io.MultiWriter(f, sum, md), io.LimitReader(body, size))
	if err == nil && n < size {
		err = &errBody{"IncompleteBody", fmt.Sprintf("the body has %d bytes, Content-Length says %d", n, size)}
	}
	if err == nil {
		err = checkSums(sum, md, sha256Hex, contentMD5)
	}
	if err == nil {
		err = s.syncFile(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, 0, err
	}
	etag := md.Sum(nil)

	s.mu.Lock()
	defer s.mu.Unlock()
	dst := s.URL(key)
	_, err = os.Lstat(dst)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, 0, err
	}
	// before mkdirs, so that a key already folded leaves no directory
	if !exists && folded != nil {
		done, err := folded()
		if err != nil {
			return nil, 0, err
		}
		if done {
			return etag, spoolAlreadyFolded, nil
		}
	}
	if err := s.mkdirs(filepath.Dir(dst)); err != nil {
		return nil, 0, err
	}
	if exists {
		have, err := fileMD5(dst)
		if err != nil {
			return nil, 0, err
		}
		if err := s.syncDir(filepath.Dir(dst)); err != nil {
			return nil, 0, err
		}
		if bytes.Equal(have, etag) {
			return have, spoolDuplicate, nil
		}
		return have, spoolConflict, nil
	}
	if err := os.Rename(tmp, dst); err != nil {
		return nil, 0, err
	}
	renamed = true
	if err := s.syncDir(filepath.Dir(dst)); err != nil {
		// not acknowledged, so not kept: the agent uploads it again; if
		// the removal fails too, the next put of the key syncs it
		s.remove(dst)
		return nil, 0, err
	}
	return etag, spoolStored, nil
}

func checkSums(sum, md hash.Hash, sha256Hex string, contentMD5 []byte) error {
	if got := fmt.Sprintf("%x", sum.Sum(nil)); got != sha256Hex {
		return &errBody{"XAmzContentSHA256Mismatch", "the body's SHA-256 is " + got + ", x-amz-content-sha256 says " + sha256Hex}
	}
	if contentMD5 != nil && !bytes.Equal(md.Sum(nil), contentMD5) {
		return &errBody{"BadDigest", "the body's MD5 differs from Content-MD5"}
	}
	return nil
}

// mkdirs creates dir and its missing parents under the root, and syncs
// the parent of each directory on the path that is not known durable, the
// ones it creates and any it finds that this process has not synced, so
// the new file's path is durable along with the file. A directory it
// created whose parent's sync fails is removed again: the put answers 500
// and the next one creates it anew.
func (s *spool) mkdirs(dir string) error {
	rel, err := filepath.Rel(s.root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("spool: %s is not under %s", dir, s.root)
	}
	p := s.root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		parent := p
		p = filepath.Join(p, part)
		err := os.Mkdir(p, 0o755)
		created := err == nil
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if !created && s.durable[p] {
			continue
		}
		if err := s.syncDir(parent); err != nil {
			if created {
				s.remove(p)
			}
			return err
		}
		s.durable[p] = true
	}
	return nil
}

func fileMD5(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// spoolStats is the spool's raw files now: how many, their bytes, and the
// oldest file's write time.
type spoolStats struct {
	files, bytes int64
	oldest       time.Time
}

func (s *spool) stats() (spoolStats, error) {
	var st spoolStats
	err := filepath.WalkDir(filepath.Join(s.root, "raw1"), func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			// a directory Delete removed during the walk
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if e.IsDir() {
			return nil
		}
		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		st.files++
		st.bytes += info.Size()
		if st.oldest.IsZero() || info.ModTime().Before(st.oldest) {
			st.oldest = info.ModTime()
		}
		return nil
	})
	return st, err
}

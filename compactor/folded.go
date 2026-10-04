package main

import (
	"context"
	"sync"
	"time"
)

// foldedKeys holds, for the put endpoint, the raw keys that recent hours'
// committed sidecars list. A key's spool file is deleted once its hour's
// final fold has committed, and with it the content a later PUT of the
// key could be compared with; the sidecar still lists the key, and a fold
// would delete a file stored there again without reading it. So a PUT of
// a key whose file is gone and which its hour's sidecar lists is answered
// 200 and dropped (put.go, result "already_folded").
//
// The fold is the sidecars' one writer, and it sets an hour's keys here
// each time it reads or commits that hour's sidecar, before it deletes any
// file, so a PUT that finds the file gone finds the key here. Another
// hour is read from the bucket once, on a PUT's miss, and kept: no read
// per PUT in the common case.
type foldedKeys struct {
	mu    sync.Mutex
	hours map[time.Time]*foldedHour
	// sets counts set calls: a read from the bucket that a set may have
	// overtaken is not kept
	sets uint64
	use  uint64
	read func(ctx context.Context, h time.Time) (map[string]bool, error)
}

type foldedHour struct {
	keys map[string]bool
	used uint64
}

// foldedKeysHours bounds the hours kept, least recently used out first: a
// day and a half of hours of a few thousand keys each, a few MiB.
const foldedKeysHours = 36

func newFoldedKeys(read func(ctx context.Context, h time.Time) (map[string]bool, error)) *foldedKeys {
	return &foldedKeys{hours: map[time.Time]*foldedHour{}, read: read}
}

// set is the keys of h's sidecar as a fold read or committed it. A nil
// foldedKeys (no put endpoint) keeps nothing.
func (f *foldedKeys) set(h time.Time, folded map[string]bool) {
	if f == nil {
		return
	}
	keys := make(map[string]bool, len(folded))
	for k := range folded {
		keys[k] = true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	f.keep(h, keys)
}

func (f *foldedKeys) keep(h time.Time, keys map[string]bool) {
	f.use++
	f.hours[h] = &foldedHour{keys: keys, used: f.use}
	for len(f.hours) > foldedKeysHours {
		var lru time.Time
		var oldest uint64
		for t, e := range f.hours {
			if oldest == 0 || e.used < oldest {
				lru, oldest = t, e.used
			}
		}
		delete(f.hours, lru)
	}
}

func (f *foldedKeys) lookup(h time.Time) (map[string]bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.hours[h]
	if !ok {
		return nil, false
	}
	f.use++
	e.used = f.use
	return e.keys, true
}

// load returns h's keys, read from the bucket on a miss. The read is kept
// only if no set came while it ran: a set of h then has newer keys, or it
// was evicted, and the next miss reads again.
func (f *foldedKeys) load(ctx context.Context, h time.Time) (map[string]bool, error) {
	if keys, ok := f.lookup(h); ok {
		return keys, nil
	}
	f.mu.Lock()
	sets := f.sets
	f.mu.Unlock()
	keys, err := f.read(ctx, h)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.hours[h]; ok {
		return e.keys, nil
	}
	if f.sets == sets {
		f.keep(h, keys)
	}
	return keys, nil
}

// has tells whether h's committed sidecar lists key. The spool calls it
// with its lock held, after finding the key's file gone: a fold deletes
// files under that lock, after setting their hour's keys here, so either
// the keys here are as new as that deletion, or a read now, which comes
// after the deletion's commit, is.
func (f *foldedKeys) has(ctx context.Context, h time.Time, key string) (bool, error) {
	keys, err := f.load(ctx, h)
	return keys[key], err
}

// readHourFolded reads hour h's sidecar from the tiers.
func (c *compactor) readHourFolded(ctx context.Context, h time.Time) (map[string]bool, error) {
	conn, err := c.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return c.readFolded(ctx, conn, c.l.hourlySidecar(h))
}

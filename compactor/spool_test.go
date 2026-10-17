package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// spoolWorld is a world whose raw source is a spool, filled through the put
// endpoint, as RAW_SOURCE=spool runs: raw deletion on, the tiers in the
// world's directory.
type spoolWorld struct {
	*world
	sp  *spool
	put *putServer
}

func newSpoolWorld(t *testing.T) *spoolWorld {
	t.Helper()
	w := newWorld(t)
	sp, err := openSpool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w.c.raw, w.c.rawDelete, w.c.m.spool = sp, true, sp.stats
	w.c.foldedKeys = newFoldedKeys(w.c.readHourFolded)
	return &spoolWorld{world: w, sp: sp, put: &putServer{l: w.c.l, bucket: "b", region: "us-east-1", keyID: "k",
		secret: "s", maxBytes: 1 << 20, spool: sp, folded: w.c.foldedKeys, m: w.c.m,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}}
}

// upload PUTs a raw object through the endpoint and returns its key.
func (w *spoolWorld) upload(key string, lines ...line) string {
	w.t.Helper()
	body := rawObject(w.t, lines...)
	r := httptest.NewRequest(http.MethodPut, "http://compactor:9000/b/"+key, bytes.NewReader(body))
	signV4(r, hashHex(body), "us-east-1", "k", "s", time.Now())
	rec := httptest.NewRecorder()
	w.put.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		w.t.Fatalf("PUT %s: %d %s", key, rec.Code, rec.Body)
	}
	return key
}

func (w *spoolWorld) spooled(key string) bool {
	_, err := os.Stat(w.sp.URL(key))
	return err == nil
}

// The fold reads an hour's keys from the spool, writes the tiers as it
// does from a bucket, with sidecars of the same keys, and deletes the
// hour's files once its final fold has committed, and with them the
// directories they leave empty. A late file in a final hour goes after the
// fold that took it.
func TestSpoolFoldsAndEmptiesAfterTheFinalFold(t *testing.T) {
	w := newSpoolWorld(t)
	a := w.upload(rawKey(h10, "n1", "0000-a"), logs(h10, 5, "ns1", "p1", "u1")...)
	b := w.upload(rawKey(h10, "n2", "3000-b"), logs(h10.Add(30*time.Minute), 3, "ns1", "p1", "u1")...)
	e := w.upload(rawKey(h11, "n1", "0000-e"), logs(h11, 2, "ns1", "p1", "u1")...)

	if r := w.fold(h10, during); r.keys != 2 || r.final || len(r.deleted) != 0 {
		t.Fatalf("fold during the hour: %+v", r)
	}
	if f := w.folded(h10); len(f) != 2 || f[a] || f[b] {
		t.Fatalf("sidecar %v, want the spool's keys, not final", f)
	}
	if n := w.hourlyRows("logs", h10); n != 8 {
		t.Fatalf("hourly logs %d, want 8", n)
	}
	if !w.spooled(a) || !w.spooled(b) {
		t.Fatal("a fold before the final one deleted spool files")
	}

	r := w.fold(h10, afterG)
	if !r.full || !r.final || len(r.deleted) != 2 {
		t.Fatalf("the final fold: %+v", r)
	}
	if w.spooled(a) || w.spooled(b) {
		t.Fatal("the final fold left the hour's files")
	}
	if _, err := os.Stat(filepath.Join(w.sp.root, "raw1", testCluster, h10.Format(dateLayout), "10")); !os.IsNotExist(err) {
		t.Fatalf("the emptied hour's directory is still there: %v", err)
	}
	if !w.spooled(e) {
		t.Fatal("the next hour's file went too")
	}
	if n := w.hourlyRows("logs", h10); n != 8 {
		t.Fatalf("hourly logs after the final fold %d, want 8", n)
	}

	// a late upload to the final hour: folded, then deleted
	late := w.upload(rawKey(h10, "n3", "5959-late"), logs(h10.Add(59*time.Minute), 2, "ns1", "p1", "u1")...)
	if err := w.c.past(w.ctx, h11.Add(2*time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	if w.spooled(late) || w.spooled(e) {
		t.Fatal("the past pass left a final hour's file")
	}
	if f := w.folded(h10); len(f) != 3 || f[late] {
		t.Fatalf("sidecar %v: want the late key, not final", f)
	}
	if n := w.hourlyRows("logs", h10); n != 10 {
		t.Fatalf("hourly logs with the late file %d, want 10", n)
	}
	if entries, err := os.ReadDir(filepath.Join(w.sp.root, "raw1")); err == nil && len(entries) != 0 {
		t.Fatalf("the spool keeps directories: %v", entries)
	}
	// the deletions the past pass's fold jobs counted: the late file and e
	var m strings.Builder
	w.c.m.write(&m)
	for _, want := range []string{"blobs_compactor_spool_files{cluster=\"itest\"} 0\n", "blobs_compactor_spool_bytes{cluster=\"itest\"} 0\n",
		"blobs_compactor_spool_oldest_age_seconds{cluster=\"itest\"} 0\n", "blobs_compactor_raw_deleted_total{cluster=\"itest\"} 2\n",
		`blobs_compactor_put_requests_total{cluster="itest",result="stored"} 4`} {
		if !strings.Contains(m.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, m.String())
		}
	}
}

// Delete removes only what it empties, and leaves the root.
func TestSpoolDeleteKeepsWhatIsNotEmpty(t *testing.T) {
	w := newSpoolWorld(t)
	a := w.upload(rawKey(h10, "n1", "0000-a"), logs(h10, 1, "ns1", "p1", "u1")...)
	b := w.upload(rawKey(h10, "n1", "0001-b"), logs(h10, 1, "ns1", "p1", "u1")...)
	if err := w.sp.Delete(w.ctx, []string{a, "raw1/itest/2026-10-01/10/n9/gone.jsonl.zst"}); err != nil {
		t.Fatal(err)
	}
	if w.spooled(a) || !w.spooled(b) {
		t.Fatal("Delete removed the wrong file")
	}
	if err := w.sp.Delete(w.ctx, []string{b}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(w.sp.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != spoolTmp {
		t.Fatalf("the root holds %v, want only %s", entries, spoolTmp)
	}
	st, err := w.sp.stats()
	if err != nil || st.files != 0 {
		t.Fatalf("stats %+v %v", st, err)
	}
}

// What a crash left in _tmp/ was never acknowledged; a start deletes it.
func TestOpenSpoolClearsInterruptedWrites(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, spoolTmp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, spoolTmp, "x.tmp"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openSpool(root); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, spoolTmp)); len(entries) != 0 {
		t.Fatalf("_tmp holds %v", entries)
	}
}

func TestSpoolConfig(t *testing.T) {
	env := func() map[string]string {
		return bucketEnv(map[string]string{"RAW_SOURCE": "spool", "SPOOL_DIR": "/spool", "PUT_LISTEN": ":9000",
			"PUT_ACCESS_KEY_ID": "pk", "PUT_SECRET_ACCESS_KEY": "ps"})
	}
	c, err := parseConfig(envOf(env()))
	if err != nil {
		t.Fatal(err)
	}
	if c.rawSource != "spool" || !c.rawDelete || c.putMaxBytes != defaultPutMaxBytes || c.putKeyID != "pk" {
		t.Fatalf("%+v", c)
	}
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"RAW_DELETE": "off"}, "RAW_DELETE off"},
		{map[string]string{"SPOOL_DIR": "spool"}, "absolute path"},
		{map[string]string{"RAW_SOURCE": "bucket"}, "only with RAW_SOURCE=spool"},
		{map[string]string{"RAW_SOURCE": "disk"}, "RAW_SOURCE"},
		{map[string]string{"PUT_SECRET_ACCESS_KEY": ""}, "PUT_SECRET_ACCESS_KEY is required"},
		{map[string]string{"PUT_MAX_BYTES": "0"}, "PUT_MAX_BYTES"},
	} {
		kv := env()
		for k, v := range tc.env {
			kv[k] = v
		}
		if _, err := parseConfig(envOf(kv)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err %v, want %q", tc.env, err, tc.want)
		}
	}
}

// putResult PUTs a raw object through the endpoint and returns the
// result it was counted under.
func (w *spoolWorld) putResult(key string, lines ...line) string {
	w.t.Helper()
	before := map[string]float64{}
	for k, v := range w.c.m.puts {
		before[k] = v
	}
	w.upload(key, lines...)
	for k, v := range w.c.m.puts {
		if v != before[k] {
			return k
		}
	}
	w.t.Fatalf("PUT %s: not counted", key)
	return ""
}

// Once a key's final fold has deleted its file, its content cannot be
// compared any more: a PUT of the key, other content or the same, is
// answered 200 and dropped, counted as already_folded, and the hour keeps
// the rows of the first content. A new key in that hour is a late upload,
// stored and folded.
func TestSpoolPutOfAKeyAlreadyFolded(t *testing.T) {
	w := newSpoolWorld(t)
	k := rawKey(h10, "n1", "0000-a")
	w.upload(k, logs(h10, 5, "ns1", "p1", "u1")...)
	if r := w.fold(h10, afterG); !r.final || len(r.deleted) != 1 {
		t.Fatalf("the final fold: %+v", r)
	}
	for _, lines := range [][]line{logs(h10.Add(time.Minute), 7, "ns2", "p2", "u2"), logs(h10, 5, "ns1", "p1", "u1")} {
		if got := w.putResult(k, lines...); got != "already_folded" {
			t.Fatalf("a PUT of the folded key: %s", got)
		}
		if w.spooled(k) {
			t.Fatal("a PUT of the folded key stored it")
		}
	}
	if _, err := os.Stat(filepath.Join(w.sp.root, "raw1")); !os.IsNotExist(err) {
		t.Fatalf("a PUT of the folded key made directories: %v", err)
	}
	late := w.upload(rawKey(h10, "n1", "5959-late"), logs(h10.Add(59*time.Minute), 2, "ns1", "p1", "u1")...)
	if err := w.c.past(w.ctx, h11.Add(2*time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := w.hourlyRows("logs", h10); n != 7 {
		t.Fatalf("hourly logs %d, want the first content's 5 and the late key's 2", n)
	}
	if w.spooled(late) {
		t.Fatal("the late key is still in the spool")
	}
	var m strings.Builder
	w.c.m.write(&m)
	for _, want := range []string{`blobs_compactor_put_requests_total{cluster="itest",result="already_folded"} 2`,
		`blobs_compactor_put_requests_total{cluster="itest",result="stored"} 2`} {
		if !strings.Contains(m.String(), want) {
			t.Errorf("metrics lack %q:\n%s", want, m.String())
		}
	}
}

// The sidecars the folds read and commit are the put endpoint's: a PUT
// reads an hour's sidecar from the tiers only when no fold has had it
// since the start, once.
func TestSpoolPutReadsAHoursSidecarOnce(t *testing.T) {
	w := newSpoolWorld(t)
	k := w.upload(rawKey(h10, "n1", "0000-a"), logs(h10, 5, "ns1", "p1", "u1")...)
	w.fold(h10, afterG)

	// a start: nothing in memory
	reads := map[time.Time]int{}
	restart := func() {
		reads = map[time.Time]int{}
		w.c.foldedKeys = newFoldedKeys(func(ctx context.Context, h time.Time) (map[string]bool, error) {
			reads[h]++
			return w.c.readHourFolded(ctx, h)
		})
		w.put.folded = w.c.foldedKeys
	}
	restart()
	if got := w.putResult(k, logs(h10, 1, "ns2", "p2", "u2")...); got != "already_folded" {
		t.Fatalf("a PUT of the folded key after a start: %s", got)
	}
	w.upload(rawKey(h10, "n1", "0001-b"), logs(h10, 1, "ns1", "p1", "u1")...)
	w.upload(rawKey(h11, "n1", "0000-c"), logs(h11, 1, "ns1", "p1", "u1")...)
	w.upload(rawKey(h11, "n1", "0001-d"), logs(h11, 1, "ns1", "p1", "u1")...)
	if reads[h10] != 1 || reads[h11] != 1 {
		t.Fatalf("sidecar reads %v, want one per hour", reads)
	}
	// a fold of an hour gives the endpoint its sidecar
	restart()
	w.fold(h11, during)
	w.upload(rawKey(h11, "n1", "0002-e"), logs(h11, 1, "ns1", "p1", "u1")...)
	if len(reads) != 0 {
		t.Fatalf("sidecar reads %v after a fold of the hour, want none", reads)
	}
	// a read that fails answers 500, and is not kept
	w.c.foldedKeys = newFoldedKeys(func(context.Context, time.Time) (map[string]bool, error) {
		return nil, errors.New("the bucket is down")
	})
	w.put.folded = w.c.foldedKeys
	body := rawObject(t, logs(h10, 1, "ns1", "p1", "u1")...)
	r := httptest.NewRequest(http.MethodPut, "http://compactor:9000/b/"+rawKey(h10, "n1", "0002-f"), bytes.NewReader(body))
	signV4(r, hashHex(body), "us-east-1", "k", "s", time.Now())
	rec := httptest.NewRecorder()
	w.put.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError || w.spooled(rawKey(h10, "n1", "0002-f")) {
		t.Fatalf("a PUT whose sidecar read fails: %d %s", rec.Code, rec.Body)
	}
}

// foldedKeys keeps a bounded number of hours, the least recently used
// out first, and a read that a set overtook is not kept.
func TestFoldedKeysBoundsAndOvertaking(t *testing.T) {
	ctx := context.Background()
	var setDuring func()
	f := newFoldedKeys(func(_ context.Context, h time.Time) (map[string]bool, error) {
		if setDuring != nil {
			setDuring()
		}
		return map[string]bool{"read": true}, nil
	})
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= foldedKeysHours; i++ {
		f.set(base.Add(time.Duration(i)*time.Hour), map[string]bool{"set": true})
		if i == 0 {
			continue
		}
		// hour 0 used after each set keeps it
		if _, ok := f.lookup(base); !ok {
			t.Fatalf("hour 0 evicted after %d sets", i)
		}
	}
	if len(f.hours) != foldedKeysHours {
		t.Fatalf("%d hours kept, want %d", len(f.hours), foldedKeysHours)
	}
	if _, ok := f.lookup(base.Add(time.Hour)); ok {
		t.Fatal("the least recently used hour was kept")
	}
	// a read of h during which a fold commits h's sidecar: the set's keys stay
	h := base.Add(-time.Hour)
	setDuring = func() { f.set(h, map[string]bool{"newer": true}) }
	if keys, err := f.load(ctx, h); err != nil || !keys["newer"] {
		t.Fatalf("load %v %v, want the set's keys", keys, err)
	}
	// a read during which another hour's set comes: answered, not kept
	h2 := base.Add(-2 * time.Hour)
	setDuring = func() { f.set(base.Add(-3*time.Hour), nil) }
	if keys, err := f.load(ctx, h2); err != nil || !keys["read"] {
		t.Fatalf("load %v %v", keys, err)
	}
	if _, ok := f.lookup(h2); ok {
		t.Fatal("a read that a set may have overtaken was kept")
	}
	setDuring = nil
	if ok, err := f.has(ctx, h2, "read"); err != nil || !ok {
		t.Fatalf("has %v %v", ok, err)
	}
	if _, ok := f.lookup(h2); !ok {
		t.Fatal("a read with no set during it was not kept")
	}
}

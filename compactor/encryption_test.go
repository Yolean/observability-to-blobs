package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tierObjects walks the world's hourly and daily tiers: every parquet
// object's key id by key ("" for plain).
func (w *world) tierObjects() map[string]string {
	w.t.Helper()
	out := map[string]string{}
	for _, p := range w.c.l.tierPrefixes() {
		objs, err := w.c.tiers.List(w.ctx, p)
		if err != nil {
			w.t.Fatal(err)
		}
		for _, o := range objs {
			if strings.HasSuffix(o.Key, ".parquet") {
				out[o.Key] = tierKeyID(o.Key)
			}
		}
	}
	return out
}

// keyOf is the key id of stem's one object.
func (w *world) keyOf(stem string) string { w.t.Helper(); return w.obj(stem).keyID }

// The same raw objects, folded and rolled up with and without encryption,
// give the same rows; every encrypted world's object is under its key, and
// a plain reader cannot read one.
func TestEncryptedTiersHoldThePlainRows(t *testing.T) {
	plain, enc := newWorld(t), newKeyedWorld(t, "k1")
	for _, w := range []*world{plain, enc} {
		w.putA()
		w.fold(h10, during)
		w.putB()
		w.putBRetry()
		w.putC()
		w.fold(h10, afterG)
		w.putE()
		w.fold(h11, h11.Add(20*time.Minute))
		if r := w.rollup(day); len(r.hours) != 2 {
			t.Fatalf("rollup %+v", r)
		}
	}
	for _, tbl := range []string{"logs", "metrics", "podindex", "pods"} {
		for _, h := range []time.Time{h10, h11} {
			if plain.hourlyRows(tbl, h) == 0 {
				continue // hour 11 has no scrapes
			}
			sameRows(t, enc.c.db, "hourly "+tbl+" "+h.Format("15"), plain.from(plain.c.l.hourlyStem(tbl, h)), enc.from(enc.c.l.hourlyStem(tbl, h)))
		}
		sameRows(t, enc.c.db, "daily "+tbl, plain.dailySource(tbl, day), enc.dailySource(tbl, day))
	}
	for k, id := range plain.tierObjects() {
		if id != "" {
			t.Fatalf("the plain world wrote %s", k)
		}
	}
	objs := enc.tierObjects()
	if len(objs) != len(plain.tierObjects()) {
		t.Fatalf("%d encrypted objects, %d plain ones", len(objs), len(plain.tierObjects()))
	}
	for k, id := range objs {
		if id != "k1" {
			t.Fatalf("%s: key %q, want k1", k, id)
		}
		b, err := os.ReadFile(filepath.Join(enc.dir, k))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "p1 line") || strings.Contains(string(b), "ns1") {
			t.Fatalf("%s holds plaintext", k)
		}
	}
	logs := enc.obj(enc.c.l.hourlyStem("logs", h10)).key
	if _, err := enc.c.db.ExecContext(enc.ctx, "SELECT * FROM read_parquet("+enc.path(logs)+")"); err == nil ||
		!strings.Contains(err.Error(), "is encrypted") {
		t.Fatalf("a plain read of %s: %v", logs, err)
	}
	// the sidecars stay plain
	if n := enc.q("SELECT count(*) FROM read_csv(" + enc.path(enc.c.l.hourlySidecar(h10)) + ", header = true)"); n != 4 {
		t.Fatalf("sidecar rows %d", n)
	}
}

// A rotation encrypts new hours and new namespace partitions with the new
// key, and rewrites every existing object under the key it was first
// written with; readers see every row.
func TestRotationKeepsEachObjectUnderItsFirstKey(t *testing.T) {
	w := newKeyedWorld(t, "k1")
	keys := []string{w.putA()}
	w.fold(h10, during)

	w.encrypt("k2", "k1")
	keys = append(keys, w.putB(), w.putC())
	w.fold(h10, afterG) // the full read, rewriting hour 10
	keys = append(keys, w.putE())
	w.fold(h11, h11.Add(20*time.Minute))
	for _, tbl := range []string{"logs", "metrics", "podindex", "pods"} {
		if id := w.keyOf(w.c.l.hourlyStem(tbl, h10)); id != "k1" {
			t.Fatalf("hour 10 %s under %q, want its first key k1", tbl, id)
		}
		if id := w.keyOf(w.c.l.hourlyStem(tbl, h11)); id != "k2" {
			t.Fatalf("hour 11 %s under %q, want the new key k2", tbl, id)
		}
	}
	w.rollup(day)
	w.wholeDay(t, keys)

	// another rotation: a late key of hour 11 for ns2 and a new namespace
	w.encrypt("k3", "k1", "k2")
	keys = append(keys, w.putF(), w.put(rawKey(h11, "n2", "0300-h"), logs(h11.Add(3*time.Minute), 2, "ns3", "r1", "u3")...))
	w.fold(h11, h11.Add(30*time.Minute))
	if id := w.keyOf(w.c.l.hourlyStem("logs", h11)); id != "k2" {
		t.Fatalf("hour 11 logs under %q after the second rotation, want k2", id)
	}
	if r := w.rollup(day); r.namespaces != 3 {
		t.Fatalf("rollup %+v, want ns1, ns2, ns3", r)
	}
	for stem, want := range map[string]string{
		w.c.l.dailyPartitionStem("logs", day, "ns2"): "k2",
		w.c.l.dailyPartitionStem("logs", day, "ns3"): "k3",
		w.c.l.dailyStem("pods", day):                 "k2",
		w.c.l.dailyStem("podindex", day):             "k2",
	} {
		if id := w.keyOf(stem); id != want {
			t.Fatalf("%s under %q, want %s", stem, id, want)
		}
	}
	w.wholeDay(t, keys)
	if n := w.q("SELECT count(*) FROM " + w.dailySource("logs", day) + " WHERE namespace = 'ns3'"); n != 2 {
		t.Fatalf("ns3 logs %d", n)
	}
}

// Turning encryption on for a bucket with plain objects: they stay plain,
// new ones are encrypted, and the readers' union of both has every row.
func TestTurningEncryptionOnKeepsPlainObjectsPlain(t *testing.T) {
	w := newWorld(t)
	keys := []string{w.putA(), w.putC()}
	w.fold(h10, afterG)
	w.rollup(day)

	w.encrypt("k1")
	keys = append(keys, w.putB())
	w.fold(h10, afterG.Add(time.Hour)) // a late key of a final hour
	keys = append(keys, w.putE(), w.put(rawKey(h11, "n2", "0300-h"), logs(h11.Add(3*time.Minute), 2, "ns3", "r1", "u3")...))
	w.fold(h11, h11.Add(20*time.Minute))
	if id := w.keyOf(w.c.l.hourlyStem("logs", h10)); id != "" {
		t.Fatalf("hour 10 logs under %q, want plain still", id)
	}
	if id := w.keyOf(w.c.l.hourlyStem("logs", h11)); id != "k1" {
		t.Fatalf("hour 11 logs under %q, want k1", id)
	}
	w.rollup(day)
	for stem, want := range map[string]string{
		w.c.l.dailyPartitionStem("logs", day, "ns1"): "",
		w.c.l.dailyPartitionStem("logs", day, "ns3"): "k1",
		w.c.l.dailyStem("pods", day):                 "",
	} {
		if id := w.keyOf(stem); id != want {
			t.Fatalf("%s under %q, want %q", stem, id, want)
		}
	}
	w.wholeDay(t, keys)

	// the hourly tier read as a reader would: the plain objects in one
	// read, each key's in another, united by name
	var objs []tierObject
	for _, h := range []time.Time{h10, h11} {
		objs = append(objs, w.obj(w.c.l.hourlyStem("logs", h)))
	}
	if n := w.q("SELECT count(*) FROM (" + w.c.readTier(objs, "") + ")"); n != h10Logs+5+2 {
		t.Fatalf("hourly logs %d, want %d", n, h10Logs+5+2)
	}
}

// A key whose objects remain must stay: without its file the compactor
// refuses to fold or roll up, says so in /metrics, and resumes once the
// file is back.
func TestMissingKeyRefuses(t *testing.T) {
	w := newKeyedWorld(t, "k1")
	w.putA()
	w.fold(h10, during)
	w.encrypt("k2", "k1")
	if err := os.Remove(filepath.Join(w.keysDir, "k1")); err != nil {
		t.Fatal(err)
	}
	b := w.putB()
	var missing errKeysMissing
	if err := w.c.past(w.ctx, during.Add(time.Hour), time.Minute); !errors.As(err, &missing) || strings.Join(missing.ids, ",") != "k1" {
		t.Fatalf("past pass: %v, want the refusal naming k1", err)
	}
	if err := w.c.rollingPass(w.ctx, during, time.Minute); !errors.As(err, &missing) {
		t.Fatalf("rolling pass: %v, want the refusal", err)
	}
	if _, ok := w.folded(h10)[b]; ok {
		t.Fatal("folded while a key is missing")
	}
	var m strings.Builder
	w.c.m.write(&m)
	for _, want := range []string{"blobs_compactor_parquet_keys_missing 1\n", `blobs_compactor_runs_total{job="rolling",result="failure"} 1`} {
		if !strings.Contains(m.String(), want) {
			t.Fatalf("metrics lack %q:\n%s", want, m.String())
		}
	}

	// the key file is back: the next rolling pass checks again and folds
	w.encrypt("k2", "k1")
	if err := w.c.rollingPass(w.ctx, during, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.folded(h10)[b]; !ok {
		t.Fatal("not folded once the key was back")
	}
	m.Reset()
	w.c.m.write(&m)
	if !strings.Contains(m.String(), "blobs_compactor_parquet_keys_missing 0\n") {
		t.Fatalf("metrics:\n%s", m.String())
	}

	// a compactor without keys over encrypted objects refuses too, rather
	// than write a plain object beside an encrypted one
	w.c.keysDir, w.c.keyID, w.c.keys = "", "", nil
	if err := w.c.checkKeys(w.ctx); !errors.As(err, &missing) || strings.Join(missing.ids, ",") != "k1" {
		t.Fatalf("without keys: %v", err)
	}
}

func TestStartRequiresTheActiveKeyFile(t *testing.T) {
	w := newWorld(t)
	w.c.keysDir, w.c.keyID = w.keysDir, "k9"
	if err := w.c.init(w.ctx, t.TempDir()); err == nil || !strings.Contains(err.Error(), "has no key file k9") {
		t.Fatalf("init: %v", err)
	}
	// and with the file, init writes and reads an encrypted object
	w.encrypt("k9")
	if err := w.c.init(w.ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestLoadParquetKeys(t *testing.T) {
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// a Secret's mount: the keys are links into a dot directory
	dir := t.TempDir()
	write(dir, "..2026_10_03/k-1", testKey("k-1"))
	if err := os.Symlink("..2026_10_03", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/k-1", filepath.Join(dir, "k-1")); err != nil {
		t.Fatal(err)
	}
	keys, err := loadParquetKeys(dir)
	if err != nil || len(keys) != 1 || keys["k-1"] != strings.TrimSpace(testKey("k-1")) {
		t.Fatalf("%v %v", keys, err)
	}
	secret := "c2hvcnQ=" // 5 bytes
	for _, tc := range []struct{ name, content, want string }{
		{"Key1", testKey("x"), "is not a key id"},
		{"k1", secret, "want the standard base64 of 32 bytes"},
		{"k1", "MDEyMzQ1Njc4OWFiY2RlZg==", "want the standard base64 of 32 bytes"}, // 16 bytes
		{"k1", strings.Repeat("x", 32), "want the standard base64 of 32 bytes"},
	} {
		dir := t.TempDir()
		write(dir, tc.name, tc.content)
		_, err := loadParquetKeys(dir)
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), tc.content) {
			t.Errorf("%s: err %v, want %q and not the content", tc.name, err, tc.want)
		}
	}
	if _, err := loadParquetKeys(filepath.Join(t.TempDir(), "none")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing directory: %v", err)
	}
}

func TestEncryptedRoundtrip(t *testing.T) {
	db, err := open("", "", initStmt{sql: "SET force_mbedtls_unsafe = true"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	dir := t.TempDir()
	if _, err := roundtrip(context.Background(), db, dir); err != nil {
		t.Fatal(err)
	}
	if err := encryptedRoundtrip(context.Background(), db, dir); err != nil {
		t.Fatal(err)
	}
}

// A key file whose content changed under the same id reads nothing that
// was written with it before: the check decrypts one stored object per id
// and refuses, rather than fail every fold of those hours later.
func TestChangedKeyContentRefuses(t *testing.T) {
	w := newKeyedWorld(t, "k1")
	w.putA()
	w.fold(h10, during)
	if err := os.WriteFile(filepath.Join(w.keysDir, "k1"), []byte(testKey("k1-other")), 0o600); err != nil {
		t.Fatal(err)
	}
	var refused errKeysMissing
	if err := w.c.checkKeys(w.ctx); !errors.As(err, &refused) || strings.Join(refused.wrong, ",") != "k1" || len(refused.ids) != 0 {
		t.Fatalf("checkKeys: %v, want the refusal naming k1 as wrong", err)
	}
	if err := w.c.rollingPass(w.ctx, during, time.Minute); !errors.As(err, &refused) {
		t.Fatalf("rolling pass: %v, want the refusal", err)
	}
	// the original content back: the check passes and folds resume
	w.encrypt("k1")
	if err := w.c.rollingPass(w.ctx, during, time.Minute); err != nil {
		t.Fatal(err)
	}
}

// With the keys directory but no active key, new objects are plain while
// the encrypted ones are still rewritten under their keys.
func TestKeysWithoutActiveKeyStillRewriteEncrypted(t *testing.T) {
	w := newKeyedWorld(t, "k1")
	w.putA()
	w.fold(h10, during)
	w.c.keyID = ""
	if err := w.c.init(w.ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	w.putB()
	w.fold(h10, during.Add(time.Minute))
	if id := w.keyOf(w.c.l.hourlyStem("logs", h10)); id != "k1" {
		t.Fatalf("hour 10 logs under %q, want k1 still", id)
	}
}

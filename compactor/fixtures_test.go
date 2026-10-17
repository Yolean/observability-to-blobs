package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A bucket in a temp directory, a compactor over it, and raw objects the
// tests write the way the agent does: JSON lines, zstd.

const testCluster = "itest"

type world struct {
	t       *testing.T
	ctx     context.Context
	dir     string
	keysDir string
	c       *compactor
}

// newWorld is a compactor without parquet keys. Its engine has no httpfs,
// so its crypto is read-only unless forced: force_mbedtls_unsafe lets the
// tests write encrypted objects, which the compactor itself never sets
// (it loads httpfs whenever PARQUET_KEY_ID is set, and selftest writes
// encrypted Parquet with the image's httpfs).
func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	db, err := open("", "", initStmt{sql: "SET force_mbedtls_unsafe = true"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	d := dirBucket{root: dir}
	c := &compactor{l: layout{testCluster}, raw: d, tiers: d, db: db, grace: 10 * time.Minute, threads: 2,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), m: newMetrics(testCluster)}
	if err := c.init(ctx, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &world{t: t, ctx: ctx, dir: dir, keysDir: t.TempDir(), c: c}
}

// newKeyedWorld is a compactor that encrypts new objects with active.
func newKeyedWorld(t *testing.T, active string) *world {
	w := newWorld(t)
	w.encrypt(active)
	return w
}

// testKey is a key file's content, the same for an id in every world.
func testKey(id string) string {
	b := []byte(strings.Repeat(id+"/", parquetKeyBytes))[:parquetKeyBytes]
	return base64.StdEncoding.EncodeToString(b) + "\n"
}

// encrypt makes active the key of new objects, writing its key file and
// those of more ids, as a rotation adds a key to the Secret.
func (w *world) encrypt(active string, more ...string) {
	w.t.Helper()
	for _, id := range append(more, active) {
		if err := os.WriteFile(filepath.Join(w.keysDir, id), []byte(testKey(id)), 0o600); err != nil {
			w.t.Fatal(err)
		}
	}
	w.c.keysDir, w.c.keyID = w.keysDir, active
	if err := w.c.checkKeys(w.ctx); err != nil {
		w.t.Fatal(err)
	}
}

// line is one raw line; the helpers below build the three tables' shapes.
type line map[string]any

func ts(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05.000000000") }

func logLine(t time.Time, ns, pod, uid, container string, restart int, msg string) line {
	return line{"table": "logs", "time": ts(t), "ts": ts(t.Truncate(time.Microsecond)), "cluster": testCluster,
		"node": "n1", "namespace": ns, "pod": pod, "pod_uid": uid, "container": container, "restart": restart,
		"stream": "stdout", "message": msg}
}

func journalLine(t time.Time, msg string) line {
	return line{"table": "logs", "ts": ts(t), "cluster": testCluster, "node": "n1", "stream": "journal",
		"unit": "k3s.service", "priority": 6, "message": msg}
}

func scrapeLine(t time.Time, ns, pod, uid, container string, restart int, name string, value float64) line {
	return line{"table": "metrics", "time": ts(t), "ts": ts(t), "cluster": testCluster, "node": "n1", "job": "pod",
		"instance": "10.0.0.1:9090", "namespace": ns, "pod": pod, "pod_uid": uid, "container": container,
		"restart": restart, "samples": []any{
			map[string]any{"name": name, "type": "gauge", "labels": map[string]string{"code": "200"}, "value": value},
			map[string]any{"name": "up", "type": "gauge", "labels": map[string]string{}, "value": 1.0},
		}}
}

func podLine(kind string, t time.Time, ns, pod, uid string) line {
	return line{"table": "podindex", "time": ts(t), "ts": ts(t), "cluster": testCluster, "node": "n1", "kind": kind,
		"namespace": ns, "pod": pod, "pod_uid": uid, "labels": map[string]string{"app": pod},
		"pod_started": ts(t.Truncate(time.Hour)), "qos_class": "besteffort", "pod_ip": "10.0.0.1"}
}

func containerLine(t time.Time, ns, pod, uid, container string, restart int) line {
	return line{"table": "podindex", "time": ts(t), "ts": ts(t), "cluster": testCluster, "node": "n1",
		"kind": "container", "namespace": ns, "pod": pod, "pod_uid": uid, "container": container, "restart": restart,
		"container_id": strings.Repeat("a", 64), "image": "busybox", "image_id": "busybox@sha256:00",
		"container_started": ts(t)}
}

func exitLine(t time.Time, ns, pod, uid, container string, restart, code int) line {
	return line{"table": "podindex", "time": ts(t), "ts": ts(t), "cluster": testCluster, "node": "n1",
		"kind": "container-exit", "namespace": ns, "pod": pod, "pod_uid": uid, "container": container,
		"restart": restart, "container_id": strings.Repeat("a", 64), "exit_code": code, "exit_reason": "Completed"}
}

// objectLine is a row of the cluster agent's apiserver table; a snapshot
// row when name is "".
func objectLine(kind string, t time.Time, name, rv string) line {
	l := line{"table": "apiserver", "time": ts(t), "ts": ts(t), "cluster": testCluster, "kind": kind,
		"api_version": "v1", "resource": "configmaps"}
	if name == "" {
		l["rows"] = 1
		return l
	}
	l["namespace"], l["name"], l["object_uid"], l["resource_version"] = "ns1", name, "uid-"+name, rv
	l["apijson"] = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"` + name + `","namespace":"ns1"}}`
	return l
}

// logs returns n log lines of one container from start, a second apart.
func logs(start time.Time, n int, ns, pod, uid string) []line {
	var out []line
	for i := 0; i < n; i++ {
		t := start.Add(time.Duration(i) * time.Second)
		out = append(out, logLine(t, ns, pod, uid, "app", 0, fmt.Sprintf("%s line %s", pod, t.Format("15:04:05"))))
	}
	return out
}

// rawKey is the agent's key for an object of hour h.
func rawKey(h time.Time, node, name string) string {
	return fmt.Sprintf("raw1/%s/%s/%s/%s/%s.jsonl.zst", testCluster, h.Format(dateLayout), h.Format("15"), node, name)
}

// put writes a raw object and returns its key.
func (w *world) put(key string, lines ...line) string {
	w.t.Helper()
	p := filepath.Join(w.dir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		w.t.Fatal(err)
	}
	if err := os.WriteFile(p, rawObject(w.t, lines...), 0o644); err != nil {
		w.t.Fatal(err)
	}
	return key
}

// rawObject is a raw object's content: JSON lines, zstd.
func rawObject(t *testing.T, lines ...line) []byte {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		j, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	return zstdFrame([]byte(b.String()))
}

// zstdFrame wraps data in a zstd frame of raw (stored) blocks: valid zstd
// for the engine's decoder, without a compressor in the module graph.
func zstdFrame(data []byte) []byte {
	// magic, a frame header with no content size and a 128 KiB window
	out := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x38}
	for {
		n := min(len(data), 128<<10)
		last := 0
		if n == len(data) {
			last = 1
		}
		h := n<<3 | last // block type 0: raw
		out = append(out, byte(h), byte(h>>8), byte(h>>16))
		out = append(out, data[:n]...)
		data = data[n:]
		if last == 1 {
			return out
		}
	}
}

func (w *world) exists(key string) bool {
	_, err := os.Stat(filepath.Join(w.dir, filepath.FromSlash(key)))
	return err == nil
}

func (w *world) remove(keys ...string) {
	w.t.Helper()
	for _, k := range keys {
		if err := os.Remove(filepath.Join(w.dir, filepath.FromSlash(k))); err != nil {
			w.t.Fatal(err)
		}
	}
}

func (w *world) fold(h, now time.Time) foldResult {
	w.t.Helper()
	r, err := w.c.fold(w.ctx, h, nil, now)
	if err != nil {
		w.t.Fatalf("fold %s at %s: %v", h, now, err)
	}
	return r
}

func (w *world) rollup(day time.Time) rollupResult {
	w.t.Helper()
	r, err := w.c.rollup(w.ctx, day)
	if err != nil {
		w.t.Fatalf("rollup %s: %v", day, err)
	}
	return r
}

// folded reads an hourly sidecar.
func (w *world) folded(h time.Time) map[string]bool {
	w.t.Helper()
	conn, err := w.c.conn(w.ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	defer conn.Close()
	f, err := w.c.readFolded(w.ctx, conn, w.c.l.hourlySidecar(h))
	if err != nil {
		w.t.Fatal(err)
	}
	return f
}

// q runs a query returning one integer, on a fresh connection.
func (w *world) q(query string) int64 {
	w.t.Helper()
	var n int64
	if err := w.c.db.QueryRowContext(w.ctx, query).Scan(&n); err != nil {
		w.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func (w *world) path(key string) string {
	return sqlString(filepath.Join(w.dir, filepath.FromSlash(key)))
}

// obj is stem's one object, plain or encrypted.
func (w *world) obj(stem string) tierObject {
	w.t.Helper()
	var found []tierObject
	objs, err := w.c.tiers.List(w.ctx, stem+".")
	if err != nil {
		w.t.Fatal(err)
	}
	for _, o := range objs {
		if t, ok := stemObject(stem, o.Key); ok {
			found = append(found, t)
		}
	}
	if len(found) != 1 {
		w.t.Fatalf("%s: objects %v, want one", stem, found)
	}
	return found[0]
}

// from is a FROM clause over stem's object, read with its key.
func (w *world) from(stem string) string {
	return "(" + w.c.readTier([]tierObject{w.obj(stem)}, "") + ")"
}

// hourlyRows counts the rows of an hourly object.
func (w *world) hourlyRows(table string, h time.Time) int64 {
	return w.q("SELECT count(*) FROM " + w.from(w.c.l.hourlyStem(table, h)))
}

// dailySource is a FROM clause over a daily table, each object read with
// its key.
func (w *world) dailySource(table string, day time.Time) string {
	w.t.Helper()
	if table == "logs" || table == "metrics" {
		parts, err := w.c.partitions(w.ctx, table, day)
		if err != nil {
			w.t.Fatal(err)
		}
		var objs []tierObject
		for _, ns := range sortedIDs(parts) {
			objs = append(objs, parts[ns])
		}
		return "(" + w.c.readTier(objs, "hive_partitioning = false, ") + ")"
	}
	return w.from(w.c.l.dailyStem(table, day))
}

// sameRows fails unless two FROM clauses hold the same rows, duplicates
// counted; the columns must come in the same order.
func sameRows(t *testing.T, db *sql.DB, what, a, b string) {
	t.Helper()
	var na, nb, aNotB, bNotA int64
	q := fmt.Sprintf(`SELECT (SELECT count(*) FROM %[1]s), (SELECT count(*) FROM %[2]s),
	  (SELECT count(*) FROM (SELECT * FROM %[1]s EXCEPT ALL SELECT * FROM %[2]s)),
	  (SELECT count(*) FROM (SELECT * FROM %[2]s EXCEPT ALL SELECT * FROM %[1]s))`, a, b)
	if err := db.QueryRowContext(context.Background(), q).Scan(&na, &nb, &aNotB, &bNotA); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if na != nb || aNotB != 0 || bNotA != 0 {
		t.Fatalf("%s: %d rows vs %d, %d only in the first, %d only in the second", what, na, nb, aNotB, bNotA)
	}
	if na == 0 {
		t.Fatalf("%s: no rows to compare", what)
	}
}

var errCrash = errors.New("crash before the sidecar")

package main

import (
	"context"
	"testing"
)

// Jobs take a fresh connection each (compactor.init makes them single-use).
// After one has spilled to the temp directory, the next connection must
// still open: DuckDB refuses to set temp_directory once it has been used.
func TestTempDirectoryAfterSpill(t *testing.T) {
	ctx := context.Background()
	c := config{dir: t.TempDir(), tempDir: t.TempDir(), memoryLimit: 64 << 20}
	httpfs, init := c.engineInit()
	db, err := open(httpfs, c.tempDir, append(init, initStmt{sql: "SET threads = 2"})...)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxIdleConns(0)
	spill := "SELECT count(*) FROM (SELECT i, md5(i::VARCHAR) m FROM range(6000000) t(i) ORDER BY m)"
	for job := 0; job < 2; job++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("job %d: connection: %v", job, err)
		}
		var dir string
		if err := conn.QueryRowContext(ctx, "SELECT current_setting('temp_directory')").Scan(&dir); err != nil || dir != c.tempDir {
			t.Fatalf("job %d: temp_directory %q, %v; want %q", job, dir, err, c.tempDir)
		}
		var n int64
		if err := conn.QueryRowContext(ctx, spill).Scan(&n); err != nil {
			t.Fatalf("job %d: %v", job, err)
		}
		conn.Close()
	}
}

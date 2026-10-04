package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// selftest proves, inside the image on a read-only root with a tmpfs /tmp,
// what the pin check cannot: the engine starts on the base image, loads
// the baked httpfs with autoinstall off, and reads and writes the formats
// a fold uses.
func selftest(ctx context.Context, log *slog.Logger) error {
	if err := version(ctx, log); err != nil {
		return err
	}
	db, err := open(httpfsPath())
	if err != nil {
		return err
	}
	defer db.Close()
	var loaded bool
	if err := db.QueryRowContext(ctx,
		"SELECT loaded FROM duckdb_extensions() WHERE extension_name = 'httpfs'").Scan(&loaded); err != nil {
		return err
	}
	if !loaded {
		return fmt.Errorf("httpfs is not loaded")
	}
	dir, err := os.MkdirTemp("", "selftest")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	n, err := roundtrip(ctx, db, dir)
	if err != nil {
		return err
	}
	if err := encryptedRoundtrip(ctx, db, dir); err != nil {
		return err
	}
	log.Info("selftest", "httpfs", httpfsPath(), "rows", n, "encrypted", true)
	return nil
}

// encryptedRoundtrip writes the roundtrip's parquet file again, encrypted
// with a random key, and reads it back with the key. The engine writes
// encrypted Parquet only with httpfs's crypto loaded, so this is the check
// that the image's extension provides it.
func encryptedRoundtrip(ctx context.Context, db *sql.DB, dir string) error {
	key := make([]byte, parquetKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	in := sqlString(filepath.Join(dir, "logs.parquet"))
	out := sqlString(filepath.Join(dir, "logs.k-selftest.parquet"))
	cfg := encryptionConfig("selftest")
	if _, err := db.ExecContext(ctx, "PRAGMA add_parquet_key('selftest', "+sqlString(base64.StdEncoding.EncodeToString(key))+")"); err != nil {
		return fmt.Errorf("PRAGMA add_parquet_key: %w", err)
	}
	if _, err := db.ExecContext(ctx, "COPY (FROM read_parquet("+in+")) TO "+out+" (FORMAT parquet, ENCRYPTION_CONFIG "+cfg+")"); err != nil {
		return err
	}
	var same bool
	err := db.QueryRowContext(ctx, "SELECT count(*) = 500 AND count(*) = (SELECT count(*) FROM (FROM read_parquet("+in+
		") INTERSECT FROM read_parquet("+out+", encryption_config = "+cfg+"))) FROM read_parquet("+out+", encryption_config = "+cfg+")").Scan(&same)
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("the encrypted copy's rows differ from the plain file's")
	}
	return nil
}

// roundtrip writes JSON lines with zstd, reads them back with pinned
// columns as a fold does, deduplicates, sorts and writes parquet, and
// returns the rows read back from the parquet file.
func roundtrip(ctx context.Context, db *sql.DB, dir string) (int, error) {
	raw := sqlString(filepath.Join(dir, "raw.jsonl.zst"))
	out := sqlString(filepath.Join(dir, "logs.parquet"))
	stmts := []string{
		`COPY (
		   SELECT 'logs' AS "table",
		          TIMESTAMP_NS '2026-09-25 10:00:00.123456789' + to_seconds(i % 500) AS time,
		          'ns' || (i % 500 % 3) AS namespace, 'line ' || (i % 500) AS message
		   FROM range(1000) t(i)
		 ) TO ` + raw + ` (FORMAT json, COMPRESSION zstd)`,
		`COPY (
		   SELECT DISTINCT time, namespace, message
		   FROM read_json(` + raw + `, format = 'newline_delimited', compression = 'zstd',
		     columns = {"table": 'VARCHAR', time: 'TIMESTAMP_NS', namespace: 'VARCHAR', message: 'VARCHAR'})
		   ORDER BY namespace, time
		 ) TO ` + out + ` (FORMAT parquet, COMPRESSION zstd, ROW_GROUP_SIZE 20000)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return 0, err
		}
	}
	var n int
	err := db.QueryRowContext(ctx, "SELECT count(*) FROM read_parquet("+out+")").Scan(&n)
	return n, err
}

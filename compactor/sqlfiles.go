package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"
)

// The SQL the itest and local/blobs.sh read too: raw.sql is the raw line
// contract, tables.sql the rows each table gets, pods.sql the index.
//
//go:embed sql/raw.sql sql/tables.sql sql/pods.sql
var sqlFiles embed.FS

func sqlFile(name string) string {
	b, err := sqlFiles.ReadFile("sql/" + name)
	if err != nil {
		panic(err) // embedded at build time
	}
	return string(b)
}

// setRawFiles points the raw view at files; the view reads the variable
// when queried (compactor/sql/raw.sql). Session variables and temp tables
// belong to a connection, so a fold runs on one *sql.Conn throughout.
func setRawFiles(ctx context.Context, c *sql.Conn, files []string) error {
	quoted := make([]string, len(files))
	for i, f := range files {
		quoted[i] = sqlString(f)
	}
	_, err := c.ExecContext(ctx, "SET VARIABLE raw_files = ["+strings.Join(quoted, ", ")+"]")
	return err
}

// loadSQL creates the raw, table and pods views on c; raw_files must be set
// first, because creating the view binds read_json once.
func loadSQL(ctx context.Context, c *sql.Conn) error {
	for _, f := range []string{"raw.sql", "tables.sql", "pods.sql"} {
		if _, err := c.ExecContext(ctx, sqlFile(f)); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
	}
	return nil
}

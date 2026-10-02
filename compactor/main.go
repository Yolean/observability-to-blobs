// blobs-compactor folds the raw1 tier into the hourly and daily tiers
// (NEXT.md section 4). How it is built and run: compactor/README.md.
package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"

	"github.com/duckdb/duckdb-go/v2"
)

// The image carries the extension at this arch-independent path; the
// embedded engine never installs one at runtime.
const defaultHTTPFS = "/usr/local/lib/duckdb/httpfs.duckdb_extension"

const usage = `usage: blobs-compactor <command>

  selftest   load httpfs, round-trip JSON lines, zstd and parquet through the engine
  version    print the engine and module versions`

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "selftest":
		err = selftest(ctx, log)
	case "version":
		err = version(ctx, log)
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Error(os.Args[1], "err", err)
		os.Exit(1)
	}
}

// open returns an in-memory engine whose every connection has autoinstall
// off and, when httpfs is non-empty, the extension loaded from that path.
// Settings are per connection, so they go in the connector's init.
func open(httpfs string) (*sql.DB, error) {
	c, err := duckdb.NewConnector("", func(e driver.ExecerContext) error {
		stmts := []string{
			"SET autoinstall_known_extensions = false",
			"SET autoload_known_extensions = false",
		}
		if httpfs != "" {
			stmts = append(stmts, "LOAD "+sqlString(httpfs))
		}
		for _, s := range stmts {
			if _, err := e.ExecContext(context.Background(), s, nil); err != nil {
				return fmt.Errorf("%s: %w", s, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(c), nil
}

func httpfsPath() string {
	if p := os.Getenv("HTTPFS_EXTENSION"); p != "" {
		return p
	}
	return defaultHTTPFS
}

// sqlString quotes s as a SQL string literal.
func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func version(ctx context.Context, log *slog.Logger) error {
	db, err := open("")
	if err != nil {
		return err
	}
	defer db.Close()
	var v string
	if err := db.QueryRowContext(ctx, "SELECT version()").Scan(&v); err != nil {
		return err
	}
	attrs := []any{"duckdb", v}
	if bi, ok := debug.ReadBuildInfo(); ok {
		attrs = append(attrs, "go", bi.GoVersion)
		for _, m := range bi.Deps {
			if m.Path == "github.com/duckdb/duckdb-go/v2" {
				attrs = append(attrs, "duckdb-go", m.Version)
			}
		}
	}
	log.Info("version", attrs...)
	return nil
}

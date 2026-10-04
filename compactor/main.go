// blobs-compactor folds the raw1 tier into the hourly and daily tiers.
// What it computes, and how it is built and run: compactor/README.md.
package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/duckdb/duckdb-go/v2"
)

// The image carries the extension at this arch-independent path; the
// embedded engine never installs one at runtime.
const defaultHTTPFS = "/usr/local/lib/duckdb/httpfs.duckdb_extension"

const usage = `usage: blobs-compactor <command>

  run               fold and roll up on the loop's cadence (ROLLING_INTERVAL, GRACE, PAST_INTERVAL)
  fold <date> <HH>  fold one hour of raw1 into the hourly tier, as the loop would now
  rollup <date>     roll one day's changed hours up into the daily tier
  selftest          load httpfs, round-trip JSON lines, zstd and parquet through the engine
  version           print the engine and module versions

fold, rollup and run take the cluster and the bucket from the environment
(compactor/README.md); date is YYYY-MM-DD, hours are UTC. Run fold and
rollup by hand only while no loop runs: the tiers want a single writer.`

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	args := os.Args[1:]
	nargs := map[string]int{"run": 0, "fold": 2, "rollup": 1, "selftest": 0, "version": 0}
	if n, ok := nargs[first(args)]; !ok || len(args) != n+1 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	var err error
	switch args[0] {
	case "selftest":
		err = selftest(ctx, log)
	case "version":
		err = version(ctx, log)
	default:
		err = command(ctx, log, args)
	}
	if err != nil {
		log.Error(args[0], "err", err)
		os.Exit(1)
	}
}

func first(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// command runs run, fold or rollup with the environment's config.
func command(ctx context.Context, log *slog.Logger, args []string) error {
	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	if err := version(ctx, log); err != nil {
		return err
	}
	c, err := newCompactor(cfg, log)
	if err != nil {
		return err
	}
	defer c.Close()
	switch args[0] {
	case "run":
		return c.run(ctx, cfg)
	case "fold":
		h, err := time.Parse(dateLayout+"/15", args[1]+"/"+args[2])
		if err != nil {
			return fmt.Errorf("fold %s %s: want YYYY-MM-DD HH", args[1], args[2])
		}
		r, err := c.foldJob(ctx, h, nil, time.Now().UTC(), cfg.jobTimeout)
		if err == nil && !r.wrote && len(r.deleted) == 0 {
			log.Info("fold", "hour", h.Format(dateLayout+"T15"), "keys", 0, "final", r.final)
		}
		return err
	default:
		d, err := time.Parse(dateLayout, args[1])
		if err != nil {
			return fmt.Errorf("rollup %s: want YYYY-MM-DD", args[1])
		}
		r, err := c.rollupJob(ctx, d, cfg.jobTimeout)
		if err == nil && len(r.hours) == 0 {
			log.Info("rollup", "day", args[1], "hours", 0)
		}
		return err
	}
}

// initStmt is a statement every connection runs first. label, when set,
// stands in for the statement in errors, and the redact strings are blanked
// out of the engine's message, so a secret is never logged.
type initStmt struct {
	label, sql string
	redact     []string
}

// open returns an in-memory engine whose every connection has autoinstall
// off, when httpfs is non-empty the extension loaded from that path, and
// then runs init. Settings are per connection, so they go in the
// connector's init.
func open(httpfs string, init ...initStmt) (*sql.DB, error) {
	c, err := duckdb.NewConnector("", func(e driver.ExecerContext) error {
		stmts := []initStmt{
			{sql: "SET autoinstall_known_extensions = false"},
			{sql: "SET autoload_known_extensions = false"},
		}
		if httpfs != "" {
			stmts = append(stmts, initStmt{sql: "LOAD " + sqlString(httpfs)})
		}
		for _, s := range append(stmts, init...) {
			if _, err := e.ExecContext(context.Background(), s.sql, nil); err != nil {
				label, msg := s.label, err.Error()
				if label == "" {
					label = s.sql
				}
				for _, r := range s.redact {
					if r == "" {
						continue
					}
					msg = strings.ReplaceAll(msg, r, "<redacted>")
				}
				return fmt.Errorf("%s: %s", label, msg)
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

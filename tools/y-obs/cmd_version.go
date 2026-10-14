package main

import (
	"context"
	"fmt"
	"runtime"
)

// version is set by release.sh (-X main.version=...).
var version = "dev"

func cmdVersion(ctx context.Context, args []string) error {
	fs := newFlagSet("version", "Usage: y-obs version\n\nThe y-obs, DuckDB and Go versions, and where httpfs comes from.\n")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	s := &session{plan: map[string][]string{}}
	if err := s.openEngine(ctx, nil); err != nil {
		return err
	}
	defer s.close()
	v, err := s.strings(ctx, "SELECT version()")
	if err != nil || len(v) == 0 {
		return fail(exitStore, "the engine: %v", err)
	}
	httpfs := "installed from DuckDB's signed repository at first use"
	if len(httpfsExtension) > 0 {
		httpfs = fmt.Sprintf("embedded (%d bytes)", len(httpfsExtension))
	}
	fmt.Printf("y-obs %s\nduckdb %s\ngo %s %s/%s\nhttpfs %s\n", version, v[0], runtime.Version(), runtime.GOOS, runtime.GOARCH, httpfs)
	return nil
}

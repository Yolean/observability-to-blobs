//go:build httpfs_embed

package main

import _ "embed"

// httpfsExtension is DuckDB's signed httpfs extension for this platform,
// put in place by release.sh, which pins it by checksum (release.env).
//
//go:embed httpfs/httpfs.duckdb_extension
var httpfsExtension []byte

//go:build !httpfs_embed

package main

// httpfsExtension is empty in a development build: the session installs
// httpfs from DuckDB's signed repository instead (release.sh embeds it).
var httpfsExtension []byte

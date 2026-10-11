package main

//go:generate cp ../../compactor/sql/raw.sql ../../compactor/sql/tables.sql ../../compactor/sql/pods.sql ../../local/apiserver_at.sql sql/

import "embed"

// sqlFS holds byte-identical copies of the views the compactor, the itest
// and local/blobs.sh read (compactor/sql/, local/apiserver_at.sql), so the
// binary needs no checkout. `go generate` refreshes them;
// TestSQLCopied fails when they differ from their sources.
//
//go:embed sql/*.sql
var sqlFS embed.FS

// sqlSources maps each embedded file to its source in the repository.
var sqlSources = map[string]string{
	"raw.sql":          "../../compactor/sql/raw.sql",
	"tables.sql":       "../../compactor/sql/tables.sql",
	"pods.sql":         "../../compactor/sql/pods.sql",
	"apiserver_at.sql": "../../local/apiserver_at.sql",
}

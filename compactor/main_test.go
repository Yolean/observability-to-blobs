package main

import (
	"context"
	"testing"
)

func TestRoundtripDeduplicates(t *testing.T) {
	db, err := open("", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	n, err := roundtrip(context.Background(), db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 1000 lines where line i+500 repeats line i, as a retried upload
	// repeats an object: DISTINCT folds them to 500.
	if n != 500 {
		t.Fatalf("rows = %d, want 500", n)
	}
}

func TestSQLString(t *testing.T) {
	if got := sqlString("/tmp/it's"); got != "'/tmp/it''s'" {
		t.Fatalf("sqlString = %s", got)
	}
}

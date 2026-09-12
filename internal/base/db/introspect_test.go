package db

import (
	"strings"
	"testing"
)

func TestExtractChecks(t *testing.T) {
	sql := `CREATE TABLE t (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  status TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'do(ne)')),
  note TEXT NOT NULL DEFAULT 'CHECK (fake)',
  dedup_key TEXT NOT NULL CHECK (dedup_key <> ''),
  unchecked TEXT
);`
	got := extractChecks(sql)
	want := []string{"status IN ('open', 'do(ne)')", "dedup_key <> ''"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("extractChecks 不对：\n得到 %q\n想要 %q", got, want)
	}
}

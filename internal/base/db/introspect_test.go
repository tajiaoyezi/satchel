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

func TestNormalizeDefault(t *testing.T) {
	for in, want := range map[string]string{
		"'-1'::integer":           "-1",
		"'open'::text":            "'open'",
		"CURRENT_TIMESTAMP":       "current_timestamp",
		"'{}'::jsonb":             "'{}'",
		"0":                       "0",
		"'0.5'::double precision": "0.5",
	} {
		if got := normalizeDefault(in); got != want {
			t.Errorf("normalizeDefault(%q) = %q，想要 %q", in, got, want)
		}
	}
}

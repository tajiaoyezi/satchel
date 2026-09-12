package dbtest

import (
	"strings"
	"testing"
)

func TestPGDecision(t *testing.T) {
	if skip, fail := pgDecision("postgres://x", ""); skip != "" || fail != "" {
		t.Fatal("有 DSN 时既不跳过也不失败")
	}
	skip, fail := pgDecision("", "")
	if fail != "" || !strings.Contains(skip, EnvDSN) || !strings.Contains(skip, "README") {
		t.Fatalf("没有 DSN 时应当跳过并说明怎么起库，得到 skip=%q fail=%q", skip, fail)
	}
	skip, fail = pgDecision("", "1")
	if skip != "" || !strings.Contains(fail, "缺少 PostgreSQL") {
		t.Fatalf("要求 PostgreSQL 又没有 DSN 时应当失败并说明，得到 skip=%q fail=%q", skip, fail)
	}
}

func TestWithSearchPath(t *testing.T) {
	cases := map[string]string{
		"postgres://u@h/d":                 "postgres://u@h/d?search_path=t_1",
		"postgres://u@h/d?sslmode=disable": "postgres://u@h/d?sslmode=disable&search_path=t_1",
		"host=h dbname=d":                  "host=h dbname=d search_path=t_1",
	}
	for in, want := range cases {
		if got := withSearchPath(in, "t_1"); got != want {
			t.Errorf("withSearchPath(%q) = %q，想要 %q", in, got, want)
		}
	}
}

package buildinfo

import "testing"

func TestDefaults(t *testing.T) {
	got := Get()
	want := Info{Version: "0.0.0-dev", Commit: "unknown", Date: "unknown"}
	if got != want {
		t.Fatalf("默认构建信息 = %+v，想要 %+v", got, want)
	}
}

package overview

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/satchel/satchel/internal/command"
)

// master-overview「分区随对应功能出现」：M1 的四个分区都是 items 为空数组、total 为 0，四个分区一个不少。
func TestOverviewEmptyPartitions(t *testing.T) {
	h := New().Bindings()["overview"]
	got, err := h(context.Background(), &command.Invocation{Path: []string{"overview"}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if len(out) != 4 {
		t.Fatalf("应当恰好四个分区：%s", raw)
	}
	for _, key := range []string{"servers", "users", "alerts", "tasks"} {
		p, ok := out[key]
		if !ok {
			t.Fatalf("缺分区 %s：%s", key, raw)
		}
		if string(p["items"]) != "[]" || string(p["total"]) != "0" {
			t.Errorf("%s 应当是 items [] 与 total 0：%s", key, raw)
		}
	}
}

package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// master-overview「文本形式」：每个分区一行「名称：总数」，顺序是服务器、用户、告警、待办；结果是经主控拿到的 JSON 对象。
func TestRenderOverview(t *testing.T) {
	var empty map[string]any
	if err := json.Unmarshal([]byte(`{"apiVersion":"satchel/v1","servers":{"items":[],"total":0},"users":{"items":[],"total":0},`+
		`"alerts":{"items":[],"total":0},"tasks":{"items":[],"total":0}}`), &empty); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := renderOverview(&b, empty); err != nil {
		t.Fatal(err)
	}
	if want := "服务器：0\n用户：0\n告警：0\n待办：0\n"; b.String() != want {
		t.Fatalf("文本形式应当是四行：\n%s", b.String())
	}

	// 分区有条目时逐条缩进列在那一行下面（条目的写法随交付分区的那一站定）。
	var withItems map[string]any
	if err := json.Unmarshal([]byte(`{"servers":{"items":["tokyo-1","osaka-2"],"total":5},"users":{"items":[],"total":0},`+
		`"alerts":{"items":[],"total":0},"tasks":{"items":[],"total":0}}`), &withItems); err != nil {
		t.Fatal(err)
	}
	b.Reset()
	if err := renderOverview(&b, withItems); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), "服务器：5\n  tokyo-1\n  osaka-2\n用户：0\n") {
		t.Fatalf("条目应当缩进列在分区那一行下面：\n%s", b.String())
	}
}

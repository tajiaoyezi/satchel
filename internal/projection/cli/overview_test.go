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

	// 服务器分区对象条目格式：name、status、connection_mode、ip。
	var withServers map[string]any
	if err := json.Unmarshal([]byte(`{"servers":{"items":[{"id":1,"name":"tokyo-1","connection_mode":"auto","status":"connected","ip_address":"198.51.100.1","ip_address_v6":""},{"id":2,"name":"osaka-2","connection_mode":"pull","status":"pending","ip_address":null,"ip_address_v6":""}],"total":2},"users":{"items":[],"total":0},"alerts":{"items":[],"total":0},"tasks":{"items":[],"total":0}}`), &withServers); err != nil {
		t.Fatal(err)
	}
	b.Reset()
	if err := renderOverview(&b, withServers); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(b.String(), "服务器：2\n  tokyo-1\tconnected\tauto\t198.51.100.1\n  osaka-2\tpending\tpull\t-\n用户：0\n") {
		t.Fatalf("服务器条目的单行格式不对：\n%s", b.String())
	}
}

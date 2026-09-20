package rest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-rest-api「请求解码」：object 类型的 flag 在请求体里必须是 JSON 对象，值原样交给处理函数。
func TestObjectFlagBody(t *testing.T) {
	tbl, err := command.New(
		&command.Command{Path: []string{"settings", "set"}, Summary: "s", Class: command.ClassMasterSettings,
			Flags: []command.Flag{{Name: "set", Type: command.TypeObject, Kind: "SystemSettings"}, {Name: "resource-version", Type: command.TypeInt}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	e := &echo{}
	h := NewHandler(tbl, e, nil)
	hdr := map[string]string{"Content-Type": "application/json"}
	code, _ := do(t, h, "POST", "/api/v1/settings/set", `{"set":{"heartbeat_interval":"45","agent_log_enabled":true,"probe_disguise_server_ids":[1,2]},"resource-version":1}`, hdr)
	if code != 200 {
		t.Fatalf("对象应当放行：%d", code)
	}
	obj, ok := e.last.Flags["set"].(map[string]any)
	if !ok || obj["heartbeat_interval"] != "45" || obj["agent_log_enabled"] != true || len(obj) != 3 {
		t.Fatalf("set 应当原样是对象：%#v", e.last.Flags["set"])
	}
	if list, ok := obj["probe_disguise_server_ids"].([]any); !ok || len(list) != 2 {
		t.Fatalf("对象里的数组原样保留：%#v", obj["probe_disguise_server_ids"])
	}
	if e.last.Flags["resource-version"] != 1 {
		t.Fatalf("resource-version：%#v", e.last.Flags["resource-version"])
	}
	for _, body := range []string{
		`{"set":"heartbeat_interval=30","resource-version":1}`,
		`{"set":["heartbeat_interval"],"resource-version":1}`,
		`{"set":null,"resource-version":1}`,
		`{"set":1,"resource-version":1}`,
	} {
		e.last = nil
		code, fields := do(t, h, "POST", "/api/v1/settings/set", body, hdr)
		if code != 400 {
			t.Errorf("%s 应当 400，得到 %d", body, code)
			continue
		}
		if err := errorOf(t, fields); err.Code != v1.CodeBadRequest || !strings.Contains(err.Reason, "set") {
			t.Errorf("%s 应当是点名 set 的 bad_request：%+v", body, err)
		}
		if e.last != nil {
			t.Errorf("%s 不该到达执行链", body)
		}
	}
	// 空对象放行（要不要拒由处理函数定）。
	if code, _ := do(t, h, "POST", "/api/v1/settings/set", `{"set":{},"resource-version":1}`, hdr); code != 200 {
		t.Fatalf("空对象应当放行：%d", code)
	}
	raw, _ := json.Marshal(e.last.Flags["set"])
	if string(raw) != "{}" {
		t.Fatalf("空对象：%s", raw)
	}
}

package audit

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-audit-log「参数摘要」：object 类型的 flag 按所属 kind 的打码字段集合逐键打码，其余键照记。
func TestDigestMasksObjectByKind(t *testing.T) {
	tbl, err := command.New(&command.Command{Path: []string{"settings", "set"}, Summary: "s", Class: command.ClassMasterSettings,
		Flags: []command.Flag{{Name: "set", Type: command.TypeObject, Kind: "SystemSettings"}, {Name: "resource-version", Type: command.TypeInt}}})
	if err != nil {
		t.Fatal(err)
	}
	cmd, _ := tbl.Lookup("settings set")
	inv := &command.Invocation{Path: []string{"settings", "set"}, Flags: map[string]any{
		"set":              map[string]any{"probe_external_token_sha256": "abc123", "heartbeat_interval": "45", "turnstile_secret_key": "s3cret"},
		"resource-version": 1,
	}}
	d := Digest(cmd, inv)
	if strings.Contains(d, "abc123") || strings.Contains(d, "s3cret") {
		t.Fatalf("打码字段的值不该出现：%s", d)
	}
	var got struct {
		Flags struct {
			Set             map[string]any `json:"set"`
			ResourceVersion int            `json:"resource-version"`
		} `json:"flags"`
	}
	if err := json.Unmarshal([]byte(d), &got); err != nil {
		t.Fatal(err)
	}
	if got.Flags.Set["probe_external_token_sha256"] != v1.Redacted || got.Flags.Set["turnstile_secret_key"] != v1.Redacted || got.Flags.Set["heartbeat_interval"] != "45" || got.Flags.ResourceVersion != 1 {
		t.Fatalf("摘要应当逐键打码、其余照记：%s", d)
	}
	// 原来的对象不被改动。
	if inv.Flags["set"].(map[string]any)["probe_external_token_sha256"] != "abc123" {
		t.Fatal("Digest 不该改调用对象")
	}
	// kind 不认识时不打码但也不崩。
	if d := Digest(&command.Command{Path: []string{"x"}, Flags: []command.Flag{{Name: "set", Type: command.TypeObject, Kind: "NoSuch"}}}, inv); !strings.Contains(d, "abc123") {
		t.Fatalf("kind 不认识时原样记录：%s", d)
	}
}

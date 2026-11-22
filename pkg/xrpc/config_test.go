package xrpc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

func TestCanonicalJSONAndHash(t *testing.T) {
	rawA := []byte(`{
		"outbounds": [{"tag": "direct", "type": "direct"}],
		"inbounds": [{"listen_port": 443, "tag": "vless-1", "type": "vless", "users": [{"name": "a&b<c>", "uuid": "1111"}]}],
		"log": {"level": "info"}
	}`)
	rawB := []byte(`{"log":{"level":"info"},"inbounds":[{"type":"vless","tag":"vless-1","listen_port":443,"users":[{"uuid":"1111","name":"a&b<c>"}]}],"outbounds":[{"type":"direct","tag":"direct"}]}`)

	canonA, err := CanonicalJSON(rawA)
	if err != nil {
		t.Fatalf("CanonicalJSON(rawA): %v", err)
	}
	canonB, err := CanonicalJSON(rawB)
	if err != nil {
		t.Fatalf("CanonicalJSON(rawB): %v", err)
	}
	if string(canonA) != string(canonB) {
		t.Fatalf("canonical mismatch:\nA=%s\nB=%s", canonA, canonB)
	}
	if want := `{"inbounds":[{"listen_port":443,"tag":"vless-1","type":"vless","users":[{"name":"a&b<c>","uuid":"1111"}]}],"log":{"level":"info"},"outbounds":[{"tag":"direct","type":"direct"}]}`; string(canonA) != want {
		t.Fatalf("unexpected canonical json:\ngot:  %s\nwant: %s", canonA, want)
	}
	if hA, hB := CanonicalConfigHash(rawA), CanonicalConfigHash(rawB); hA != hB || len(hA) != 64 {
		t.Fatalf("hash mismatch: %q vs %q", hA, hB)
	}

	// 含多余尾部 token 应报错。
	if _, err := CanonicalJSON([]byte(`{"a":1} {"b":2}`)); err == nil {
		t.Fatalf("expected error for trailing JSON token")
	}

	// 含 sing-box 注释时 CanonicalJSON 报错，CanonicalConfigHash 退回对去首尾空白原文字节哈希。
	jsonc := []byte("  // comment\n  {\"log\":{\"level\":\"warn\"}}\n")
	if _, err := CanonicalJSON(jsonc); err == nil {
		t.Fatalf("expected error for JSON with comments")
	}
	wantSum := sha256.Sum256([]byte("// comment\n  {\"log\":{\"level\":\"warn\"}}"))
	if got, want := CanonicalConfigHash(jsonc), hex.EncodeToString(wantSum[:]); got != want {
		t.Fatalf("fallback hash = %q, want %q", got, want)
	}
}

func TestConfigRPCRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)

	// WantRestart 默认 true，显式 false 为 false。
	if !(ConfigApplyParams{Config: "{}"}).WantRestart() {
		t.Fatalf("default WantRestart should be true")
	}
	f := false
	if (ConfigApplyParams{Config: "{}", Restart: &f}).WantRestart() {
		t.Fatalf("explicit false WantRestart should be false")
	}

	cases := []any{
		ConfigStatusResult{
			Exists:        true,
			Path:          "/etc/satchel-agent/kernel.json",
			Hash:          "abcd",
			Size:          128,
			Modified:      &now,
			Inbounds:      []string{"in-1"},
			KernelRunning: true,
			KernelVersion: "1.14.4",
			Generation:    3,
		},
		ConfigTestParams{Config: `{"log":{"level":"info"}}`},
		ConfigTestResult{OK: true, Inbounds: []string{"in-1"}, Hash: "abcd"},
		ConfigApplyParams{Config: `{}`, ExpectedCurrentHash: "old", ForceInitial: true, ApplyID: "apply-1", ProtectMaster443: true},
		ConfigApplyResult{Applied: false, RolledBack: true, RollbackError: "", Error: "bind: address already in use", CurrentHash: "old", Generation: 2},
		ConfigFilesParams{Scope: "all"},
		ConfigFilesResult{Files: []ConfigFileInfo{{Scope: "kernel", Path: "/etc/satchel-agent/kernel.json", Name: "kernel.json", Size: 128, Modified: now}}},
		ConfigReadParams{Path: "/etc/satchel-agent/kernel.json", MaxBytes: ConfigReadMaxBytes},
		ConfigReadResult{Scope: "kernel", Path: "/etc/satchel-agent/kernel.json", Content: "{}", Size: 2, Modified: now, Hash: "4413"},
	}

	for i, tc := range cases {
		b, err := json.Marshal(tc)
		if err != nil {
			t.Fatalf("case %d marshal: %v", i, err)
		}
		if len(b) == 0 {
			t.Fatalf("case %d empty json", i)
		}
	}
	// 验证 ProtectMaster443 omitempty 向后兼容：false 时不序列化该字段，true 时完整往返。
	rawFalse, err := json.Marshal(ConfigApplyParams{Config: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if string(rawFalse) != `{"config":"{}"}` {
		t.Fatalf("expected protect_master_443 omitted when false, got %s", rawFalse)
	}
	rawTrue, err := json.Marshal(ConfigApplyParams{Config: `{}`, ProtectMaster443: true})
	if err != nil {
		t.Fatal(err)
	}
	var gotApply ConfigApplyParams
	if err := json.Unmarshal(rawTrue, &gotApply); err != nil {
		t.Fatal(err)
	}
	if !gotApply.ProtectMaster443 {
		t.Fatalf("expected ProtectMaster443=true after round-trip, got %+v", gotApply)
	}
	if CodeConflict != "conflict" {
		t.Fatalf("CodeConflict = %q, want conflict", CodeConflict)
	}
}

package v1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// m0-06 的契约修正：metadata.name 的来源与省略、省略即为真、Node 三列打码。

// resource-model「Kind catalog with operation class」：名字字段集合来自自然键。
func TestNameFields(t *testing.T) {
	want := map[Kind][]string{
		"User": {"username"}, "Inbound": {"server_id", "tag"}, "Certificate": {"domain", "server_id"},
		"CustomRule": {"name", "type"}, "Task": nil, "PackageAssignment": nil, "SystemSettings": nil,
	}
	for kind, fields := range want {
		k, ok := Lookup(kind)
		if !ok {
			t.Fatalf("没有 kind %s", kind)
		}
		if strings.Join(k.NameFields, ",") != strings.Join(fields, ",") {
			t.Errorf("%s 的 NameFields 应当是 %v，得到 %v", kind, fields, k.NameFields)
		}
	}
	named := 0
	for _, k := range Kinds() {
		for _, f := range k.NameFields {
			if !contains(k.SpecFields, f) {
				t.Errorf("kind %s 的名字字段 %s 不在 spec 字段集合里", k.Name, f)
			}
		}
		if len(k.NameFields) > 0 {
			named++
		}
	}
	if named != 15 {
		t.Errorf("有自然键的 kind 应当是 15 个，得到 %d", named)
	}
}

// resource-model「Object envelope」：单列自然键直接用值，复合用 / 连接，没有自然键就省略 name。
func TestObjectNameAndOmission(t *testing.T) {
	inbound, _ := Lookup("Inbound")
	if name, ok := inbound.ObjectName(InboundSpec{ServerID: 3, Tag: "vless-in"}); !ok || name != "3/vless-in" {
		t.Errorf("Inbound 的 name 应当是 3/vless-in，得到 %q %v", name, ok)
	}
	rule, _ := Lookup("CustomRule")
	if name, ok := rule.ObjectName(&CustomRuleSpec{Name: "dns", Type: "rules"}); !ok || name != "dns/rules" {
		t.Errorf("CustomRule 的 name 应当是 dns/rules，得到 %q %v", name, ok)
	}
	cert, _ := Lookup("Certificate")
	if name, ok := cert.ObjectName(CertificateSpec{Domain: "example.com"}); !ok || name != "example.com/" {
		t.Errorf("主控本机证书的 name 应当是 example.com/，得到 %q %v", name, ok)
	}
	user, _ := Lookup("User")
	if name, ok := user.ObjectName(UserSpec{Username: "alice"}); !ok || name != "alice" {
		t.Errorf("User 的 name 应当是 alice，得到 %q %v", name, ok)
	}
	task, _ := Lookup("Task")
	if name, ok := task.ObjectName(TaskSpec{Title: "x"}); ok || name != "" {
		t.Errorf("Task 没有自然键，不该有 name，得到 %q %v", name, ok)
	}
	if name, ok := inbound.ObjectName(TaskSpec{Title: "x"}); ok || name != "" {
		t.Errorf("传错结构体应当返回 false 而不是 panic，得到 %q %v", name, ok)
	}

	// 序列化时 name 由 spec 算出：调用方填的值被覆盖，没填也会补上。
	ts0 := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	in := Object[InboundSpec, InboundStatus]{
		APIVersion: APIVersion, Kind: "Inbound",
		Metadata: Metadata{ID: 7, Name: "stale", ResourceVersion: 1, CreatedAt: ts0, UpdatedAt: ts0},
		Spec:     InboundSpec{ServerID: 3, Tag: "vless-in", Protocol: "vless", Port: 443},
	}
	if got := metadataName(t, in); got != "3/vless-in" {
		t.Errorf("Inbound 序列化后 metadata.name 应当是 3/vless-in，得到 %q", got)
	}
	usr := Object[UserSpec, UserStatus]{APIVersion: APIVersion, Kind: "User", Metadata: Metadata{ID: 1, ResourceVersion: 1, CreatedAt: ts0, UpdatedAt: ts0}, Spec: UserSpec{Username: "alice"}}
	if got := metadataName(t, usr); got != "alice" {
		t.Errorf("User 序列化后 metadata.name 应当是 alice，得到 %q", got)
	}
	// 往返：解回来的 name 与序列化的一致。
	data0, _ := json.Marshal(in)
	back, err := Decode[InboundSpec, InboundStatus](data0)
	if err != nil || back.Metadata.Name != "3/vless-in" {
		t.Errorf("往返后 name 应当保留，得到 %+v %v", back, err)
	}
	// spec 不是该 kind 的结构体：报 internal，不 panic。
	wrong := Object[TaskSpec, TaskStatus]{APIVersion: APIVersion, Kind: "Inbound", Spec: TaskSpec{Title: "x"}}
	if _, err := json.Marshal(wrong); err == nil || !strings.Contains(err.Error(), "metadata.name") {
		t.Errorf("spec 类型对不上应当报错，得到 %v", err)
	}
	// RawObject 不算 name，原样输出。
	raw := RawObject{APIVersion: APIVersion, Kind: "Inbound", Metadata: Metadata{ID: 7}, Spec: json.RawMessage(`{"server_id":3,"tag":"x"}`), Status: json.RawMessage(`{}`)}
	if got := metadataName(t, raw); got != "" {
		t.Errorf("RawObject 不该算 name，得到 %q", got)
	}

	ts := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	obj := Object[PackageAssignmentSpec, PackageAssignmentStatus]{
		APIVersion: APIVersion, Kind: "PackageAssignment",
		Metadata: Metadata{ID: 9, ResourceVersion: 1, CreatedAt: ts, UpdatedAt: ts},
		Spec:     PackageAssignmentSpec{Username: "alice", PackageID: 2},
	}
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(top["metadata"], &meta); err != nil {
		t.Fatal(err)
	}
	if _, has := meta["name"]; has {
		t.Errorf("没有自然键的 kind 序列化后 metadata 不该有 name：%s", top["metadata"])
	}
	if _, has := meta["id"]; !has {
		t.Errorf("metadata 应当有 id：%s", top["metadata"])
	}
	// 反序列化时 metadata 不带 name 也接受。
	if _, err := Decode[PackageAssignmentSpec, PackageAssignmentStatus]([]byte(`{"apiVersion":"satchel/v1","kind":"PackageAssignment","metadata":{"id":9,"resourceVersion":1},"spec":{"username":"alice","package_id":2}}`)); err != nil {
		t.Fatalf("不带 name 的 metadata 应当被接受：%v", err)
	}
}

// resource-model「Kind catalog with operation class」与「Apply field rejection list」：省略即为真。
func TestDefaultTrueFields(t *testing.T) {
	want := map[Kind][]string{
		"Node": {"enabled"}, "Server": {"ipv6_enabled", "include_in_traffic_stats"}, "User": {"is_active"},
		"NotifyChannel": nil, "Task": nil,
	}
	for kind, fields := range want {
		k, _ := Lookup(kind)
		if strings.Join(k.DefaultTrueFields, ",") != strings.Join(fields, ",") {
			t.Errorf("%s 的 DefaultTrueFields 应当是 %v，得到 %v", kind, fields, k.DefaultTrueFields)
		}
	}
	total := 0
	for _, k := range Kinds() {
		total += len(k.DefaultTrueFields)
	}
	// 18 列里有 5 列在附属表上，kind 清单里是 13 个。
	if total != 13 {
		t.Errorf("kind 清单里省略即为真的字段应当共 13 个，得到 %d", total)
	}
	// 渠道默认关：NotifyChannel 没填 enabled 解出 false。
	var ch NotifyChannelSpec
	if err := DecodeSpec("NotifyChannel", []byte(`{"name":"ops","type":"webhook"}`), &ch); err != nil {
		t.Fatal(err)
	}
	if ch.Enabled {
		t.Error("NotifyChannel 没填 enabled 应当是 false")
	}

	var node NodeSpec
	if err := DecodeSpec("Node", []byte(`{"username":"alice","raw_url":"vless://u@h:443","node_name":"jp","protocol":"vless"}`), &node); err != nil {
		t.Fatal(err)
	}
	if !node.Enabled {
		t.Error("没填 enabled 的 Node spec 应当解出 true")
	}
	var explicit NodeSpec
	if err := DecodeSpec("Node", []byte(`{"username":"alice","enabled":false}`), &explicit); err != nil {
		t.Fatal(err)
	}
	if explicit.Enabled {
		t.Error("显式 false 应当保持 false")
	}
	var srv ServerSpec
	if err := DecodeSpec("Server", []byte(`{"name":"tokyo"}`), &srv); err != nil {
		t.Fatal(err)
	}
	if !srv.Ipv6Enabled || !srv.IncludeInTrafficStats {
		t.Errorf("Server 的两个省略即为真字段应当是 true：%+v", srv)
	}
	if srv.Use443 || srv.DdnsEnabled || srv.LockEntryIP {
		t.Errorf("没标省略即为真的布尔字段仍应是 false：%+v", srv)
	}
	// User 的 is_active 是动作专属，不在 spec 里：DecodeSpec 不碰它，写进 spec 会被拒。
	var user UserSpec
	if err := DecodeSpec("User", []byte(`{"username":"alice","is_active":true}`), &user); codeOf(t, err).Code != CodeFieldNotApplyable {
		t.Errorf("is_active 应当被拒收，得到 %v", err)
	}
	var tok ApiTokenSpec
	if err := DecodeSpec("ApiToken", []byte(`{}`), &tok); err != nil {
		t.Errorf("空 spec 的 kind 不受影响：%v", err)
	}
}

// storage-schema「Core kind tables」：Node 的 URI、解析结果与 Clash 片段默认打码。
func TestNodeSpecMasksCredentials(t *testing.T) {
	node, _ := Lookup("Node")
	for _, f := range []string{"raw_url", "parsed_config", "clash_config", "routed_outbound_json", "routed_admin_credential"} {
		if !contains(node.MaskedFields, f) {
			t.Errorf("Node 的打码字段集合应当含 %s，得到 %v", f, node.MaskedFields)
		}
	}
	spec := NodeSpec{
		Username: "alice", RawURL: "vless://11111111-2222-3333-4444-555555555555@h:443", NodeName: "jp",
		ParsedConfig: SecretJSON(`{"uuid":"11111111-2222-3333-4444-555555555555"}`), ClashConfig: "password: hunter2",
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"11111111-2222", "hunter2", "vless://"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("序列化输出不该含原文 %q：%s", secret, data)
		}
	}
	if !strings.Contains(string(data), `"node_name":"jp"`) {
		t.Errorf("node_name 不打码：%s", data)
	}
}

// metadataName 序列化一个信封，取出 metadata.name（没有时返回空串）。
func metadataName[S, T any](t *testing.T, obj Object[S, T]) string {
	t.Helper()
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	raw, ok := top.Metadata["name"]
	if !ok {
		return ""
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		t.Fatal(err)
	}
	return name
}

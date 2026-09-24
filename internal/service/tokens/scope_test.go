package tokens

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"

	core "github.com/satchel/satchel/internal/core/tokens"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-api-tokens「令牌的形状与存储」：sat_ 加 43 个 base64url 字符，库里只存 SHA-256 十六进制。
func TestFormat(t *testing.T) {
	a, ha, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := Generate()
	if !strings.HasPrefix(a, Prefix) || len(a) != 47 || a == b {
		t.Fatalf("令牌形状不对：%q %q", a, b)
	}
	sum := sha256.Sum256([]byte(a))
	if ha != hex.EncodeToString(sum[:]) || Hash(a) != ha || len(ha) != 64 {
		t.Fatalf("哈希应当是明文的 SHA-256 十六进制：%s", ha)
	}
	if strings.ContainsAny(a[len(Prefix):], "+/=") {
		t.Fatalf("应当是无填充的 base64url：%s", a)
	}
}

func grant(scopes []v1.Scope, danger ...v1.Danger) core.Grant {
	return core.Grant{Scopes: scopes, Danger: danger}
}

var (
	readOnly = []v1.Scope{v1.ScopeRead}
	readOp   = []v1.Scope{v1.ScopeRead, v1.ScopeOperate}
)

// master-api-tokens「权限范围与预设」：预设由权限范围推出，secrets 不影响预设。
func TestPreset(t *testing.T) {
	cases := []struct {
		g    core.Grant
		want string
	}{
		{grant(readOnly), PresetReadonly},
		{grant([]v1.Scope{v1.ScopeRead, v1.ScopeSecrets}), PresetReadonly},
		{grant(readOp), PresetOps},
		{grant(readOp, v1.DangerDelete), PresetOps},
		{grant([]v1.Scope{v1.ScopeRead, v1.ScopeOperate, v1.ScopeSecrets}, v1.AllDangers[:5]...), PresetOps},
		{grant(readOp, v1.AllDangers...), PresetFull},
		{grant([]v1.Scope{v1.ScopeRead, v1.ScopeOperate, v1.ScopeSecrets}, v1.AllDangers...), PresetFull},
	}
	for _, tc := range cases {
		if got := presetOf(normalize(tc.g)); got != tc.want {
			t.Errorf("%+v 应当推出 %s，得到 %s", tc.g, tc.want, got)
		}
	}
	// 规范化：read 恒在、去重、按固定顺序。
	g := normalize(grant([]v1.Scope{v1.ScopeSecrets, v1.ScopeOperate, v1.ScopeOperate}, v1.DangerMaster, v1.DangerDelete, v1.DangerDelete))
	if !reflect.DeepEqual(g.Scopes, []v1.Scope{v1.ScopeRead, v1.ScopeOperate, v1.ScopeSecrets}) || !reflect.DeepEqual(g.Danger, []v1.Danger{v1.DangerDelete, v1.DangerMaster}) {
		t.Fatalf("规范化不对：%+v", g)
	}
	if e := normalize(core.Grant{}); !reflect.DeepEqual(e.Scopes, readOnly) || e.Danger == nil {
		t.Fatalf("空的权限范围规范化后是只读、危险类是空数组而不是 null：%+v", e)
	}
}

func str(s string) *string { return &s }
func boolp(b bool) *bool   { return &b }

// --preset / --danger / --secrets 的组合规则。
func TestScopeChange(t *testing.T) {
	base := grant(readOnly)
	cases := []struct {
		name   string
		base   core.Grant
		c      change
		scopes []v1.Scope
		danger []v1.Danger
		bad    bool
	}{
		{"什么都不给是只读", base, change{}, readOnly, []v1.Danger{}, false},
		{"ops", base, change{preset: str("ops")}, readOp, []v1.Danger{}, false},
		{"full", base, change{preset: str("full")}, readOp, v1.AllDangers, false},
		{"ops 加一个危险类", base, change{preset: str("ops"), danger: []string{"delete"}, dangerG: true}, readOp, []v1.Danger{v1.DangerDelete}, false},
		{"只给危险类隐含 operate", base, change{danger: []string{"exec", "restart"}, dangerG: true}, readOp, []v1.Danger{v1.DangerRestart, v1.DangerExec}, false},
		{"full 再用 danger 替换", base, change{preset: str("full"), danger: []string{"batch"}, dangerG: true}, readOp, []v1.Danger{v1.DangerBatch}, false},
		{"只读带危险类", base, change{preset: str("readonly"), danger: []string{"delete"}, dangerG: true}, nil, nil, true},
		{"不认识的预设", base, change{preset: str("admin")}, nil, nil, true},
		{"不认识的危险类", base, change{danger: []string{"nuke"}, dangerG: true}, nil, nil, true},
		{"空危险类清单只是清空", grant(readOp, v1.DangerDelete), change{danger: []string{}, dangerG: true}, readOp, []v1.Danger{}, false},
		{"空危险类清单不把只读变成可操作", base, change{danger: []string{}, dangerG: true}, readOnly, []v1.Danger{}, false},
		{"打开 secrets", base, change{secrets: boolp(true)}, []v1.Scope{v1.ScopeRead, v1.ScopeSecrets}, []v1.Danger{}, false},
		{"换预设保留 secrets", grant([]v1.Scope{v1.ScopeRead, v1.ScopeSecrets}), change{preset: str("ops")}, []v1.Scope{v1.ScopeRead, v1.ScopeOperate, v1.ScopeSecrets}, []v1.Danger{}, false},
		{"关掉 secrets", grant([]v1.Scope{v1.ScopeRead, v1.ScopeOperate, v1.ScopeSecrets}), change{secrets: boolp(false)}, readOp, []v1.Danger{}, false},
		{"降成只读清掉危险类", grant(readOp, v1.DangerDelete), change{preset: str("readonly")}, readOnly, []v1.Danger{}, false},
		{"只改危险类保留原有的 operate", grant(readOp, v1.DangerDelete), change{danger: []string{"master"}, dangerG: true}, readOp, []v1.Danger{v1.DangerMaster}, false},
	}
	for _, tc := range cases {
		got, err := tc.c.apply(tc.base)
		if tc.bad {
			if err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
				t.Errorf("%s：应当 bad_request，得到 %+v %v", tc.name, got, err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got.Scopes, tc.scopes) || !reflect.DeepEqual(got.Danger, tc.danger) {
			t.Errorf("%s：得到 %+v %v，想要 scopes=%v danger=%v", tc.name, got, err, tc.scopes, tc.danger)
		}
	}
}

// master-api-tokens「签发者与权限上限」：普通用户只能 read 与 operate，超出点名；使用时取交集。
func TestCap(t *testing.T) {
	full := normalize(grant([]v1.Scope{v1.ScopeRead, v1.ScopeOperate, v1.ScopeSecrets}, v1.AllDangers...))
	if err := checkCap(full, v1.RoleAdmin); err != nil {
		t.Fatalf("管理员什么都能签：%v", err)
	}
	if err := checkCap(normalize(grant(readOp)), v1.RoleUser); err != nil {
		t.Fatalf("普通用户能签 ops：%v", err)
	}
	err := checkCap(normalize(grant([]v1.Scope{v1.ScopeRead, v1.ScopeSecrets}, v1.DangerDelete)), v1.RoleUser)
	e := v1.AsError(err)
	if err == nil || e.Code != v1.CodeForbidden || !strings.Contains(e.Reason, "secrets") || !strings.Contains(e.Reason, "delete") {
		t.Fatalf("普通用户带危险类与 secrets 应当 forbidden 并点名：%v", err)
	}
	scopes, danger := intersect(full, v1.RoleUser)
	if !reflect.DeepEqual(scopes, readOp) || len(danger) != 0 || danger == nil {
		t.Fatalf("降级后的交集应当只剩 read 与 operate：%v %v", scopes, danger)
	}
	scopes, danger = intersect(full, v1.RoleAdmin)
	if len(scopes) != 3 || len(danger) != len(v1.AllDangers) {
		t.Fatalf("管理员的交集是原样：%v %v", scopes, danger)
	}
}

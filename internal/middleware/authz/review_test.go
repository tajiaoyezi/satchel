package authz

import (
	"testing"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-identity-and-authz「confirm 是字符串」：object 口径逐字相等（不去空白），count 口径必须是规范的十进制。
func TestConfirmShapes(t *testing.T) {
	c := &calls{}
	r := Wrap(testTable(t), nil, c.runner())
	admin := v1.LocalAdmin("root")
	for _, bad := range []string{"alice ", " alice", "Alice", "ALICE"} {
		if e := run(t, r, admin, []string{"demo", "remove"}, []string{"alice"}, bad); e == nil || e.Code != v1.CodeConfirmRequired {
			t.Errorf("object 口径 %q 应当被拒：%+v", bad, e)
		}
	}
	for _, bad := range []string{"true", "01", "+12", "12 ", "-1", "1.0", "twelve", ""} {
		if e := run(t, r, admin, []string{"demo", "bulk"}, nil, bad); e == nil || e.Code != v1.CodeConfirmRequired || e.State["kind"] != "count" {
			t.Errorf("count 口径 %q 应当被拒：%+v", bad, e)
		}
	}
	for _, ok := range []string{"0", "7", "12", "1200"} {
		if e := run(t, r, admin, []string{"demo", "bulk"}, nil, ok); e != nil {
			t.Errorf("count 口径 %q 应当放行：%+v", ok, e)
		}
	}
	if c.n != 4 {
		t.Fatalf("应当恰好执行 4 次，得到 %d", c.n)
	}
}

// 顺序的另外两条反向用例：人类专属先于 scope，危险类先于 confirm。
func TestOrderEdges(t *testing.T) {
	c := &calls{}
	r := Wrap(testTable(t), nil, c.runner())
	// 只读令牌调人类专属命令：是 human_required（第 ② 步），不是缺 operate 的 forbidden（第 ③ 步）。
	if e := run(t, r, token([]v1.Scope{v1.ScopeRead}, nil), []string{"demo", "lock"}, nil, ""); e == nil || e.Code != v1.CodeHumanRequired {
		t.Fatalf("应当 human_required：%+v", e)
	}
	// 有 operate 没危险类、又不带 confirm：是 forbidden（第 ④ 步），不是 confirm_required（第 ⑤ 步）。
	if e := run(t, r, token([]v1.Scope{v1.ScopeRead, v1.ScopeOperate}, nil), []string{"demo", "remove"}, []string{"alice"}, ""); e == nil || e.Code != v1.CodeForbidden {
		t.Fatalf("应当 forbidden：%+v", e)
	}
	if c.n != 0 {
		t.Fatal("都不该执行")
	}
}

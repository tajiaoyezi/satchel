package command

import (
	stdctx "context"
	"errors"
	"testing"
	"time"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

func TestFlagParse(t *testing.T) {
	cases := []struct {
		f    Flag
		raw  string
		want any
		bad  bool
	}{
		{Flag{Name: "s", Type: TypeString}, "x", "x", false},
		{Flag{Name: "n", Type: TypeInt}, "42", 42, false},
		{Flag{Name: "n", Type: TypeInt}, "ten", nil, true},
		{Flag{Name: "b", Type: TypeBool}, "", true, false},
		{Flag{Name: "b", Type: TypeBool}, "false", false, false},
		{Flag{Name: "b", Type: TypeBool}, "maybe", nil, true},
		{Flag{Name: "d", Type: TypeDuration}, "5m", 5 * time.Minute, false},
		{Flag{Name: "d", Type: TypeDuration}, "soon", nil, true},
	}
	for _, tc := range cases {
		got, err := tc.f.Parse(tc.raw)
		if tc.bad {
			if err == nil {
				t.Errorf("%s=%q 应当解析失败", tc.f.Name, tc.raw)
			} else if v1.AsError(err).Code != v1.CodeBadRequest {
				t.Errorf("解析失败应当是 bad_request：%v", err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s=%q 得到 %v (%v)，想要 %v", tc.f.Name, tc.raw, got, err, tc.want)
		}
	}
}

func TestFlagFromJSON(t *testing.T) {
	n := Flag{Name: "n", Type: TypeInt}
	if got, err := n.FromJSON(float64(7)); err != nil || got != 7 {
		t.Fatalf("JSON 数字应当收窄成 int：%v %v", got, err)
	}
	if _, err := n.FromJSON(7.5); err == nil {
		t.Fatal("小数不是整数")
	}
	if _, err := n.FromJSON("7"); err == nil {
		t.Fatal("字符串不是整数")
	}
	ss := Flag{Name: "s", Type: TypeStrings}
	if got, err := ss.FromJSON([]any{"a", "b"}); err != nil || len(got.([]string)) != 2 {
		t.Fatalf("字符串数组：%v %v", got, err)
	}
	if _, err := ss.FromJSON([]any{"a", 1}); err == nil {
		t.Fatal("混杂数组应当失败")
	}
	d := Flag{Name: "d", Type: TypeDuration}
	if got, err := d.FromJSON("30s"); err != nil || got != 30*time.Second {
		t.Fatalf("时长从字符串解析：%v %v", got, err)
	}
	b := Flag{Name: "b", Type: TypeBool}
	if _, err := b.FromJSON("true"); err == nil {
		t.Fatal("布尔不接受字符串")
	}
}

func TestPageAndCursor(t *testing.T) {
	p := &Page{}
	if err := p.Normalize(); err != nil || p.Limit != DefaultLimit {
		t.Fatalf("默认 limit 应当是 %d：%v", DefaultLimit, err)
	}
	for _, bad := range []int{-1, 501, 1000} {
		p := &Page{Limit: bad}
		if err := p.Normalize(); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("limit=%d 应当 bad_request", bad)
		}
	}
	if err := (&Page{Limit: 500}).Normalize(); err != nil {
		t.Fatal("500 是上限，合法")
	}
	c := EncodeIDCursor(120)
	if id, err := DecodeIDCursor(c); err != nil || id != 120 {
		t.Fatalf("游标往返：%d %v", id, err)
	}
	if id, err := DecodeIDCursor(""); err != nil || id != 0 {
		t.Fatal("空游标是第一页")
	}
	for _, bad := range []string{"garbage", EncodeIDCursor(0), "aWQ6YWJj"} {
		if _, err := DecodeIDCursor(bad); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("坏游标 %q 应当 bad_request", bad)
		}
	}
}

func TestDispatch(t *testing.T) {
	called := ""
	b := Bindings{"whoami": func(_ stdctx.Context, inv *Invocation) (any, error) {
		called = inv.Name()
		return map[string]any{"ok": true}, nil
	}}
	r := Dispatch(b)
	if _, err := r.Run(stdctx.Background(), &Invocation{Path: []string{"whoami"}}); err != nil || called != "whoami" {
		t.Fatalf("应当调到 whoami：%v", err)
	}
	_, err := r.Run(stdctx.Background(), &Invocation{Path: []string{"nosuch"}})
	var e *v1.Error
	if !errors.As(err, &e) || e.Code != v1.CodeInternal {
		t.Fatalf("没绑定的命令应当是 internal：%v", err)
	}
}

func TestInvocationAccessors(t *testing.T) {
	inv := &Invocation{Path: []string{"audit", "list"}, Args: []string{"x"}, Flags: map[string]any{"n": 3, "s": "v", "b": true, "d": time.Second, "ss": []string{"a"}}}
	if inv.Name() != "audit list" || inv.Int("n", 0) != 3 || inv.String("s", "") != "v" || !inv.Bool("b") || inv.Duration("d", 0) != time.Second || len(inv.Strings("ss")) != 1 {
		t.Fatal("取值器不对")
	}
	if inv.Int("missing", 9) != 9 || inv.String("missing", "d") != "d" || inv.Arg(0) != "x" || inv.Arg(5) != "" {
		t.Fatal("缺省值不对")
	}
}

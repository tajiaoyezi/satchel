package authz

import (
	"testing"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-identity-and-authz「不要身份的命令跳过第 ① 步」。
func TestAnonymousCommands(t *testing.T) {
	tbl, err := command.New(
		&command.Command{Path: []string{"setup", "status"}, Summary: "s", Class: command.ClassRead, Anonymous: true},
		&command.Command{Path: []string{"setup", "init"}, Summary: "s", Class: command.ClassAction, Anonymous: true},
		&command.Command{Path: []string{"whoami"}, Summary: "s", Class: command.ClassRead},
	)
	if err != nil {
		t.Fatal(err)
	}
	c := &calls{}
	r := Wrap(tbl, nil, c.runner())
	for _, path := range [][]string{{"setup", "status"}, {"setup", "init"}} {
		if e := run(t, r, v1.Anonymous(), path, nil, ""); e != nil {
			t.Fatalf("anonymous 调 %v 应当执行：%+v", path, e)
		}
	}
	if e := run(t, r, v1.Anonymous(), []string{"whoami"}, nil, ""); e == nil || e.Code != v1.CodeUnauthenticated {
		t.Fatalf("没标不要身份的命令仍要身份：%+v", e)
	}
	// 有身份的也能调不要身份的命令。
	if e := run(t, r, v1.LocalAdmin("root"), []string{"setup", "status"}, nil, ""); e != nil {
		t.Fatalf("本机管理员调 setup status：%+v", e)
	}
	if c.n != 3 {
		t.Fatalf("应当执行 3 次，得到 %d", c.n)
	}
}

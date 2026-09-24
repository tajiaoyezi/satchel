package audit

import (
	"context"
	"strings"
	"testing"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// master-audit-log：不要身份的命令执行后照记（actor_kind anonymous、actor 空）；password 类型的 flag 打码。
func TestAnonymousAllowedCommandsAreRecorded(t *testing.T) {
	tbl, err := command.New(
		&command.Command{Path: []string{"setup", "init"}, Summary: "s", Class: command.ClassAction, Anonymous: true,
			Flags: []command.Flag{{Name: "username", Type: command.TypeString}, {Name: "password", Type: command.TypePassword}}},
		&command.Command{Path: []string{"whoami"}, Summary: "s", Class: command.ClassRead},
	)
	if err != nil {
		t.Fatal(err)
	}
	rec := &memRecorder{}
	r := Wrap(rec, tbl, nil, next(map[string]any{"ok": true}, nil))
	inv := &command.Invocation{Path: []string{"setup", "init"}, Flags: map[string]any{"username": "admin", "password": "hunter2 long"}}
	if _, err := r.Run(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	if len(rec.entries) != 1 {
		t.Fatalf("不要身份的命令应当记 1 条，得到 %d", len(rec.entries))
	}
	e := rec.entries[0]
	if e.ActorKind != v1.ActorAnonymous || e.Actor != "" || e.Command != "setup init" {
		t.Fatalf("记录不对：%+v", e)
	}
	if strings.Contains(e.ArgsDigest, "hunter2") || !strings.Contains(e.ArgsDigest, `"password":"***"`) || !strings.Contains(e.ArgsDigest, `"username":"admin"`) {
		t.Fatalf("password 类型应当打码：%s", e.ArgsDigest)
	}
	// anonymous 调普通命令（会被 authz 拒）仍不记。
	if _, _ = r.Run(context.Background(), &command.Invocation{Path: []string{"whoami"}}); len(rec.entries) != 1 {
		t.Fatalf("anonymous 调普通命令不该记，得到 %d 条", len(rec.entries))
	}
	// verify 值永不进摘要。
	d := Digest(nil, &command.Invocation{Path: []string{"x"}, Verify: &command.Verification{Password: "pw-secret", Code: "123456", User: "admin"}})
	if strings.Contains(d, "pw-secret") || strings.Contains(d, "123456") || strings.Contains(d, "verify") {
		t.Fatalf("当场验证的值不该进摘要：%s", d)
	}
}

// master-audit-log：带无效凭据的请求不记（连不要身份的命令也不记）；令牌身份的记录带 token_id 与签发者。
func TestInvalidCredentialAndTokenRecords(t *testing.T) {
	tbl, err := command.New(
		&command.Command{Path: []string{"setup", "status"}, Summary: "s", Class: command.ClassRead, Anonymous: true},
		&command.Command{Path: []string{"whoami"}, Summary: "s", Class: command.ClassRead},
	)
	if err != nil {
		t.Fatal(err)
	}
	rec := &memRecorder{}
	r := Wrap(rec, tbl, nil, next(nil, v1.New(v1.CodeUnauthenticated, "invalid")))
	invalid := v1.WithCredentialSource(v1.WithIdentity(context.Background(), v1.Anonymous()), v1.SourceInvalid)
	for _, path := range [][]string{{"setup", "status"}, {"whoami"}} {
		_, _ = r.Run(invalid, &command.Invocation{Path: path})
	}
	if len(rec.entries) != 0 {
		t.Fatalf("无效凭据不该记，得到 %+v", rec.entries)
	}
	tokenID := int64(12)
	tok := v1.WithCredentialSource(v1.WithIdentity(context.Background(),
		v1.Identity{Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin, TokenID: &tokenID, Scopes: []v1.Scope{v1.ScopeRead}}), v1.SourceToken)
	ok := Wrap(rec, tbl, nil, next(map[string]any{"ok": true}, nil))
	if _, err := ok.Run(tok, &command.Invocation{Path: []string{"whoami"}}); err != nil {
		t.Fatal(err)
	}
	if len(rec.entries) != 1 || rec.entries[0].TokenID == nil || *rec.entries[0].TokenID != 12 || rec.entries[0].Actor != "admin" || rec.entries[0].ActorKind != v1.ActorToken {
		t.Fatalf("令牌身份的记录：%+v", rec.entries)
	}
}

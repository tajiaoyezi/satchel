package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/core/sessions"
	"github.com/satchel/satchel/internal/core/users"
	"github.com/satchel/satchel/internal/service/auth"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// runWith 用给定选项执行一次根命令。
func runWith(t *testing.T, opts Options, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Execute(opts, args, &out, &errOut)
	return out.String(), errOut.String(), code
}

// fakePrompt 按顺序吐出预设的答案，并记下每次的提示语。
type fakePrompt struct {
	answers []string
	labels  []string
}

func (f *fakePrompt) prompt(label string) (string, error) {
	f.labels = append(f.labels, label)
	if len(f.answers) == 0 {
		return "", ErrNoTerminal
	}
	a := f.answers[0]
	f.answers = f.answers[1:]
	return a, nil
}

// capture 记下发给主控的 Invocation。
type capture struct{ last *command.Invocation }

func (c *capture) Run(_ context.Context, inv *command.Invocation) (any, error) {
	c.last = inv
	return map[string]any{"ok": true}, nil
}

func promptOptions(fp *fakePrompt, cap *capture) Options {
	opts := testOptions()
	opts.Prompt = fp.prompt
	opts.Remote = func(string) command.Runner { return cap }
	return opts
}

// master-cli「password 类型的 flag 不作为命令行参数存在」：给了就是未知 flag（usage，退出码 2），不发请求。
func TestPasswordFlagsAreNotArguments(t *testing.T) {
	cap := &capture{}
	opts := promptOptions(&fakePrompt{}, cap)
	for _, args := range [][]string{
		{"account", "set-password", "--verify-password", "x"},
		{"account", "set-password", "--new-password", "x"},
		{"setup", "init", "--username", "admin", "--password", "x"},
		{"account", "totp", "setup", "--verify-password=x"},
	} {
		_, stderr, code := runWith(t, opts, args...)
		if code != v1.ExitUsage || !strings.Contains(stderr, "unknown flag") {
			t.Errorf("%v 应当是 usage 错误：%d\n%s", args, code, stderr)
		}
		if cap.last != nil {
			t.Fatalf("%v 不该发请求", args)
		}
	}
}

// master-human-verification「CLI 从终端读当场验证的值」与 master-cli「新密码读两遍」。
func TestPromptedValuesReachTheRequest(t *testing.T) {
	// 人类专属 + 新密码：新密码两遍、当场验证的密码、验证码（没给 --verify-code 就问）。
	fp := &fakePrompt{answers: []string{"n3wpass!!", "n3wpass!!", "current-pw", ""}}
	cap := &capture{}
	if _, stderr, code := runWith(t, promptOptions(fp, cap), "account", "set-password"); code != 0 {
		t.Fatalf("set-password 应当成功：%d\n%s", code, stderr)
	}
	if len(fp.labels) != 4 || !strings.Contains(fp.labels[1], "再输入一次") || !strings.Contains(fp.labels[2], "密码") || !strings.Contains(fp.labels[3], "验证码") {
		t.Fatalf("提示顺序不对：%q", fp.labels)
	}
	inv := cap.last
	if inv.Flags["new-password"] != "n3wpass!!" || inv.Verify == nil || inv.Verify.Password != "current-pw" || inv.Verify.Code != "" || inv.Verify.User != "" {
		t.Fatalf("请求内容：flags=%v verify=%+v", inv.Flags, inv.Verify)
	}
	// 给了 --verify-code 与 --verify-user 就不再问验证码；它们不进 Flags。
	fp = &fakePrompt{answers: []string{"n3wpass!!", "n3wpass!!", "current-pw"}}
	cap = &capture{}
	if _, stderr, code := runWith(t, promptOptions(fp, cap), "account", "set-password", "--verify-code", "123456", "--verify-user", "admin"); code != 0 {
		t.Fatalf("带 verify-code 的 set-password 应当成功：%d\n%s", code, stderr)
	}
	if len(fp.labels) != 3 || cap.last.Verify.Code != "123456" || cap.last.Verify.User != "admin" || len(cap.last.Flags) != 1 {
		t.Fatalf("带参数的当场验证：labels=%q flags=%v verify=%+v", fp.labels, cap.last.Flags, cap.last.Verify)
	}
	// 人类专属但没有新密码的命令：只问当场验证。
	fp = &fakePrompt{answers: []string{"current-pw", "12345678"}}
	cap = &capture{}
	if _, _, code := runWith(t, promptOptions(fp, cap), "account", "totp", "disable"); code != 0 || len(fp.labels) != 2 || cap.last.Verify.Code != "12345678" {
		t.Fatalf("totp disable 的当场验证：%d %q %+v", code, fp.labels, cap.last.Verify)
	}
	// 两遍不一致：bad_request，不发请求。
	fp = &fakePrompt{answers: []string{"aaaaaaaa", "bbbbbbbb"}}
	cap = &capture{}
	_, stderr, code := runWith(t, promptOptions(fp, cap), "account", "set-password", "--json")
	if code != v1.ExitFailure || decodeError(t, stderr).Code != v1.CodeBadRequest || cap.last != nil {
		t.Fatalf("两遍不一致应当 bad_request 且不发请求：%d %s", code, stderr)
	}
	// setup init：密码两遍进 flags；不是人类专属，不问当场验证。
	fp = &fakePrompt{answers: []string{"secret12", "secret12"}}
	cap = &capture{}
	if _, stderr, code := runWith(t, promptOptions(fp, cap), "setup", "init", "--username", "admin"); code != 0 {
		t.Fatalf("setup init：%d\n%s", code, stderr)
	}
	if len(fp.labels) != 2 || cap.last.Flags["password"] != "secret12" || cap.last.Flags["username"] != "admin" || cap.last.Verify != nil {
		t.Fatalf("setup init 的请求：labels=%q flags=%v", fp.labels, cap.last.Flags)
	}
	// 不要密码、不是人类专属的命令一次也不问。
	fp = &fakePrompt{}
	cap = &capture{}
	if _, _, code := runWith(t, promptOptions(fp, cap), "account", "show"); code != 0 || len(fp.labels) != 0 {
		t.Fatalf("account show 不该提示：%d %q", code, fp.labels)
	}
}

// master-human-verification「没有终端」：人类专属 human_required（退出码 9）、其它 bad_request；立即返回、不等待、不发请求。
func TestNoTerminalRefusesWithoutBlocking(t *testing.T) {
	cap := &capture{}
	opts := promptOptions(&fakePrompt{}, cap) // 没有答案 → ErrNoTerminal
	_, stderr, code := runWith(t, opts, "account", "set-password", "--json")
	if code != v1.ExitHumanRequired || decodeError(t, stderr).Code != v1.CodeHumanRequired || !strings.Contains(stderr, "没有终端") {
		t.Fatalf("人类专属命令没终端应当 human_required：%d %s", code, stderr)
	}
	_, stderr, code = runWith(t, opts, "setup", "init", "--username", "admin", "--json")
	if code != v1.ExitFailure || decodeError(t, stderr).Code != v1.CodeBadRequest || !strings.Contains(stderr, "没有终端") {
		t.Fatalf("setup init 没终端应当 bad_request：%d %s", code, stderr)
	}
	if cap.last != nil {
		t.Fatal("没终端时不该发请求")
	}
	// 默认的终端读取：go test 的 stdin 不是终端，必须立刻拒绝而不是挂着等输入。
	done := make(chan int, 1)
	go func() {
		_, _, code := run(t, "setup", "init", "--username", "admin", "--json")
		done <- code
	}()
	select {
	case code := <-done:
		if code != v1.ExitFailure {
			t.Fatalf("默认终端读取在没有终端时应当失败退出，得到 %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没有终端时命令不该阻塞等待输入")
	}
}

// seedAccount 直接插一行账号，再给它两个会话。
func seedAccount(t *testing.T, bdb *bun.DB, username, role string) *users.Account {
	t.Helper()
	hash, _ := auth.HashPassword("old-password")
	now := time.Now().UTC()
	u := &model.User{Username: username, Role: role, IsActive: true, PasswordHash: hash, TotpSecret: "KEEPME", TotpEnabled: true,
		RecoveryCodes: json.RawMessage(`["h1","h2"]`), NodeSpeedLimitOverrides: json.RawMessage(`{}`), NodeDeviceLimitOverrides: json.RawMessage(`{}`),
		CreatedAt: now, UpdatedAt: now, ResourceVersion: 1}
	if _, err := bdb.NewInsert().Model(u).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
	repo := sessions.New(bdb)
	for _, h := range []string{"hash-" + username + "-1", "hash-" + username + "-2"} {
		if err := repo.Insert(context.Background(), sessions.Session{TokenHash: h, Username: username, ExpiresAt: now.Add(time.Hour), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	a, err := users.New(bdb, store.New(bdb, schema.Default())).GetByUsername(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// master-accounts「主控本机重置管理员密码」的本体，双库：改哈希、作废全部会话、不动两步验证；非管理员与不存在的账号被拒。
func TestResetAdminPasswordDualDB(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		ctx := context.Background()
		seedAccount(t, bdb, "admin", "admin")
		seedAccount(t, bdb, "bob", "user")
		revoked, err := ResetAdminPassword(ctx, bdb, "admin", "brand-new-pw")
		if err != nil || revoked != 2 {
			t.Fatalf("重置：%d %v", revoked, err)
		}
		a, _ := users.New(bdb, store.New(bdb, schema.Default())).GetByUsername(ctx, "admin")
		if !auth.CheckPassword(a.PasswordHash, "brand-new-pw") || auth.CheckPassword(a.PasswordHash, "old-password") {
			t.Fatal("新密码应当生效、旧密码失效")
		}
		if !a.TOTPEnabled || a.TOTPSecret != "KEEPME" || len(a.RecoveryCodes) != 2 {
			t.Fatalf("两步验证不该被动：%+v", a)
		}
		if n, _ := sessions.New(bdb).CountByUser(ctx, "admin", time.Now()); n != 0 {
			t.Fatalf("会话应当全部作废，剩 %d", n)
		}
		if n, _ := sessions.New(bdb).CountByUser(ctx, "bob", time.Now()); n != 2 {
			t.Fatalf("别人的会话不该动，剩 %d", n)
		}
		if _, err := ResetAdminPassword(ctx, bdb, "bob", "brand-new-pw"); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("普通用户应当 bad_request：%v", err)
		}
		if _, err := ResetAdminPassword(ctx, bdb, "nobody", "brand-new-pw"); err == nil || v1.AsError(err).Code != v1.CodeNotFound {
			t.Fatalf("不存在应当 not_found：%v", err)
		}
		if _, err := ResetAdminPassword(ctx, bdb, "admin", "short"); err == nil || v1.AsError(err).Code != v1.CodeBadRequest {
			t.Fatalf("太短的密码应当 bad_request：%v", err)
		}
	})
}

// satchel admin reset-password 经真实 CLI（SQLite 数据目录）：confirm、账号检查、终端读两遍、stderr 提示、输出。
func TestAdminResetPasswordCommand(t *testing.T) {
	sqliteOnly(t)
	dir := filepath.Join(t.TempDir(), "data")
	if _, stderr, code := run(t, "db", "migrate", "--data-dir", dir); code != 0 {
		t.Fatal(stderr)
	}
	bdb, err := OpenForWrite(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	seedAccount(t, bdb, "admin", "admin")
	seedAccount(t, bdb, "bob", "user")
	bdb.Close()

	fp := &fakePrompt{}
	opts := testOptions()
	opts.Prompt = fp.prompt
	// 缺 confirm：428 口径的 confirm_required，state 给出该填的值；还没到问密码那一步。
	_, stderr, code := runWith(t, opts, "admin", "reset-password", "admin", "--data-dir", dir, "--json")
	if e := decodeError(t, stderr); code != v1.ExitConfirmRequired || e.Code != v1.CodeConfirmRequired || e.State["expected"] != "admin" || len(fp.labels) != 0 {
		t.Fatalf("缺 confirm：%d %s %q", code, stderr, fp.labels)
	}
	// confirm 填错也是缺 confirm。
	if _, stderr, code = runWith(t, opts, "admin", "reset-password", "admin", "--confirm", "root", "--data-dir", dir, "--json"); code != v1.ExitConfirmRequired {
		t.Fatalf("confirm 不等于用户名：%d %s", code, stderr)
	}
	// 非管理员、不存在：都在问密码之前拒绝。
	_, stderr, code = runWith(t, opts, "admin", "reset-password", "bob", "--confirm", "bob", "--data-dir", dir, "--json")
	if code != v1.ExitFailure || decodeError(t, stderr).Code != v1.CodeBadRequest || len(fp.labels) != 0 {
		t.Fatalf("非管理员：%d %s %q", code, stderr, fp.labels)
	}
	_, stderr, code = runWith(t, opts, "admin", "reset-password", "nobody", "--confirm", "nobody", "--data-dir", dir, "--json")
	if code != v1.ExitNotFound || decodeError(t, stderr).Code != v1.CodeNotFound {
		t.Fatalf("不存在：%d %s", code, stderr)
	}
	// 没有终端：bad_request。
	_, stderr, code = runWith(t, opts, "admin", "reset-password", "admin", "--confirm", "admin", "--data-dir", dir, "--json")
	if code != v1.ExitFailure || decodeError(t, stderr).Code != v1.CodeBadRequest || !strings.Contains(stderr, "没有终端") {
		t.Fatalf("没终端：%d %s", code, stderr)
	}
	// 两遍不一致。
	fp.answers = []string{"aaaaaaaa", "bbbbbbbb"}
	if _, stderr, code = runWith(t, opts, "admin", "reset-password", "admin", "--confirm", "admin", "--data-dir", dir, "--json"); code != v1.ExitFailure || !strings.Contains(stderr, "不一致") {
		t.Fatalf("两遍不一致：%d %s", code, stderr)
	}
	// 成功：文本输出、stderr 提示未进审计、库里生效。
	fp.answers = []string{"brand-new-pw", "brand-new-pw"}
	stdout, stderr, code := runWith(t, opts, "admin", "reset-password", "admin", "--confirm", "admin", "--data-dir", dir)
	if code != 0 || !strings.Contains(stdout, "已重置 admin 的密码，作废了 2 个会话") || !strings.Contains(stderr, "未进审计") {
		t.Fatalf("成功：%d\n%s\n%s", code, stdout, stderr)
	}
	bdb, _ = OpenForWrite(context.Background(), dir)
	defer bdb.Close()
	a, _ := users.New(bdb, store.New(bdb, schema.Default())).GetByUsername(context.Background(), "admin")
	if !auth.CheckPassword(a.PasswordHash, "brand-new-pw") || !a.TOTPEnabled {
		t.Fatalf("库里应当是新密码、两步验证不动：%+v", a)
	}
	if n, _ := sessions.New(bdb).CountByUser(context.Background(), "admin", time.Now()); n != 0 {
		t.Fatalf("会话应当全部作废，剩 %d", n)
	}
	// JSON 输出形状。
	fp.answers = []string{"brand-new-pw2", "brand-new-pw2"}
	stdout, _, code = runWith(t, opts, "admin", "reset-password", "admin", "--confirm", "admin", "--data-dir", dir, "--json")
	if code != 0 {
		t.Fatal(stdout)
	}
	got := decodeJSONObject(t, stdout)
	if string(got["username"]) != `"admin"` || string(got["sessions_revoked"]) != "0" {
		t.Fatalf("JSON 输出：%s", stdout)
	}
}

package tokens

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/schema"
	"github.com/satchel/satchel/internal/base/store"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/tokens"
	"github.com/satchel/satchel/internal/core/users"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

type fixture struct {
	t   *testing.T
	bdb *bun.DB
	s   *Service
}

func newFixture(t *testing.T, bdb *bun.DB) *fixture {
	st := store.New(bdb, schema.Default())
	f := &fixture{t: t, bdb: bdb, s: New(core.New(bdb, st), users.New(bdb, st), nil)}
	f.user("admin", v1.RoleAdmin)
	f.user("bob", v1.RoleUser)
	return f
}

func (f *fixture) user(name string, role v1.Role) {
	f.t.Helper()
	u := &model.User{Username: name, Role: string(role), IsActive: true, PasswordHash: "h", RecoveryCodes: json.RawMessage(`[]`),
		NodeSpeedLimitOverrides: json.RawMessage(`{}`), NodeDeviceLimitOverrides: json.RawMessage(`{}`), CreatedAt: time.Now(), UpdatedAt: time.Now(), ResourceVersion: 1}
	if _, err := f.bdb.NewInsert().Model(u).Exec(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

// setUser 直接改账号的列（M3 之前没有用户管理命令）：停用、降级、软删除。
func (f *fixture) setUser(name, column string, value any) {
	f.t.Helper()
	if _, err := f.bdb.NewUpdate().Model((*model.User)(nil)).Set("? = ?", bun.Ident(column), value).Where("username = ?", name).Exec(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

// advance 把服务的时钟拨快。
func (f *fixture) advance(d time.Duration) {
	base := f.s.now()
	f.s.now = func() time.Time { return base.Add(d) }
}

func as(id v1.Identity) context.Context { return v1.WithIdentity(context.Background(), id) }

func admin() context.Context {
	return as(v1.Identity{Actor: "admin", ActorKind: v1.ActorUser, Role: v1.RoleAdmin, Scopes: v1.AllScopes, Danger: v1.AllDangers})
}

func bob() context.Context {
	return as(v1.Identity{Actor: "bob", ActorKind: v1.ActorUser, Role: v1.RoleUser, Scopes: readOp, Danger: []v1.Danger{}})
}

// run 调一条命令：name 是命令名，args 是位置参数，flags 是已解析的 flag 值。
func (f *fixture) run(ctx context.Context, name string, args []string, flags map[string]any, verify *command.Verification) (any, error) {
	if flags == nil {
		flags = map[string]any{}
	}
	inv := &command.Invocation{Path: strings.Fields(name), Args: args, Flags: flags, Verify: verify}
	if name == "token list" || name == "mcp status" {
		inv.Page = &command.Page{}
		if limit, ok := flags["limit"].(int); ok {
			inv.Page.Limit = limit
			delete(flags, "limit")
		}
		if cursor, ok := flags["cursor"].(string); ok {
			inv.Page.Cursor = cursor
			delete(flags, "cursor")
		}
	}
	return f.s.Bindings()[name](ctx, inv)
}

func (f *fixture) create(ctx context.Context, flags map[string]any) Created {
	f.t.Helper()
	out, err := f.run(ctx, "token create", nil, flags, nil)
	if err != nil {
		f.t.Fatalf("签发 %v 失败：%v", flags, err)
	}
	return out.(Created)
}

func (f *fixture) list(ctx context.Context, name string, flags map[string]any) *command.PageResult {
	f.t.Helper()
	out, err := f.run(ctx, name, nil, flags, nil)
	if err != nil {
		f.t.Fatalf("%s 失败：%v", name, err)
	}
	return out.(*command.PageResult)
}

func ids(res *command.PageResult) []int64 {
	var out []int64
	for _, it := range res.Items {
		out = append(out, it.(Info).ID)
	}
	return out
}

func wantErr(t *testing.T, err error, code v1.Code, contains ...string) {
	t.Helper()
	e := v1.AsError(err)
	if err == nil || e.Code != code {
		t.Fatalf("想要 %s，得到 %v", code, err)
	}
	for _, c := range contains {
		if !strings.Contains(e.Reason, c) {
			t.Fatalf("reason 应当提到 %q：%s", c, e.Reason)
		}
	}
}

func (f *fixture) count() int {
	f.t.Helper()
	n, err := f.bdb.NewSelect().Model((*model.ApiToken)(nil)).Count(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return n
}

// master-api-tokens「令牌的形状与存储」「权限范围与预设」「签发者与权限上限」的服务层部分。
func TestCreate(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := newFixture(t, bdb)
		ctx := context.Background()

		ci := f.create(admin(), map[string]any{"name": "  ci  "})
		if ci.Name != "ci" || ci.Owner != "admin" || ci.Preset != PresetReadonly || !reflect.DeepEqual(ci.Scopes, readOnly) ||
			ci.Danger == nil || len(ci.Danger) != 0 || ci.ExpiresAt != nil || ci.State != "active" || ci.Runtime != "" {
			t.Fatalf("默认应当只读、不过期：%+v", ci)
		}
		if !strings.HasPrefix(ci.Token, Prefix) || len(ci.Token) != 47 {
			t.Fatalf("明文形状不对：%q", ci.Token)
		}
		var row model.ApiToken
		if err := bdb.NewSelect().Model(&row).Where("id = ?", ci.ID).Scan(ctx); err != nil {
			t.Fatal(err)
		}
		if row.TokenHash != Hash(ci.Token) {
			t.Fatalf("库里应当是明文的 SHA-256：%s", row.TokenHash)
		}
		raw, _ := json.Marshal(row)
		if strings.Contains(string(raw), ci.Token) {
			t.Fatal("库里任何一列都不能含明文")
		}

		bot := f.create(admin(), map[string]any{"name": "bot", "preset": "ops", "danger": []string{"delete"}, "runtime": "claude-code@laptop"})
		if bot.Preset != PresetOps || !reflect.DeepEqual(bot.Scopes, readOp) || !reflect.DeepEqual(bot.Danger, []v1.Danger{v1.DangerDelete}) || bot.Runtime != "claude-code@laptop" {
			t.Fatalf("ops 加一个危险类：%+v", bot)
		}
		root := f.create(admin(), map[string]any{"name": "root-bot", "danger": []string{"delete", "restart", "permission", "batch", "exec", "master"}})
		if root.Preset != PresetFull {
			t.Fatalf("六类全开就是全权：%+v", root)
		}

		before := f.count()
		_, err := f.run(admin(), "token create", nil, map[string]any{"name": "x", "preset": "readonly", "danger": []string{"delete"}}, nil)
		wantErr(t, err, v1.CodeBadRequest)
		// 普通用户签全权被拒并点名危险类，签 ops 成功；带 secrets 也被拒。
		_, err = f.run(bob(), "token create", nil, map[string]any{"name": "x", "preset": "full"}, nil)
		wantErr(t, err, v1.CodeForbidden, "delete")
		_, err = f.run(bob(), "token create", nil, map[string]any{"name": "x", "secrets": true}, nil)
		wantErr(t, err, v1.CodeForbidden, "secrets")
		for _, bad := range []map[string]any{
			{"name": "   "},
			{"name": strings.Repeat("名", 65)},
			{"name": "x", "runtime": "has space"},
			{"name": "x", "runtime": strings.Repeat("a", 65)},
			{"name": "x", "expires-in": -time.Second},
			{"name": "x", "preset": "root"},
		} {
			_, err := f.run(admin(), "token create", nil, bad, nil)
			wantErr(t, err, v1.CodeBadRequest)
		}
		if f.count() != before {
			t.Fatal("被拒的签发不能留下任何令牌")
		}
		if mine := f.create(bob(), map[string]any{"name": "x", "preset": "ops"}); mine.Owner != "bob" || mine.Preset != PresetOps {
			t.Fatalf("普通用户签 ops：%+v", mine)
		}
		if long := f.create(admin(), map[string]any{"name": strings.Repeat("名", 64), "expires-in": time.Duration(0)}); long.ExpiresAt != nil {
			t.Fatal("expires-in 0 表示不过期")
		}
		exp := f.create(admin(), map[string]any{"name": "h", "expires-in": time.Hour})
		if exp.ExpiresAt == nil || exp.ExpiresAt.Sub(exp.CreatedAt) < 59*time.Minute || exp.ExpiresAt.Sub(exp.CreatedAt) > 61*time.Minute {
			t.Fatalf("expires-in 1h：%+v", exp)
		}

		// 本机管理员：令牌挂在 verify-user 指明的管理员名下；没指明是 human_required。
		local := as(v1.LocalAdmin("root"))
		out, err := f.run(local, "token create", nil, map[string]any{"name": "hermes", "preset": "ops"}, &command.Verification{Password: "p", User: "admin"})
		if err != nil || out.(Created).Owner != "admin" {
			t.Fatalf("本机管理员签发：%+v %v", out, err)
		}
		_, err = f.run(local, "token create", nil, map[string]any{"name": "x"}, nil)
		wantErr(t, err, v1.CodeHumanRequired)
		f.setUser("admin", "is_active", false)
		_, err = f.run(local, "token create", nil, map[string]any{"name": "x"}, &command.Verification{User: "admin"})
		wantErr(t, err, v1.CodeForbidden)
		f.setUser("admin", "is_active", true)
		// 令牌身份到不了这里（authz 先拦），处理函数自己也不认。
		_, err = f.run(as(v1.Identity{Actor: "admin", ActorKind: v1.ActorToken, Role: v1.RoleAdmin}), "token create", nil, map[string]any{"name": "x"}, nil)
		wantErr(t, err, v1.CodeHumanRequired)
	})
}

// token list：可见范围、--owner、keyset 分页、state；输出里没有明文与哈希。
func TestList(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := newFixture(t, bdb)
		a1 := f.create(admin(), map[string]any{"name": "a1"})
		a2 := f.create(admin(), map[string]any{"name": "a2", "expires-in": time.Hour})
		a3 := f.create(admin(), map[string]any{"name": "a3"})
		b1 := f.create(bob(), map[string]any{"name": "b1"})
		b2 := f.create(bob(), map[string]any{"name": "b2"})
		if _, err := f.run(admin(), "token revoke", []string{itoa(a3.ID)}, nil, nil); err != nil {
			t.Fatal(err)
		}
		f.advance(2 * time.Hour)

		all := f.list(admin(), "token list", nil)
		if all.Total != 5 || !reflect.DeepEqual(ids(all), []int64{b2.ID, b1.ID, a3.ID, a2.ID, a1.ID}) {
			t.Fatalf("管理员看全部、按 id 倒序：%v total=%d", ids(all), all.Total)
		}
		states := map[int64]string{}
		for _, it := range all.Items {
			states[it.(Info).ID] = it.(Info).State
		}
		if states[a1.ID] != "active" || states[a2.ID] != "expired" || states[a3.ID] != "revoked" {
			t.Fatalf("state 不对：%v", states)
		}
		raw, _ := json.Marshal(all)
		for _, secret := range []string{a1.Token, Hash(a1.Token), "token_hash", Prefix} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("列表里不能出现 %q", secret)
			}
		}
		if got := f.list(admin(), "token list", map[string]any{"owner": "bob"}); !reflect.DeepEqual(ids(got), []int64{b2.ID, b1.ID}) || got.Total != 2 {
			t.Fatalf("--owner bob：%v", ids(got))
		}
		if got := f.list(bob(), "token list", nil); !reflect.DeepEqual(ids(got), []int64{b2.ID, b1.ID}) || got.Total != 2 {
			t.Fatalf("普通用户只看自己的：%v", ids(got))
		}
		if got := f.list(bob(), "token list", map[string]any{"owner": "admin"}); len(got.Items) != 0 || got.Total != 0 || got.Items == nil {
			t.Fatalf("普通用户指定别人是空列表：%+v", got)
		}
		// 本机管理员不是账号，但角色是管理员：看全部。
		if got := f.list(as(v1.LocalAdmin("root")), "token list", nil); got.Total != 5 {
			t.Fatalf("本机管理员看全部：%d", got.Total)
		}
		p1 := f.list(admin(), "token list", map[string]any{"limit": 2})
		p2 := f.list(admin(), "token list", map[string]any{"limit": 2, "cursor": p1.NextCursor})
		p3 := f.list(admin(), "token list", map[string]any{"limit": 2, "cursor": p2.NextCursor})
		if !reflect.DeepEqual(append(append(ids(p1), ids(p2)...), ids(p3)...), ids(all)) || p3.NextCursor != "" || p1.NextCursor == "" {
			t.Fatalf("keyset 分页：%v %v %v", ids(p1), ids(p2), ids(p3))
		}
		_, err := f.run(admin(), "token list", nil, map[string]any{"cursor": "garbage"}, nil)
		wantErr(t, err, v1.CodeBadRequest)
	})
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// master-api-tokens「token 命令」：改权限立刻生效、令牌字符串不变；吊销；别人的令牌 not_found；上限跟着签发者走。
func TestUpdateAndRevoke(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := newFixture(t, bdb)
		ctx := context.Background()
		tok := f.create(admin(), map[string]any{"name": "ro"})
		id := itoa(tok.ID)
		if got, ok := f.s.Resolve(ctx, tok.Token); !ok || !reflect.DeepEqual(got.Scopes, readOnly) {
			t.Fatalf("只读令牌：%+v %v", got, ok)
		}
		_, err := f.run(admin(), "token update", []string{id}, nil, nil)
		wantErr(t, err, v1.CodeBadRequest)
		out, err := f.run(admin(), "token update", []string{id}, map[string]any{"preset": "ops"}, nil)
		if err != nil || out.(Info).Preset != PresetOps {
			t.Fatalf("改成 ops：%+v %v", out, err)
		}
		if got, ok := f.s.Resolve(ctx, tok.Token); !ok || !reflect.DeepEqual(got.Scopes, readOp) {
			t.Fatalf("改权限应当立刻生效、令牌字符串不变：%+v %v", got, ok)
		}
		out, err = f.run(admin(), "token update", []string{id}, map[string]any{"name": "renamed", "expires-in": time.Hour, "secrets": true}, nil)
		if info := out.(Info); err != nil || info.Name != "renamed" || info.ExpiresAt == nil || !reflect.DeepEqual(info.Scopes, []v1.Scope{v1.ScopeRead, v1.ScopeOperate, v1.ScopeSecrets}) {
			t.Fatalf("改名字、过期时间、secrets：%+v %v", out, err)
		}
		out, err = f.run(admin(), "token update", []string{id}, map[string]any{"expires-in": time.Duration(0)}, nil)
		if err != nil || out.(Info).ExpiresAt != nil {
			t.Fatalf("expires-in 0 改回不过期：%+v %v", out, err)
		}
		for _, bad := range []map[string]any{{"name": ""}, {"preset": "readonly", "danger": []string{"exec"}}, {"expires-in": -time.Minute}} {
			_, err := f.run(admin(), "token update", []string{id}, bad, nil)
			wantErr(t, err, v1.CodeBadRequest)
		}

		// 普通用户动不了别人的令牌，也不知道它存不存在。
		for _, name := range []string{"token update", "token revoke"} {
			_, err := f.run(bob(), name, []string{id}, map[string]any{"name": "mine"}, nil)
			wantErr(t, err, v1.CodeNotFound)
			_, err = f.run(bob(), name, []string{"999999"}, map[string]any{"name": "mine"}, nil)
			wantErr(t, err, v1.CodeNotFound)
		}
		if _, ok := f.s.Resolve(ctx, tok.Token); !ok {
			t.Fatal("bob 的吊销被拒后令牌仍可用")
		}
		// 上限跟着签发者：bob 的令牌谁来改都不能超出 read 与 operate。
		mine := f.create(bob(), map[string]any{"name": "b", "preset": "ops"})
		for _, ctx := range []context.Context{bob(), admin()} {
			_, err := f.run(ctx, "token update", []string{itoa(mine.ID)}, map[string]any{"preset": "full"}, nil)
			wantErr(t, err, v1.CodeForbidden)
		}
		if out, err := f.run(bob(), "token update", []string{itoa(mine.ID)}, map[string]any{"preset": "readonly"}, nil); err != nil || out.(Info).Preset != PresetReadonly {
			t.Fatalf("bob 改自己的令牌：%+v %v", out, err)
		}

		out, err = f.run(admin(), "token revoke", []string{id}, nil, nil)
		if info := out.(Info); err != nil || info.State != "revoked" || info.RevokedAt == nil {
			t.Fatalf("吊销：%+v %v", out, err)
		}
		if _, ok := f.s.Resolve(ctx, tok.Token); ok {
			t.Fatal("吊销后立刻失效")
		}
		_, err = f.run(admin(), "token revoke", []string{id}, nil, nil)
		wantErr(t, err, v1.CodeBadRequest)
		_, err = f.run(admin(), "token update", []string{id}, map[string]any{"name": "again"}, nil)
		wantErr(t, err, v1.CodeBadRequest)
		for _, bad := range []string{"abc", "0", "-1", ""} {
			_, err := f.run(admin(), "token revoke", []string{bad}, nil, nil)
			wantErr(t, err, v1.CodeBadRequest)
		}
	})
}

// mcp status：只列有 runtime 标签的，按最后使用时间倒序，从没用过的排最后；偏移量翻页；可见范围同 token list。
func TestMcpStatus(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := newFixture(t, bdb)
		ctx := context.Background()
		a := f.create(admin(), map[string]any{"name": "a", "runtime": "claude-code@a"})
		b := f.create(admin(), map[string]any{"name": "b", "runtime": "codex@b"})
		f.create(admin(), map[string]any{"name": "plain"})
		d := f.create(bob(), map[string]any{"name": "d", "runtime": "hermes@d"})
		if _, ok := f.s.Resolve(ctx, a.Token); !ok {
			t.Fatal("a 应当有效")
		}
		f.advance(2 * time.Minute)
		if _, ok := f.s.Resolve(ctx, b.Token); !ok {
			t.Fatal("b 应当有效")
		}
		all := f.list(admin(), "mcp status", nil)
		if all.Total != 3 || !reflect.DeepEqual(ids(all), []int64{b.ID, a.ID, d.ID}) {
			t.Fatalf("按最后使用时间倒序、没用过的排最后、没有 runtime 的不列：%v total=%d", ids(all), all.Total)
		}
		if got := all.Items[0].(Info); got.LastUsedAt == nil || got.Runtime != "codex@b" {
			t.Fatalf("mcp status 的条目：%+v", got)
		}
		var paged []int64
		cursor := ""
		for i := 0; i < 5; i++ {
			p := f.list(admin(), "mcp status", map[string]any{"limit": 1, "cursor": cursor})
			paged = append(paged, ids(p)...)
			if cursor = p.NextCursor; cursor == "" {
				break
			}
		}
		if !reflect.DeepEqual(paged, ids(all)) {
			t.Fatalf("偏移量翻页：%v", paged)
		}
		if got := f.list(bob(), "mcp status", nil); !reflect.DeepEqual(ids(got), []int64{d.ID}) {
			t.Fatalf("普通用户只看自己的：%v", ids(got))
		}
		_, err := f.run(admin(), "mcp status", nil, map[string]any{"cursor": command.EncodeIDCursor(3)}, nil)
		wantErr(t, err, v1.CodeBadRequest)
	})
}

// master-api-tokens「令牌解析成身份」：有效、四种无效、签发者降级、节流、查库出错。
func TestResolve(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := newFixture(t, bdb)
		ctx := context.Background()
		ops := f.create(admin(), map[string]any{"name": "ops", "preset": "ops"})
		got, ok := f.s.Resolve(ctx, ops.Token)
		if !ok || got.Actor != "admin" || got.ActorKind != v1.ActorToken || got.Role != v1.RoleAdmin || got.TokenID == nil || *got.TokenID != ops.ID ||
			!reflect.DeepEqual(got.Scopes, readOp) || got.Danger == nil || len(got.Danger) != 0 {
			t.Fatalf("令牌身份：%+v %v", got, ok)
		}
		for _, bad := range []string{"", "abc", "mmwx_" + ops.Token[len(Prefix):], ops.Token + "x", Prefix} {
			if _, ok := f.s.Resolve(ctx, bad); ok {
				t.Errorf("%q 应当无效", bad)
			}
		}
		// 查库出错当无效（这里用已取消的 ctx 让查询失败）。
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, ok := f.s.Resolve(cancelled, ops.Token); ok {
			t.Fatal("查库出错应当当无效")
		}
		// 查库出错要单独交出 error（authn 据此不计成猜令牌）；真正无效的令牌不带 error。
		if _, ok, err := f.s.ResolveToken(cancelled, ops.Token); ok || err == nil {
			t.Fatalf("查库出错应当交回 error：%v %v", ok, err)
		}
		if _, ok, err := f.s.ResolveToken(ctx, Prefix+"nosuch"); ok || err != nil {
			t.Fatalf("不存在的令牌无效但不是错误：%v %v", ok, err)
		}

		// 签发者停用后无效，重新启用又能用；软删除后无效。
		f.setUser("admin", "is_active", false)
		if _, ok := f.s.Resolve(ctx, ops.Token); ok {
			t.Fatal("签发者停用后应当无效")
		}
		f.setUser("admin", "is_active", true)
		if _, ok := f.s.Resolve(ctx, ops.Token); !ok {
			t.Fatal("签发者重新启用后应当又能用")
		}
		f.user("carol", v1.RoleAdmin)
		full, err := f.run(as(v1.LocalAdmin("root")), "token create", nil, map[string]any{"name": "full", "preset": "full", "secrets": true}, &command.Verification{User: "carol"})
		if err != nil {
			t.Fatal(err)
		}
		fullTok := full.(Created).Token
		f.setUser("carol", "role", "user")
		if got, ok := f.s.Resolve(ctx, fullTok); !ok || got.Role != v1.RoleUser || !reflect.DeepEqual(got.Scopes, readOp) || len(got.Danger) != 0 {
			t.Fatalf("签发者降级后权限随之收窄：%+v %v", got, ok)
		}
		f.setUser("carol", "deleted_at", time.Now().UTC())
		if _, ok := f.s.Resolve(ctx, fullTok); ok {
			t.Fatal("签发者软删除后应当无效")
		}

		// 过期：expires_at 一到就无效。
		short := f.create(admin(), map[string]any{"name": "short", "expires-in": time.Hour})
		if _, ok := f.s.Resolve(ctx, short.Token); !ok {
			t.Fatal("没过期时有效")
		}
		f.advance(time.Hour)
		if _, ok := f.s.Resolve(ctx, short.Token); ok {
			t.Fatal("expires_at 到了应当无效")
		}
	})
}

// 最后使用时间 60 秒节流。
func TestResolveThrottle(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := newFixture(t, bdb)
		ctx := context.Background()
		tok := f.create(admin(), map[string]any{"name": "t"})
		lastUsed := func() *time.Time {
			t.Helper()
			got, err := f.s.repo.GetByID(ctx, tok.ID)
			if err != nil {
				t.Fatal(err)
			}
			return got.LastUsedAt
		}
		start := f.s.now()
		f.s.Resolve(ctx, tok.Token)
		first := lastUsed()
		if first == nil || first.Sub(start).Abs() > time.Second {
			t.Fatalf("第一次使用应当写最后使用时间：%v", first)
		}
		f.advance(5 * time.Second)
		f.s.Resolve(ctx, tok.Token)
		f.advance(5 * time.Second)
		f.s.Resolve(ctx, tok.Token)
		if got := lastUsed(); got == nil || !got.Equal(*first) {
			t.Fatalf("十秒内再用不写：%v，第一次是 %v", got, first)
		}
		f.advance(51 * time.Second)
		f.s.Resolve(ctx, tok.Token)
		if got := lastUsed(); got == nil || got.Sub(*first) < time.Minute {
			t.Fatalf("超过 60 秒再写：%v，第一次是 %v", got, first)
		}
		if items := f.list(admin(), "token list", nil).Items; items[0].(Info).LastUsedAt == nil {
			t.Fatal("token list 里能看到最后使用时间")
		}
	})
}

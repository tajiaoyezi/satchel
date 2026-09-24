// Package tokens 是业务层的 API 令牌（master-api-tokens）：签发、列表、改权限、吊销四条命令与 mcp status，
// 权限范围的规范化、预设推导与按签发者角色的上限，以及给 authn 用的令牌解析（令牌 → 身份对象）。
// 签发、改权限、吊销是第 05 章七组：人类专属，当场验证由 authz 在处理函数之前做完。
package tokens

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/tokens"
	"github.com/satchel/satchel/internal/core/users"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// touchInterval 是最后使用时间的写入节流：距上次写入不足这么久就不写。
const touchInterval = 60 * time.Second

// Service 持有令牌与用户两个仓储（用户仓储用来查签发者的角色与状态）。
type Service struct {
	repo   *core.Repo
	users  *users.Repo
	now    func() time.Time
	logger *slog.Logger
}

// New 建服务；logger 为 nil 时用 slog.Default()。
func New(repo *core.Repo, u *users.Repo, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, users: u, now: func() time.Time { return time.Now().UTC() }, logger: logger}
}

// Bindings 是本服务提供的命令处理函数，按命令名给 cmd/satchel 绑定。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{
		"token create": s.create,
		"token list":   s.list,
		"token update": s.update,
		"token revoke": s.revoke,
		"mcp status":   s.mcpStatus,
	}
}

// Info 是一把令牌对外的样子：token list、mcp status、token update / revoke 的输出。没有明文，也没有哈希。
type Info struct {
	ID         int64       `json:"id"`
	Name       string      `json:"name"`
	Owner      string      `json:"owner"`
	Preset     string      `json:"preset"`
	Scopes     []v1.Scope  `json:"scopes"`
	Danger     []v1.Danger `json:"danger"`
	Runtime    string      `json:"runtime"`
	ExpiresAt  *time.Time  `json:"expires_at"`
	LastUsedAt *time.Time  `json:"last_used_at"`
	CreatedAt  time.Time   `json:"created_at"`
	RevokedAt  *time.Time  `json:"revoked_at"`
	// State 是 active、revoked 或 expired。
	State string `json:"state"`
}

// Created 是 token create 的输出：Info 加上只出现这一次的明文。
type Created struct {
	Info
	Token string `json:"token"`
}

func (s *Service) info(t *core.Token) Info {
	g := normalize(t.Grant)
	state := "active"
	switch {
	case t.Revoked:
		state = "revoked"
	case t.ExpiresAt != nil && !s.now().Before(*t.ExpiresAt):
		state = "expired"
	}
	return Info{
		ID: t.ID, Name: t.Name, Owner: t.Owner, Preset: presetOf(g), Scopes: g.Scopes, Danger: g.Danger, Runtime: t.Runtime,
		ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, CreatedAt: t.CreatedAt, RevokedAt: t.RevokedAt, State: state,
	}
}

// callerAccount 是调用者自己的账号名：用户与令牌身份就是 actor（令牌的 actor 是签发者），本机管理员不是账号、返回空串。
func callerAccount(id v1.Identity) string {
	if id.ActorKind == v1.ActorUser || id.ActorKind == v1.ActorToken {
		return id.Actor
	}
	return ""
}

// visible 报告调用者能不能看到、改这把令牌：管理员角色全部可以，普通用户只有自己签发的。
func visible(id v1.Identity, t *core.Token) bool {
	return id.IsAdmin() || (callerAccount(id) != "" && t.Owner == callerAccount(id))
}

var runtimeRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

func validName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if n := utf8.RuneCountInString(name); n < 1 || n > 64 {
		return "", v1.Newf(v1.CodeBadRequest, "令牌的名字要 1 到 64 个字符，得到 %d 个", n).WithNext("用 --name 给令牌起个名字，如 --name ci")
	}
	return name, nil
}

// changeOf 从调用对象里取权限相关的参数：没给的留 nil。
func changeOf(inv *command.Invocation) change {
	var c change
	if v, ok := inv.Flags["preset"].(string); ok {
		p := strings.TrimSpace(v)
		c.preset = &p
	}
	if v, ok := inv.Flags["danger"].([]string); ok {
		c.danger, c.dangerG = v, true
	}
	if v, ok := inv.Flags["secrets"].(bool); ok {
		c.secrets = &v
	}
	return c
}

// expiresOf 取 --expires-in：没给返回 given=false；0 表示不过期（nil）；负数 bad_request。
func (s *Service) expiresOf(inv *command.Invocation) (at *time.Time, given bool, err error) {
	d, ok := inv.Flags["expires-in"].(time.Duration)
	if !ok {
		return nil, false, nil
	}
	if d < 0 {
		return nil, true, v1.Newf(v1.CodeBadRequest, "expires-in 不能是负数，得到 %s", d)
	}
	if d == 0 {
		return nil, true, nil
	}
	t := s.now().Add(d)
	return &t, true, nil
}

// owner 找出签发者的账号：用户就是自己；本机管理员是当场验证指明的那个管理员账号（authz 的验证器已经验过它启用中且是管理员）。
func (s *Service) owner(ctx context.Context, inv *command.Invocation) (*users.Account, error) {
	id := v1.IdentityFrom(ctx)
	var name string
	switch id.ActorKind {
	case v1.ActorUser:
		name = id.Actor
	case v1.ActorLocalAdmin:
		if inv.Verify != nil {
			name = strings.TrimSpace(inv.Verify.User)
		}
		if name == "" {
			return nil, v1.New(v1.CodeHumanRequired, "本机管理员签发令牌要用 verify-user 指明一个管理员账号，令牌挂在它名下").
				WithNext("加上 --verify-user <管理员用户名>")
		}
	default:
		return nil, v1.Newf(v1.CodeHumanRequired, "身份 %s 不能签发令牌", id.ActorKind)
	}
	a, err := s.users.GetByUsername(ctx, name)
	if errors.Is(err, users.ErrNotFound) || (err == nil && (a.Deleted || !a.IsActive)) {
		return nil, v1.Newf(v1.CodeForbidden, "签发者账号 %s 不存在或已停用", name)
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (s *Service) create(ctx context.Context, inv *command.Invocation) (any, error) {
	name, err := validName(inv.String("name", ""))
	if err != nil {
		return nil, err
	}
	runtime := strings.TrimSpace(inv.String("runtime", ""))
	if runtime != "" && !runtimeRe.MatchString(runtime) {
		return nil, v1.Newf(v1.CodeBadRequest, "runtime 标签只能是 1 到 64 个字母、数字或 . _ @ -，得到 %q", runtime)
	}
	grant, err := changeOf(inv).apply(core.Grant{Scopes: []v1.Scope{v1.ScopeRead}})
	if err != nil {
		return nil, err
	}
	expires, _, err := s.expiresOf(inv)
	if err != nil {
		return nil, err
	}
	owner, err := s.owner(ctx, inv)
	if err != nil {
		return nil, err
	}
	if err := checkCap(grant, owner.Role); err != nil {
		return nil, err
	}
	plain, hash, err := Generate()
	if err != nil {
		return nil, err
	}
	t := &core.Token{Owner: owner.Username, Name: name, Grant: grant, Preset: presetOf(grant), ExpiresAt: expires, Runtime: runtime}
	if err := s.repo.Insert(ctx, t, hash); err != nil {
		return nil, err
	}
	return Created{Info: s.info(t), Token: plain}, nil
}

// findVisible 按位置参数里的 id 找一把调用者看得到的令牌：id 不合法 bad_request，找不到或看不到都是 not_found。
func (s *Service) findVisible(ctx context.Context, inv *command.Invocation) (*core.Token, error) {
	id, err := strconv.ParseInt(inv.Arg(0), 10, 64)
	if err != nil || id <= 0 {
		return nil, v1.Newf(v1.CodeBadRequest, "令牌 id 必须是正整数，得到 %q", inv.Arg(0)).WithNext("用 token list 查看令牌的 id")
	}
	t, err := s.repo.GetByID(ctx, id)
	if errors.Is(err, core.ErrNotFound) || (err == nil && !visible(v1.IdentityFrom(ctx), t)) {
		return nil, v1.Newf(v1.CodeNotFound, "没有 id 为 %d 的令牌", id).WithNext("用 token list 查看你能看到的令牌")
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Service) update(ctx context.Context, inv *command.Invocation) (any, error) {
	t, err := s.findVisible(ctx, inv)
	if err != nil {
		return nil, err
	}
	if t.Revoked {
		return nil, v1.Newf(v1.CodeBadRequest, "令牌 %d 已吊销，不能再改", t.ID)
	}
	var columns []string
	if raw, ok := inv.Flags["name"].(string); ok {
		name, err := validName(raw)
		if err != nil {
			return nil, err
		}
		t.Name = name
		columns = append(columns, "name")
	}
	c := changeOf(inv)
	if c.preset != nil || c.dangerG || c.secrets != nil {
		grant, err := c.apply(t.Grant)
		if err != nil {
			return nil, err
		}
		a, err := s.users.GetByUsername(ctx, t.Owner)
		if err != nil {
			return nil, err
		}
		if err := checkCap(grant, a.Role); err != nil {
			return nil, err
		}
		t.Grant, t.Preset = grant, presetOf(grant)
		columns = append(columns, "scopes", "preset")
	}
	expires, given, err := s.expiresOf(inv)
	if err != nil {
		return nil, err
	}
	if given {
		t.ExpiresAt = expires
		columns = append(columns, "expires_at")
	}
	if len(columns) == 0 {
		return nil, v1.New(v1.CodeBadRequest, "没有要改的项").WithNext("给出 --name、--preset、--danger、--secrets 或 --expires-in 中的至少一项")
	}
	if err := s.repo.Update(ctx, t, columns...); err != nil {
		if v1.AsError(err).Code == v1.CodeConflict {
			return nil, v1.Newf(v1.CodeBadRequest, "令牌 %d 已吊销，不能再改", t.ID)
		}
		return nil, err
	}
	return s.info(t), nil
}

func (s *Service) revoke(ctx context.Context, inv *command.Invocation) (any, error) {
	t, err := s.findVisible(ctx, inv)
	if err != nil {
		return nil, err
	}
	if t.Revoked {
		return nil, v1.Newf(v1.CodeBadRequest, "令牌 %d 已经吊销过了", t.ID)
	}
	at := s.now()
	if err := s.repo.Revoke(ctx, t.ID, at); err != nil {
		if v1.AsError(err).Code == v1.CodeConflict {
			return nil, v1.Newf(v1.CodeBadRequest, "令牌 %d 已经吊销过了", t.ID)
		}
		return nil, err
	}
	t.Revoked, t.RevokedAt = true, &at
	return s.info(t), nil
}

// listFilter 决定调用者这次能看哪些令牌：管理员可按 --owner 过滤，普通用户只看自己的；普通用户指定了别人的名字返回 false（空列表）。
func listFilter(id v1.Identity, inv *command.Invocation) (core.Filter, bool) {
	owner := strings.TrimSpace(inv.String("owner", ""))
	if id.IsAdmin() {
		return core.Filter{Owner: owner}, true
	}
	self := callerAccount(id)
	if self == "" || (owner != "" && owner != self) {
		return core.Filter{}, false
	}
	return core.Filter{Owner: self}, true
}

func pageOf(inv *command.Invocation) (command.Page, error) {
	page := command.Page{}
	if inv.Page != nil {
		page = *inv.Page
	}
	return page, page.Normalize()
}

func (s *Service) list(ctx context.Context, inv *command.Invocation) (any, error) {
	page, err := pageOf(inv)
	if err != nil {
		return nil, err
	}
	beforeID, err := command.DecodeIDCursor(page.Cursor)
	if err != nil {
		return nil, err
	}
	f, ok := listFilter(v1.IdentityFrom(ctx), inv)
	if !ok {
		return &command.PageResult{Items: []any{}}, nil
	}
	total, err := s.repo.Count(ctx, f)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.List(ctx, f, page.Limit+1, beforeID)
	if err != nil {
		return nil, err
	}
	res := &command.PageResult{Items: make([]any, 0, len(rows)), Total: total}
	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		res.NextCursor = command.EncodeIDCursor(rows[len(rows)-1].ID)
	}
	for i := range rows {
		res.Items = append(res.Items, s.info(&rows[i]))
	}
	return res, nil
}

// mcpStatus 是 mcp status：绑了 runtime 标签的令牌，按最后使用时间倒序（从没用过的排最后），可见范围与 token list 相同。
func (s *Service) mcpStatus(ctx context.Context, inv *command.Invocation) (any, error) {
	page, err := pageOf(inv)
	if err != nil {
		return nil, err
	}
	offset, err := command.DecodeOffsetCursor(page.Cursor)
	if err != nil {
		return nil, err
	}
	f, ok := listFilter(v1.IdentityFrom(ctx), inv)
	if !ok {
		return &command.PageResult{Items: []any{}}, nil
	}
	f.RuntimeOnly = true
	total, err := s.repo.Count(ctx, f)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.ListByLastUsed(ctx, f, page.Limit+1, offset)
	if err != nil {
		return nil, err
	}
	res := &command.PageResult{Items: make([]any, 0, len(rows)), Total: total}
	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		res.NextCursor = command.EncodeOffsetCursor(offset + page.Limit)
	}
	for i := range rows {
		res.Items = append(res.Items, s.info(&rows[i]))
	}
	return res, nil
}

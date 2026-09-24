package v1

import "context"

// ActorKind 是身份的种类（master-identity-and-authz）。本 change 里只会出现 local_admin 与 anonymous，
// user（m1-02 会话）、token（m1-04 令牌）、system（scheduler 以系统身份跑）往同一个形状里填。
type ActorKind string

const (
	ActorLocalAdmin ActorKind = "local_admin" // 经数据目录的 unix socket 连进来的 root 或运行主控的 OS 用户
	ActorUser       ActorKind = "user"        // 网页会话
	ActorToken      ActorKind = "token"       // 带权限范围的 API 令牌
	ActorSystem     ActorKind = "system"      // 内置维护任务
	ActorAnonymous  ActorKind = "anonymous"   // 没有身份
)

// Scope 是第 05 章功能②的权限范围：read 只读、operate 可操作（危险类另开）、secrets 密钥读取。
type Scope string

const (
	ScopeRead    Scope = "read"
	ScopeOperate Scope = "operate"
	ScopeSecrets Scope = "secrets"
)

// Danger 是第 05 章的危险操作六类，令牌默认不给、须单独开。
type Danger string

const (
	DangerDelete     Danger = "delete"     // 删除类（含恢复）
	DangerRestart    Danger = "restart"    // 重启类
	DangerPermission Danger = "permission" // 权限类
	DangerBatch      Danger = "batch"      // 批量类
	DangerExec       Danger = "exec"       // 节点执行类
	DangerMaster     Danger = "master"     // 主控自身类
)

// AllScopes 与 AllDangers 是全集，本机管理员两样都有。
var (
	AllScopes  = []Scope{ScopeRead, ScopeOperate, ScopeSecrets}
	AllDangers = []Danger{DangerDelete, DangerRestart, DangerPermission, DangerBatch, DangerExec, DangerMaster}
)

// Role 是账号角色：admin 管理员、user 普通用户（第 10 章两角色）。本机管理员与系统身份都是 admin。
type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// Identity 是一次调用的身份对象，whoami 原样返回它；字段名就是 JSON 输出的字段名。
// 令牌身份的 Role 是签发者账号的角色（权限上限随签发者，第 05 章功能②）。
type Identity struct {
	Actor     string    `json:"actor"`
	ActorKind ActorKind `json:"actor_kind"`
	Role      Role      `json:"role"`
	TokenID   *int64    `json:"token_id"`
	Scopes    []Scope   `json:"scopes"`
	Danger    []Danger  `json:"danger"`
}

// Anonymous 是没有身份的调用者。
func Anonymous() Identity {
	return Identity{ActorKind: ActorAnonymous, Scopes: []Scope{}, Danger: []Danger{}}
}

// LocalAdmin 是经 unix socket 判定出的本机管理员：全部 scope 与全部危险类，角色 admin。
func LocalAdmin(actor string) Identity {
	return Identity{Actor: actor, ActorKind: ActorLocalAdmin, Role: RoleAdmin, Scopes: append([]Scope{}, AllScopes...), Danger: append([]Danger{}, AllDangers...)}
}

// IsAdmin 报告身份是不是管理员角色。
func (id Identity) IsAdmin() bool { return id.Role == RoleAdmin }

type identityKey struct{}

// WithIdentity 把身份放进 ctx；authn 中间件写、执行链与处理函数读。放在契约层是因为 service 层不能引用 middleware。
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom 取 ctx 里的身份；没有就是 anonymous。
func IdentityFrom(ctx context.Context) Identity {
	if id, ok := ctx.Value(identityKey{}).(Identity); ok {
		return id
	}
	return Anonymous()
}

// IsAnonymous 报告这是不是没有身份的调用者（零值也算）。
func (id Identity) IsAnonymous() bool {
	return id.ActorKind == "" || id.ActorKind == ActorAnonymous
}

// HasScope 报告身份是否带某个权限范围。
func (id Identity) HasScope(s Scope) bool {
	for _, have := range id.Scopes {
		if have == s {
			return true
		}
	}
	return false
}

// HasDanger 报告身份是否开了某个危险类。
func (id Identity) HasDanger(d Danger) bool {
	for _, have := range id.Danger {
		if have == d {
			return true
		}
	}
	return false
}

// CredentialSource 是身份从哪来：socket 对端凭据、网页会话 cookie、API 令牌，或带了无效凭据。REST 的同源检查只对会话来的与没有身份的写请求做。
type CredentialSource string

const (
	SourceNone    CredentialSource = ""
	SourceSocket  CredentialSource = "socket"
	SourceSession CredentialSource = "session"
	SourceToken   CredentialSource = "token"
	// SourceInvalid 表示请求带了凭据但无效：令牌不存在、已吊销、已过期、签发者已停用或删除，或 Authorization 头不是 Bearer。
	// 这时身份是 anonymous，authz 一律 unauthenticated（连不要身份的命令也拒），审计不记，也不退回 socket 或 cookie 的身份。
	SourceInvalid CredentialSource = "invalid"
)

type sourceKey struct{}

// WithCredentialSource 把身份来源放进 ctx。
func WithCredentialSource(ctx context.Context, s CredentialSource) context.Context {
	return context.WithValue(ctx, sourceKey{}, s)
}

// CredentialSourceFrom 取 ctx 里的身份来源；没有就是 SourceNone。
func CredentialSourceFrom(ctx context.Context) CredentialSource {
	if s, ok := ctx.Value(sourceKey{}).(CredentialSource); ok {
		return s
	}
	return SourceNone
}

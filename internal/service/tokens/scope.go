package tokens

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"

	core "github.com/satchel/satchel/internal/core/tokens"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Prefix 是令牌字符串的前缀：authn 在查库之前就能拒掉明显不是 Satchel 令牌的串（例如 mmwx 的 mmwx_ 令牌）。
const Prefix = "sat_"

// 预设（master-api-tokens「权限范围与预设」）：由权限范围推出，始终与之一致。
const (
	PresetReadonly = "readonly"
	PresetOps      = "ops"
	PresetFull     = "full"
)

// Generate 生成一把新令牌：sat_ 加 32 字节随机数的 base64url（无填充），返回明文与它的哈希。
func Generate() (plain, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", v1.Wrap(v1.CodeInternal, "生成随机令牌失败", err)
	}
	plain = Prefix + base64.RawURLEncoding.EncodeToString(buf)
	return plain, Hash(plain), nil
}

// Hash 是令牌在库里的样子：SHA-256 十六进制。
func Hash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// scopeOrder 与 v1.AllDangers 决定规范化后的顺序，列表与审计里读起来稳定。
var scopeOrder = []v1.Scope{v1.ScopeRead, v1.ScopeOperate, v1.ScopeSecrets}

// normalize 把权限范围规范化：read 恒在、去重、按固定顺序排，未知的项丢掉（写入前已校验过）。
func normalize(g core.Grant) core.Grant {
	hasScope := map[v1.Scope]bool{v1.ScopeRead: true}
	for _, s := range g.Scopes {
		hasScope[s] = true
	}
	hasDanger := map[v1.Danger]bool{}
	for _, d := range g.Danger {
		hasDanger[d] = true
	}
	out := core.Grant{Scopes: []v1.Scope{}, Danger: []v1.Danger{}}
	for _, s := range scopeOrder {
		if hasScope[s] {
			out.Scopes = append(out.Scopes, s)
		}
	}
	for _, d := range v1.AllDangers {
		if hasDanger[d] {
			out.Danger = append(out.Danger, d)
		}
	}
	return out
}

func has[T comparable](list []T, v T) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// presetOf 由权限范围推出预设：没有 operate 是只读；operate 加六类全开是全权；其余是日常运维。secrets 不影响预设。
func presetOf(g core.Grant) string {
	if !has(g.Scopes, v1.ScopeOperate) {
		return PresetReadonly
	}
	if len(g.Danger) == len(v1.AllDangers) {
		return PresetFull
	}
	return PresetOps
}

// change 是一次签发或改权限里给出的权限相关参数；nil 表示没给。
type change struct {
	preset  *string
	danger  []string
	dangerG bool
	secrets *bool
}

// apply 在 base 之上应用一次改动：--preset 把 operate 与危险类设成预设的样子（secrets 保留）；--danger 把危险类设成
// 恰好给出的这几个并隐含 operate；--secrets 开关密钥读取。只读预设同时给危险类是 bad_request。返回规范化后的结果。
func (c change) apply(base core.Grant) (core.Grant, error) {
	g := normalize(base)
	if c.preset != nil {
		secrets := has(g.Scopes, v1.ScopeSecrets)
		switch *c.preset {
		case PresetReadonly:
			g = core.Grant{Scopes: []v1.Scope{v1.ScopeRead}}
		case PresetOps:
			g = core.Grant{Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate}}
		case PresetFull:
			g = core.Grant{Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate}, Danger: append([]v1.Danger{}, v1.AllDangers...)}
		default:
			return core.Grant{}, v1.Newf(v1.CodeBadRequest, "preset 只能是 readonly、ops 或 full，得到 %q", *c.preset)
		}
		if secrets {
			g.Scopes = append(g.Scopes, v1.ScopeSecrets)
		}
	}
	if c.dangerG {
		danger := make([]v1.Danger, 0, len(c.danger))
		for _, raw := range c.danger {
			d := v1.Danger(strings.TrimSpace(raw))
			if !has(v1.AllDangers, d) {
				return core.Grant{}, v1.Newf(v1.CodeBadRequest, "危险类只能是 delete、restart、permission、batch、exec、master，得到 %q", raw)
			}
			danger = append(danger, d)
		}
		// 空清单（REST 的 "danger": []）只是清空危险类，不隐含 operate。
		if len(danger) > 0 {
			if c.preset != nil && *c.preset == PresetReadonly {
				return core.Grant{}, v1.New(v1.CodeBadRequest, "只读的令牌不能开危险类：危险类只在「可操作」时才有意义").
					WithNext("去掉 --preset readonly，或去掉 --danger")
			}
			if !has(g.Scopes, v1.ScopeOperate) {
				g.Scopes = append(g.Scopes, v1.ScopeOperate)
			}
		}
		g.Danger = danger
	}
	if c.secrets != nil {
		scopes := make([]v1.Scope, 0, len(g.Scopes))
		for _, s := range g.Scopes {
			if s != v1.ScopeSecrets {
				scopes = append(scopes, s)
			}
		}
		if *c.secrets {
			scopes = append(scopes, v1.ScopeSecrets)
		}
		g.Scopes = scopes
	}
	return normalize(g), nil
}

// capOf 是一个角色能给令牌的上限：管理员全部 scope 与六个危险类；普通用户只有 read 与 operate（与会话身份的上限相同）。
func capOf(role v1.Role) core.Grant {
	if role == v1.RoleAdmin {
		return core.Grant{Scopes: append([]v1.Scope{}, v1.AllScopes...), Danger: append([]v1.Danger{}, v1.AllDangers...)}
	}
	return core.Grant{Scopes: []v1.Scope{v1.ScopeRead, v1.ScopeOperate}, Danger: []v1.Danger{}}
}

// checkCap 在签发与改权限时检查上限：超出直接 forbidden 并点名超出的项，不静默截掉。
func checkCap(g core.Grant, role v1.Role) error {
	limit := capOf(role)
	var excess []string
	for _, s := range g.Scopes {
		if !has(limit.Scopes, s) {
			excess = append(excess, string(s))
		}
	}
	for _, d := range g.Danger {
		if !has(limit.Danger, d) {
			excess = append(excess, string(d))
		}
	}
	if len(excess) > 0 {
		return v1.Newf(v1.CodeForbidden, "签发者是普通用户，令牌只能带 read 与 operate，超出的有：%s（危险类与密钥读取只有管理员的令牌能开）", strings.Join(excess, "、")).
			WithState("excess", excess)
	}
	return nil
}

// intersect 是令牌被使用时实际生效的权限：权限范围与签发者当下角色上限的交集（签发者被降级后，危险类与密钥读取随之失效）。
func intersect(g core.Grant, role v1.Role) ([]v1.Scope, []v1.Danger) {
	limit := capOf(role)
	g = normalize(g)
	scopes := []v1.Scope{}
	for _, s := range g.Scopes {
		if has(limit.Scopes, s) {
			scopes = append(scopes, s)
		}
	}
	danger := []v1.Danger{}
	for _, d := range g.Danger {
		if has(limit.Danger, d) {
			danger = append(danger, d)
		}
	}
	return scopes, danger
}

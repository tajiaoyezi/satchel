package command

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Invocation 是三个投影解析出的同一个调用对象：CLI 从 cobra、REST 从查询参数或请求体、
// MCP 经 CLI 的解析器，最后都产出它交给同一个 Runner。
type Invocation struct {
	// Path 是命令路径各段。
	Path []string
	// Args 是位置参数的值，按登记顺序。
	Args []string
	// Flags 是 flag 名到已按类型解析的值：string、int、bool、time.Duration、[]string。没给的 flag 不出现。
	Flags map[string]any
	// Confirm 是危险命令的确认字符串；缺失为空串。
	Confirm string
	// Page 只在列表命令上有。
	Page *Page
	// Verify 是人类专属命令的当场验证值；不在 Flags 里，永不进审计摘要。没给为 nil。
	Verify *Verification
}

// Verification 是第 05 章七组的当场验证：密码、第二因素（TOTP 或恢复码）、要验的管理员账号（本机管理员必填）。
type Verification struct {
	Password string
	Code     string
	User     string
}

// Name 是命令名（各段空格连接）。
func (inv *Invocation) Name() string { return strings.Join(inv.Path, " ") }

// String 取字符串 flag，没给返回 def。
func (inv *Invocation) String(flag, def string) string {
	if v, ok := inv.Flags[flag].(string); ok {
		return v
	}
	return def
}

// Int 取整数 flag，没给返回 def。
func (inv *Invocation) Int(flag string, def int) int {
	if v, ok := inv.Flags[flag].(int); ok {
		return v
	}
	return def
}

// Bool 取布尔 flag，没给返回 false。
func (inv *Invocation) Bool(flag string) bool {
	v, _ := inv.Flags[flag].(bool)
	return v
}

// Duration 取时长 flag，没给返回 def。
func (inv *Invocation) Duration(flag string, def time.Duration) time.Duration {
	if v, ok := inv.Flags[flag].(time.Duration); ok {
		return v
	}
	return def
}

// Strings 取可重复 flag。
func (inv *Invocation) Strings(flag string) []string {
	v, _ := inv.Flags[flag].([]string)
	return v
}

// Arg 取第 i 个位置参数，没有返回空串。
func (inv *Invocation) Arg(i int) string {
	if i < len(inv.Args) {
		return inv.Args[i]
	}
	return ""
}

// Handler 是一条命令在主控里的处理函数：返回可编成 JSON 对象的结果，或四字段错误。
type Handler func(ctx context.Context, inv *Invocation) (any, error)

// Bindings 是命令名到处理函数的绑定，由 cmd/satchel 装配；Table.CheckBindings 校验它与表一一对应。
type Bindings map[string]Handler

// Runner 是执行链：投影层拿到 Invocation 后只调它。主控进程里是 audit(authz(dispatch))，
// CLI 进程里是连主控的客户端。
type Runner interface {
	Run(ctx context.Context, inv *Invocation) (any, error)
}

// RunnerFunc 让一个函数满足 Runner。
type RunnerFunc func(ctx context.Context, inv *Invocation) (any, error)

// Run 调用函数本身。
func (f RunnerFunc) Run(ctx context.Context, inv *Invocation) (any, error) { return f(ctx, inv) }

// Dispatch 是执行链最内层：按 Bindings 找处理函数并调用。
func Dispatch(b Bindings) Runner {
	return RunnerFunc(func(ctx context.Context, inv *Invocation) (any, error) {
		h, ok := b[inv.Name()]
		if !ok {
			return nil, v1.Newf(v1.CodeInternal, "命令 %s 没有装配处理函数", inv.Name())
		}
		return h(ctx, inv)
	})
}

// 列表分页（master-rest-api「列表分页」）。
const (
	DefaultLimit = 50
	MaxLimit     = 500
)

// Page 是列表命令的分页参数。Cursor 是不透明字符串，来自上一页的 NextCursor。
type Page struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor,omitempty"`
}

// Normalize 补默认值并校验范围：limit 不在 1 到 500 之间报 bad_request，不静默截断。
func (p *Page) Normalize() error {
	if p.Limit == 0 {
		p.Limit = DefaultLimit
	}
	if p.Limit < 1 || p.Limit > MaxLimit {
		return v1.Newf(v1.CodeBadRequest, "limit 必须在 1 到 %d 之间，得到 %d", MaxLimit, p.Limit)
	}
	return nil
}

// PageResult 是列表命令的结果：items 至多 limit 条，total 是过滤条件下的总条数，nextCursor 末页为空串。
type PageResult struct {
	Items      []any  `json:"items"`
	Total      int    `json:"total"`
	NextCursor string `json:"nextCursor"`
}

// EncodeIDCursor 把 keyset 游标（最后一条的 id）编成不透明字符串。
func EncodeIDCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("id:" + strconv.FormatInt(id, 10)))
}

// DecodeIDCursor 解 EncodeIDCursor 的输出；空串表示第一页，返回 0。解不出报 bad_request。
func DecodeIDCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err == nil {
		if n, ok := strings.CutPrefix(string(raw), "id:"); ok {
			if id, err := strconv.ParseInt(n, 10, 64); err == nil && id > 0 {
				return id, nil
			}
		}
	}
	return 0, v1.Newf(v1.CodeBadRequest, "cursor 不合法，请用上一页返回的 nextCursor")
}

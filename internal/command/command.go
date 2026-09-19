package command

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Class 是命令的类别：local 只在 CLI 进程里跑、不经主控；其余四种是第 07 章五类操作里有写路径或读路径的类别。
type Class string

const (
	ClassLocal          Class = "local"
	ClassRead           Class = "read"
	ClassConfig         Class = "config"          // 配置类：plan / apply
	ClassAction         Class = "action"          // 动作类
	ClassMasterSettings Class = "master_settings" // 主控设置类
)

// Scope 由类别推出（master-command-table）：read → read，其余非本地 → operate，local 没有 scope。
func (c Class) Scope() (v1.Scope, bool) {
	switch c {
	case ClassRead:
		return v1.ScopeRead, true
	case ClassConfig, ClassAction, ClassMasterSettings:
		return v1.ScopeOperate, true
	}
	return "", false
}

var validClasses = map[Class]bool{ClassLocal: true, ClassRead: true, ClassConfig: true, ClassAction: true, ClassMasterSettings: true}

// FlagType 是 flag 的取值类型。file 是 CLI 本地读取的文件路径，只有 CLI 投影接受。
type FlagType string

const (
	TypeString   FlagType = "string"
	TypeInt      FlagType = "int"
	TypeBool     FlagType = "bool"
	TypeDuration FlagType = "duration"
	TypeStrings  FlagType = "strings" // 可重复
	TypeFile     FlagType = "file"
)

var validFlagTypes = map[FlagType]bool{TypeString: true, TypeInt: true, TypeBool: true, TypeDuration: true, TypeStrings: true, TypeFile: true}

// Arg 是位置参数。
type Arg struct {
	Name        string
	Description string
	Optional    bool
}

// Flag 是命令的一个 flag。Secret 标密钥类：审计摘要与日志里打码。表里的 flag 一律没有短名。
type Flag struct {
	Name        string
	Type        FlagType
	Default     string
	Description string
	Secret      bool
}

// Parse 把命令行或查询参数里的字符串按类型解析。strings 类型由调用方逐个值调它。
func (f Flag) Parse(raw string) (any, error) {
	switch f.Type {
	case TypeString, TypeFile, TypeStrings:
		return raw, nil
	case TypeInt:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, v1.Newf(v1.CodeBadRequest, "参数 %s 的值 %s 不是整数", f.Name, raw)
		}
		return n, nil
	case TypeBool:
		switch raw {
		case "", "true", "1":
			return true, nil
		case "false", "0":
			return false, nil
		}
		return nil, v1.Newf(v1.CodeBadRequest, "参数 %s 的值 %s 不是布尔值", f.Name, raw)
	case TypeDuration:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return nil, v1.Newf(v1.CodeBadRequest, "参数 %s 的值 %s 不是时长（如 30s、5m）", f.Name, raw)
		}
		return d, nil
	}
	return nil, v1.Newf(v1.CodeInternal, "参数 %s 的类型 %s 不认识", f.Name, f.Type)
}

// FromJSON 把 JSON 请求体里解出来的值按类型校验、归一（int 从 float64 收窄，duration 从字符串解析）。
func (f Flag) FromJSON(v any) (any, error) {
	bad := func() (any, error) {
		return nil, v1.Newf(v1.CodeBadRequest, "参数 %s 的值 %v 不是 %s", f.Name, v, f.Type)
	}
	switch f.Type {
	case TypeString, TypeFile:
		s, ok := v.(string)
		if !ok {
			return bad()
		}
		return s, nil
	case TypeInt:
		n, ok := v.(float64)
		if !ok || n != float64(int(n)) {
			return bad()
		}
		return int(n), nil
	case TypeBool:
		b, ok := v.(bool)
		if !ok {
			return bad()
		}
		return b, nil
	case TypeDuration:
		s, ok := v.(string)
		if !ok {
			return bad()
		}
		return f.Parse(s)
	case TypeStrings:
		list, ok := v.([]any)
		if !ok {
			return bad()
		}
		out := make([]string, 0, len(list))
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return bad()
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, v1.Newf(v1.CodeInternal, "参数 %s 的类型 %s 不认识", f.Name, f.Type)
}

// ConfirmKind 是危险命令的 confirm 口径：object 填对象名（取某个位置参数的值），count 填本次受影响数量。
type ConfirmKind string

const (
	ConfirmObject ConfirmKind = "object"
	ConfirmCount  ConfirmKind = "count"
)

// Confirm 登记危险命令要怎么确认。Arg 是 object 口径下作对象名的位置参数名。
type Confirm struct {
	Kind ConfirmKind
	Arg  string
}

// REST 是一条命令的 HTTP 映射：方法与 ServeMux 风格的路径模板（位置参数写成 {name}）。
type REST struct {
	Method string
	Path   string
}

// Command 是命令表里的一条记录。
type Command struct {
	// Path 是命令路径的各段，如 ["audit","list"]。
	Path    []string
	Summary string
	Args    []Arg
	Flags   []Flag
	Class   Class
	// Danger 为空表示不是危险操作。
	Danger v1.Danger
	// Confirm 只在 Danger 非空时登记。
	Confirm *Confirm
	// HumanOnly 是第 05 章七组人类专属操作：任何令牌都拒绝，本机管理员与用户要当场验证。
	HumanOnly bool
	// List 表示列表命令：一律分页，REST 用 GET 并去掉末尾的 list 段。
	List bool
	// Hidden 的命令不出现在帮助里（如 __verify）。
	Hidden bool
	// Offline 表示不依赖主控状态：CLI 本地作答、不进审计（explain）。
	Offline bool
	// Columns 是列表命令文本输出的列；没登记就用 item 的 JSON 键排序。
	Columns []string
	// REST 非空时覆盖默认映射。
	REST *REST
}

// Name 是命令路径各段用空格连起来的名字，也是 Bindings 与审计记录里的 command。
func (c *Command) Name() string { return strings.Join(c.Path, " ") }

// Scope 返回命令所需的权限范围；local 命令返回 false。
func (c *Command) Scope() (v1.Scope, bool) { return c.Class.Scope() }

// Route 返回命令的 REST 映射：登记了 REST 就用它，否则按默认规则推——
// read 用 GET、其余用 POST；路径是 /api/v1/ 加各段（列表命令去掉末尾 list），位置参数依次追加为 {name}。
func (c *Command) Route() REST {
	if c.REST != nil {
		return *c.REST
	}
	segs := c.Path
	if c.List && len(segs) > 1 && segs[len(segs)-1] == "list" {
		segs = segs[:len(segs)-1]
	}
	path := APIPrefix + strings.Join(segs, "/")
	for _, a := range c.Args {
		path += "/{" + a.Name + "}"
	}
	method := "POST"
	if c.Class == ClassRead {
		method = "GET"
	}
	return REST{Method: method, Path: path}
}

// APIPrefix 是管理 REST 的路径前缀。
const APIPrefix = "/api/v1/"

// FlagByName 按名字找 flag。
func (c *Command) FlagByName(name string) (Flag, bool) {
	for _, f := range c.Flags {
		if f.Name == name {
			return f, true
		}
	}
	return Flag{}, false
}

// FileFlags 返回 file 类型的 flag 名，REST 与 MCP 按它拒绝。
func (c *Command) FileFlags() []string {
	var names []string
	for _, f := range c.Flags {
		if f.Type == TypeFile {
			names = append(names, f.Name)
		}
	}
	return names
}

// RequiredArgs 返回必填位置参数的个数。
func (c *Command) RequiredArgs() int {
	n := 0
	for _, a := range c.Args {
		if !a.Optional {
			n++
		}
	}
	return n
}

var (
	segmentRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$|^__[a-z]+$`)
	nameRe    = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

// validate 检查单条记录自身的规则；跨记录的规则（路径唯一、前缀）在 Table 里查。
func (c *Command) validate() error {
	name := c.Name()
	if len(c.Path) == 0 || len(c.Path) > 3 {
		return fmt.Errorf("命令 %q 的路径要一到三段", name)
	}
	for _, seg := range c.Path {
		if !segmentRe.MatchString(seg) {
			return fmt.Errorf("命令 %q 的路径段 %q 不合法（小写字母、数字、连字符）", name, seg)
		}
	}
	if strings.TrimSpace(c.Summary) == "" {
		return fmt.Errorf("命令 %q 没有一句话说明", name)
	}
	if !validClasses[c.Class] {
		return fmt.Errorf("命令 %q 的类别 %q 不认识", name, c.Class)
	}
	seen := map[string]bool{}
	sawOptional := false
	for _, a := range c.Args {
		if !nameRe.MatchString(a.Name) {
			return fmt.Errorf("命令 %q 的位置参数名 %q 不合法", name, a.Name)
		}
		if seen[a.Name] {
			return fmt.Errorf("命令 %q 的参数名 %q 重复", name, a.Name)
		}
		seen[a.Name] = true
		if sawOptional && !a.Optional {
			return fmt.Errorf("命令 %q 的必填位置参数 %q 不能排在可选参数之后", name, a.Name)
		}
		sawOptional = sawOptional || a.Optional
	}
	for _, f := range c.Flags {
		if !nameRe.MatchString(f.Name) {
			return fmt.Errorf("命令 %q 的 flag 名 %q 不合法", name, f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("命令 %q 的参数名 %q 重复", name, f.Name)
		}
		seen[f.Name] = true
		if !validFlagTypes[f.Type] {
			return fmt.Errorf("命令 %q 的 flag %s 类型 %q 不认识", name, f.Name, f.Type)
		}
		if reservedFlags[f.Name] {
			return fmt.Errorf("命令 %q 的 flag %s 与投影层保留的 flag 撞名", name, f.Name)
		}
	}
	if c.Danger != "" && !validDanger(c.Danger) {
		return fmt.Errorf("命令 %q 的危险类 %q 不认识", name, c.Danger)
	}
	if c.Class == ClassLocal {
		if c.Danger != "" || c.HumanOnly || c.REST != nil || c.List || c.Offline {
			return fmt.Errorf("命令 %q 是本地命令，不能登记危险类、人类专属、列表、离线或 REST 映射", name)
		}
	}
	if c.Danger != "" && c.Confirm == nil {
		return fmt.Errorf("命令 %q 登记了危险类却没有 confirm 口径", name)
	}
	if c.Danger == "" && c.Confirm != nil {
		return fmt.Errorf("命令 %q 没有危险类却登记了 confirm 口径", name)
	}
	if c.Confirm != nil {
		switch c.Confirm.Kind {
		case ConfirmObject:
			if _, ok := c.argByName(c.Confirm.Arg); !ok {
				return fmt.Errorf("命令 %q 的 confirm 口径 object 指向的位置参数 %q 不存在", name, c.Confirm.Arg)
			}
		case ConfirmCount:
		default:
			return fmt.Errorf("命令 %q 的 confirm 口径 %q 不认识", name, c.Confirm.Kind)
		}
	}
	if c.List && c.Class != ClassRead {
		return fmt.Errorf("命令 %q 是列表命令，类别必须是 read", name)
	}
	if c.HumanOnly && (c.Class == ClassRead || c.Class == ClassLocal) {
		return fmt.Errorf("命令 %q 是人类专属，类别不能是 read 或 local", name)
	}
	if c.REST != nil {
		if c.REST.Method != "GET" && c.REST.Method != "POST" {
			return fmt.Errorf("命令 %q 的 REST 方法 %q 只能是 GET 或 POST", name, c.REST.Method)
		}
		if !strings.HasPrefix(c.REST.Path, APIPrefix) {
			return fmt.Errorf("命令 %q 的 REST 路径 %q 必须以 %s 开头", name, c.REST.Path, APIPrefix)
		}
		if _, err := url.Parse(c.REST.Path); err != nil {
			return fmt.Errorf("命令 %q 的 REST 路径 %q 不合法", name, c.REST.Path)
		}
	}
	return nil
}

func (c *Command) argByName(name string) (Arg, bool) {
	for _, a := range c.Args {
		if a.Name == name {
			return a, true
		}
	}
	return Arg{}, false
}

// reservedFlags 是投影层自己加的 flag，命令不许再登记：--json、--data-dir 是 CLI 的全局 flag，
// --confirm 随危险类自动加，--limit / --cursor 随列表命令自动加，--server / --token 是 m1-04 的客户端 flag。
var reservedFlags = map[string]bool{"json": true, "data-dir": true, "confirm": true, "limit": true, "cursor": true, "server": true, "token": true, "help": true}

// ClientOnlyFlags 是只属于 CLI 客户端的根 flag：主控端（REST、MCP）见到它们一律拒绝——身份只来自连接。
var ClientOnlyFlags = []string{"server", "token"}

// FilePathFlags 是 CLI 惯用的文件路径 flag 名（含短名 -f），REST 与 MCP 上一律拒绝、不打开任何路径。
var FilePathFlags = []string{"f", "filename", "file"}

func validDanger(d v1.Danger) bool {
	for _, want := range v1.AllDangers {
		if want == d {
			return true
		}
	}
	return false
}

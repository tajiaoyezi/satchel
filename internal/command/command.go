package command

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
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

// FlagType 是 flag 的取值类型。file 是 CLI 本地读取的文件路径，只有 CLI 投影接受；
// password 是 CLI 从终端读、不作为命令行参数存在的值（REST 从请求体读），审计摘要与日志里恒打码；
// object 是「字段名到值」的一个对象：CLI 上是可重复的 --flag 字段=值（值原样是字符串，由处理函数按字段类型解析），
// REST 从请求体收 JSON 对象，登记时必须指明所属的 kind（Flag.Kind），审计摘要按该 kind 的打码字段集合逐键打码。
type FlagType string

const (
	TypeString   FlagType = "string"
	TypeInt      FlagType = "int"
	TypeBool     FlagType = "bool"
	TypeDuration FlagType = "duration"
	TypeStrings  FlagType = "strings" // 可重复
	TypeFile     FlagType = "file"
	TypePassword FlagType = "password"
	TypeObject   FlagType = "object"
)

var validFlagTypes = map[FlagType]bool{TypeString: true, TypeInt: true, TypeBool: true, TypeDuration: true, TypeStrings: true, TypeFile: true, TypePassword: true, TypeObject: true}

// writeClasses 是有写路径的类别：password 与 object 类型的 flag 只能登记在这些类别的命令上。
var writeClasses = map[Class]bool{ClassConfig: true, ClassAction: true, ClassMasterSettings: true}

// Arg 是位置参数。
type Arg struct {
	Name        string
	Description string
	Optional    bool
}

// Flag 是命令的一个 flag。Secret 标密钥类：审计摘要与日志里打码（password 类型隐含）。表里的 flag 一律没有短名。
// Kind 只给 object 类型：字段名只能是该 kind 的字段，explain 报出来，审计按它的打码字段集合打码。
type Flag struct {
	Name        string
	Type        FlagType
	Default     string
	Description string
	Secret      bool
	Kind        v1.Kind
}

// Masked 报告这个 flag 的值在摘要与日志里要不要打码：标了 Secret 或类型是 password。
func (f Flag) Masked() bool { return f.Secret || f.Type == TypePassword }

// Parse 把命令行或查询参数里的字符串按类型解析。strings 类型由调用方逐个值调它；
// object 类型没有单个字符串的形式（CLI 用 ObjectFromPairs 把多个 字段=值 拼成对象），到这里是内部错误。
func (f Flag) Parse(raw string) (any, error) {
	switch f.Type {
	case TypeString, TypeFile, TypeStrings, TypePassword:
		return raw, nil
	case TypeObject:
		return nil, v1.Newf(v1.CodeInternal, "参数 %s 是对象，不能从单个字符串解析", f.Name)
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

// FromJSON 把 JSON 请求体里解出来的值按类型校验、归一（int 从 float64 或 json.Number 收窄，duration 从字符串解析）。
// 错误文案里的值按 JSON 写法给出，字符串带引号，"1" 与 1 分得清。
func (f Flag) FromJSON(v any) (any, error) {
	bad := func() (any, error) {
		return nil, v1.Newf(v1.CodeBadRequest, "参数 %s 的值 %s 不是 %s", f.Name, jsonText(v), f.Type)
	}
	switch f.Type {
	case TypeString, TypeFile, TypePassword:
		s, ok := v.(string)
		if !ok {
			return bad()
		}
		return s, nil
	case TypeInt:
		const (
			maxIntPlusOne = float64(uint64(1) << (strconv.IntSize - 1))
			minInt        = -maxIntPlusOne
		)
		switch n := v.(type) {
		case float64:
			if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n < minInt || n >= maxIntPlusOne {
				return bad()
			}
			return int(n), nil
		case json.Number:
			i, err := strconv.Atoi(n.String())
			if err != nil {
				return bad()
			}
			return i, nil
		}
		return bad()
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
	case TypeObject:
		obj, ok := v.(map[string]any)
		if !ok {
			return nil, v1.Newf(v1.CodeBadRequest, "参数 %s 必须是一个 JSON 对象（字段名到值）", f.Name)
		}
		return obj, nil
	}
	return nil, v1.Newf(v1.CodeInternal, "参数 %s 的类型 %s 不认识", f.Name, f.Type)
}

// jsonText 把一个值按 JSON 写法编成文本给错误文案用；编不了的退回 %v。
func jsonText(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(raw)
}

// ObjectFromPairs 把 CLI 上可重复给出的 字段=值 拼成 object 类型的值：按第一个 = 拆，值原样是字符串（可以含 =）。
// 没有 = 的片段、同一个字段给两次都是用法错误（usage，退出码 2）。
func ObjectFromPairs(flag string, pairs []string) (map[string]any, error) {
	obj := make(map[string]any, len(pairs))
	for _, p := range pairs {
		key, value, ok := strings.Cut(p, "=")
		if !ok || key == "" {
			return nil, v1.Newf(v1.CodeUsage, "用法错误：参数 --%s 的值 %q 要写成 字段=值", flag, p).
				WithNext("例如 --" + flag + " heartbeat_interval=30")
		}
		if _, dup := obj[key]; dup {
			return nil, v1.Newf(v1.CodeUsage, "用法错误：参数 --%s 里字段 %s 给了两次", flag, key)
		}
		obj[key] = value
	}
	return obj, nil
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

// Shape 是命令的执行形状（master-command-table、master-jobs、master-backup）：普通命令是空。
// 长任务受理后立刻返回 job；上传类命令的 REST 请求体是文件本身（来自唯一的 file 类型 flag --file）；
// 下载类命令成功时的结果是文件本身（*File，CLI 写到唯一的 file 类型 flag --output）。
type Shape string

const (
	ShapeJob      Shape = "job"
	ShapeUpload   Shape = "upload"
	ShapeDownload Shape = "download"
)

// UploadFlag 与 DownloadFlag 是上传类、下载类命令那个唯一的 file 类型 flag 的名字；NoWaitFlag 是长任务自动带的保留 flag。
const (
	UploadFlag   = "file"
	DownloadFlag = "output"
	NoWaitFlag   = "no-wait"
)

// File 是下载类命令的结果：REST 把它流式写出，CLI 写到本地文件。
type File struct {
	Name        string
	ContentType string
	Size        int64
	Open        func() (io.ReadCloser, error)
}

// Command 是命令表里的一条记录。
type Command struct {
	// Path 是命令路径的各段，如 ["audit","list"]。
	Path    []string
	Summary string
	Args    []Arg
	Flags   []Flag
	Class   Class
	// Danger 为空表示不是静态危险操作。
	Danger v1.Danger
	// Confirm 只在 Danger 非空时登记。
	Confirm *Confirm
	// DynamicDanger 表示所需危险类与 confirm 口径由业务层在运行时动态判定（如 apply 按 Plan 内容与滚动窗口判定）。
	// 标了 DynamicDanger 的命令不能同时设静态 Danger、Anonymous、ClassLocal 或 ClassRead，投影层自动暴露保留 flag --confirm。
	DynamicDanger bool
	// HumanOnly 是第 05 章七组人类专属操作：任何令牌都拒绝，本机管理员与用户要当场验证
	// （请求里自动带保留 flag verify-password / verify-code / verify-user，见 VerifyFlags）。
	HumanOnly bool
	// DynamicHuman 是条件人类专属操作（如 apply 及可能改写主控同机节点 nginx 的命令）：
	// 与 HumanOnly 互斥，投影层自动暴露 verify-* 保留参数；仅当运行时命中同机节点等人类专属条件时，
	// 业务层通过 HumanVerifier 强制执行当场验证（令牌一律返 human_required），远程节点对合规令牌照常放行。
	DynamicHuman bool
	// Anonymous 表示不要身份也能调（只给初始化向导的 setup status / setup init 用）：authz 跳过身份检查，MCP 一律拒绝，审计照记。
	Anonymous bool
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
	// Shape 是执行形状；空是普通命令。
	Shape Shape
}

// Name 是命令路径各段用空格连起来的名字，也是 Bindings 与审计记录里的 command。
func (c *Command) Name() string { return strings.Join(c.Path, " ") }

// HasConfirm 报告这条命令是否需要投影层自动暴露保留 flag --confirm（静态危险类或动态危险类）。
func (c *Command) HasConfirm() bool { return c.Danger != "" || c.DynamicDanger }

// HasVerify 报告这条命令是否需要投影层自动暴露当场验证保留 flag（静态人类专属或条件人类专属）。
func (c *Command) HasVerify() bool { return c.HumanOnly || c.DynamicHuman }

// HumanVerifier 是第 05 章七组的当场验证入口：由 service/auth.Verifier 实现，
// 既给 authz 处理静态 HumanOnly，也给业务服务处理 DynamicHuman（如主控同机节点 nginx 写操作）。
type HumanVerifier interface {
	Verify(ctx context.Context, inv *Invocation) error
}

// VerifyDynamicHuman 对条件人类专属命令执行一次性当场验证：
// 令牌与系统身份一律返回 human_required；管理员会话与本机管理员调用 verifier 当场验密与第二因素。
func VerifyDynamicHuman(ctx context.Context, verifier HumanVerifier, cmdName, detail string, inv *Invocation) error {
	id := v1.IdentityFrom(ctx)
	if id.ActorKind == v1.ActorToken || id.ActorKind == v1.ActorSystem {
		return v1.Newf(v1.CodeHumanRequired, "%s（%s）属于只有人能做的操作，令牌一律不能做", cmdName, detail).
			WithNext(HumanOnlyNext(cmdName))
	}
	if verifier == nil {
		return v1.Newf(v1.CodeHumanRequired, "%s（%s）属于只有人能做的操作，需要当场验证身份，但验证入口未装配", cmdName, detail)
	}
	return verifier.Verify(ctx, inv)
}

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

// IsFilePathFlag 报告 name 在这条命令上算不算文件路径参数（REST 与 MCP 按它拒绝、不打开任何路径）：命令登记了这个名字时，
// 只有 file 类型才算；没登记的 CLI 惯用文件路径名（FilePathFlags）算。命令把这几个名字登记成别的类型时照常解析
// （如 logs list 的 file 是日志文件的名字）。
func (c *Command) IsFilePathFlag(name string) bool {
	if f, ok := c.FlagByName(name); ok {
		return f.Type == TypeFile
	}
	for _, n := range FilePathFlags {
		if n == name {
			return true
		}
	}
	return false
}

// FileFlags 返回 file 类型的 flag 名。
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
	argNameRe = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
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
		// 位置参数名还是 REST 路径模板里的通配名（ServeMux 只认 Go 标识符），所以不许有连字符。
		if !argNameRe.MatchString(a.Name) {
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
		// 本地命令没有危险类与人类专属，投影层不会给它自动加 confirm 与 verify-*；需要时它自己登记
		// （admin reset-password 的 --confirm，mcp init 替 token create 带的 --verify-user / --verify-code）。
		if reservedFlags[f.Name] && !(c.Class == ClassLocal && localSelfRegistered[f.Name]) {
			return fmt.Errorf("命令 %q 的 flag %s 与投影层保留的 flag 撞名", name, f.Name)
		}
		if f.Name == "force" {
			// 仅 config 类别、标了 DynamicDanger 且路径为 apply 的命令允许登记 bool 型 --force（跳过乐观锁版本号检查、需权限危险类并进审计）。
			if !(name == "apply" && c.Class == ClassConfig && c.DynamicDanger && f.Type == TypeBool) {
				return fmt.Errorf("命令 %q 不能登记 --force：force 覆盖版本校验归危险操作的权限类，要 confirm 并进审计（第 05 章、第 07 章），仅允许 apply 命令登记", name)
			}
		}
	}
	if c.Danger != "" && !validDanger(c.Danger) {
		return fmt.Errorf("命令 %q 的危险类 %q 不认识", name, c.Danger)
	}
	if c.DynamicDanger {
		if c.Class == ClassLocal || c.Class == ClassRead {
			return fmt.Errorf("命令 %q 标了动态危险类，类别不能是 read 或 local", name)
		}
		if c.Danger != "" {
			return fmt.Errorf("命令 %q 不能同时登记静态危险类与动态危险类", name)
		}
		if c.Anonymous {
			return fmt.Errorf("命令 %q 标了不要身份，不能同时是动态危险类", name)
		}
	}
	for _, f := range c.Flags {
		if (f.Type == TypePassword || f.Type == TypeObject) && !writeClasses[c.Class] {
			return fmt.Errorf("命令 %q 的 flag %s 是 %s 类型，只能登记在有写路径的命令上（不能是 read 或 local）", name, f.Name, f.Type)
		}
		if f.Type == TypeObject {
			if _, ok := v1.Lookup(f.Kind); !ok {
				return fmt.Errorf("命令 %q 的 flag %s 是 object 类型，所属的 kind %q 不在 kind 清单里", name, f.Name, f.Kind)
			}
		} else if f.Kind != "" {
			return fmt.Errorf("命令 %q 的 flag %s 不是 object 类型，不能登记 kind", name, f.Name)
		}
	}
	if c.Anonymous {
		if c.Class != ClassRead && c.Class != ClassAction {
			return fmt.Errorf("命令 %q 标了不要身份，类别只能是 read 或 action", name)
		}
		if c.HumanOnly || c.DynamicHuman || c.Danger != "" {
			return fmt.Errorf("命令 %q 标了不要身份，不能同时是人类专属、条件人类专属或危险类", name)
		}
	}
	if c.Class == ClassLocal {
		if c.Danger != "" || c.DynamicDanger || c.HumanOnly || c.DynamicHuman || c.REST != nil || c.List || c.Offline {
			return fmt.Errorf("命令 %q 是本地命令，不能登记危险类、人类专属、条件人类专属、列表、离线或 REST 映射", name)
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
	if c.DynamicHuman {
		if c.Class == ClassRead || c.Class == ClassLocal {
			return fmt.Errorf("命令 %q 是条件人类专属，类别不能是 read 或 local", name)
		}
		if c.HumanOnly {
			return fmt.Errorf("命令 %q 不能同时登记人类专属与条件人类专属", name)
		}
	}
	if err := c.validateShape(); err != nil {
		return err
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

// validateShape 检查执行形状的约束：只标在 action、非列表的命令上；上传类恰好一个 file 类型的 --file、不能是人类专属
// （请求体被文件占了，当场验证的密码放不进去）；下载类恰好一个 file 类型的 --output。普通命令的 file 类型 flag 照旧是
// 「CLI 读出内容内联发给主控」（M2 的 plan -f），不受这里限制。
func (c *Command) validateShape() error {
	name := c.Name()
	files := c.FileFlags()
	switch c.Shape {
	case "":
		return nil
	case ShapeJob, ShapeUpload, ShapeDownload:
	default:
		return fmt.Errorf("命令 %q 的执行形状 %q 不认识", name, c.Shape)
	}
	if c.Class != ClassAction || c.List {
		return fmt.Errorf("命令 %q 标了执行形状 %s，只能是 action 类别、非列表的命令", name, c.Shape)
	}
	want := ""
	switch c.Shape {
	case ShapeUpload:
		if c.HasVerify() {
			return fmt.Errorf("命令 %q 是上传类，不能是人类专属或条件人类专属：请求体被文件占了，当场验证的密码放不进去", name)
		}
		want = UploadFlag
	case ShapeDownload:
		want = DownloadFlag
	}
	if want == "" {
		if len(files) > 0 {
			return fmt.Errorf("命令 %q 是长任务，不能登记 file 类型的 flag %s", name, files[0])
		}
		return nil
	}
	if len(files) != 1 || files[0] != want {
		return fmt.Errorf("命令 %q 是%s类，要恰好一个 file 类型的 flag --%s，得到 %v", name, map[Shape]string{ShapeUpload: "上传", ShapeDownload: "下载"}[c.Shape], want, files)
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
// --confirm 随危险类自动加，--limit / --cursor 随列表命令自动加，verify-* 随人类专属自动加，--no-wait 随长任务自动加，--server / --token 是 m1-04 的客户端 flag。
var reservedFlags = map[string]bool{"json": true, "data-dir": true, "confirm": true, "limit": true, "cursor": true, "server": true, "token": true, "help": true, NoWaitFlag: true,
	VerifyPasswordFlag: true, VerifyCodeFlag: true, VerifyUserFlag: true}

// localSelfRegistered 是本地命令可以自己登记的保留 flag 名。
var localSelfRegistered = map[string]bool{"confirm": true, VerifyUserFlag: true, VerifyCodeFlag: true}

// 人类专属命令自动带的当场验证保留 flag（master-human-verification）：值不进 Invocation.Flags，进 Invocation.Verify，永不进审计摘要。
const (
	VerifyPasswordFlag = "verify-password"
	VerifyCodeFlag     = "verify-code"
	VerifyUserFlag     = "verify-user"
)

// VerifyFlags 是三个保留 flag 的名字，投影层登记与解码时用同一份。
var VerifyFlags = []string{VerifyPasswordFlag, VerifyCodeFlag, VerifyUserFlag}

// RootFlag 是 CLI 根命令上的一个全局 flag（help 由 cobra 给每条命令自带，不在这里）。
// ClientOnly 表示它只属于 CLI 客户端：主控端（REST、MCP）见到一律拒绝——身份只来自连接，数据目录是主控自己的。
type RootFlag struct {
	Name       string
	Type       FlagType
	ClientOnly bool
}

// RootFlags 是 CLI 根命令上的全局 flag，只此一份：CLI 投影按它登记，ClientOnlyFlags 由它推出，skills 的检查按它认根 flag。
var RootFlags = []RootFlag{
	{Name: "json", Type: TypeBool},
	{Name: "server", Type: TypeString, ClientOnly: true},
	{Name: "token", Type: TypeString, ClientOnly: true},
	{Name: "data-dir", Type: TypeString, ClientOnly: true},
}

// ClientOnlyFlags 是只属于 CLI 客户端的根 flag（--data-dir 现在就有，--server / --token 随 m1-04 登记），由 RootFlags 推出：
// 主控端（REST、MCP）见到它们一律拒绝。
var ClientOnlyFlags = clientOnlyFlags()

func clientOnlyFlags() []string {
	var names []string
	for _, f := range RootFlags {
		if f.ClientOnly {
			names = append(names, f.Name)
		}
	}
	return names
}

// ConnectingLocal 是会连主控的本地命令：只有它们接受显式的 --server / --token（master-cli「本地命令不接受 --server 与 --token」）。
// 别的本地命令也从根上继承了这两个 flag，执行时一律以 usage 拒绝。
var ConnectingLocal = []string{"login", "mcp stdio", "mcp init"}

// IsConnectingLocal 报告 name 是不是会连主控的本地命令。
func IsConnectingLocal(name string) bool {
	for _, n := range ConnectingLocal {
		if n == name {
			return true
		}
	}
	return false
}

// CLIFlags 是一条命令在 CLI 上接受的全部 flag 与类型（不含 cobra 自带的 help）：
//   - 登记的 flag 去掉 password 类型——密码只从终端读，CLI 不登记它；
//   - 按命令属性自动带的保留 flag：危险类加 confirm，人类专属加 verify-code 与 verify-user（verify-password 同样只从终端读），
//     长任务加 no-wait，列表命令加 limit 与 cursor；
//   - 根 flag，其中 server、token 只给经主控的命令与会连主控的本地命令（别的本地命令执行时拒绝它们）。
//
// skills 的检查按它认 flag；CLI 投影有测试核对它与 cobra 树上实际登记的一致。
func CLIFlags(c *Command) []Flag {
	var out []Flag
	for _, f := range c.Flags {
		if f.Type != TypePassword {
			out = append(out, Flag{Name: f.Name, Type: f.Type})
		}
	}
	if c.HasConfirm() {
		out = append(out, Flag{Name: "confirm", Type: TypeString})
	}
	if c.HasVerify() {
		out = append(out, Flag{Name: VerifyCodeFlag, Type: TypeString}, Flag{Name: VerifyUserFlag, Type: TypeString})
	}
	if c.Shape == ShapeJob {
		out = append(out, Flag{Name: NoWaitFlag, Type: TypeBool})
	}
	if c.List {
		out = append(out, Flag{Name: "limit", Type: TypeInt}, Flag{Name: "cursor", Type: TypeString})
	}
	for _, r := range RootFlags {
		if (r.Name == "server" || r.Name == "token") && c.Class == ClassLocal && !IsConnectingLocal(c.Name()) {
			continue
		}
		out = append(out, Flag{Name: r.Name, Type: r.Type})
	}
	return out
}

// ClientOnlyCommands 是只在 CLI 里有意义的命令路径的首段（第 05 章：login、mcp init 在 satchel_run 的拒绝清单里）：
// MCP 按首段拒绝。login、logout、mcp stdio、mcp init 是本地命令；mcp status 是读命令，同样按首段拒绝（token list 在 MCP 上可得）。
var ClientOnlyCommands = []string{"login", "logout", "mcp"}

// FilePathFlags 是 CLI 惯用的文件路径 flag 名（含短名 -f）：命令没有登记它们时，REST 与 MCP 上拒绝、不打开任何路径
// （见 IsFilePathFlag）。
var FilePathFlags = []string{"f", "filename", "file"}

func validDanger(d v1.Danger) bool {
	for _, want := range v1.AllDangers {
		if want == d {
			return true
		}
	}
	return false
}

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// mcp init（master-mcp「mcp init 把一个 runtime 接上主控」）分三步，前一步不过不做后一步：
// ① 检查：runtime 名、写进配置的主控地址、把要改的每个文件用占位令牌完整算一遍（解析不了、写法不支持就停，此时还没签发令牌）；
// ② 取令牌：--use-token 从终端读一把已有的，否则经 CLI 当前的连接调 token create（人类专属，当场验证从终端读）；
// ③ 写配置：先备份、再原子写；令牌签出来之后写失败是 partial_failure，输出带令牌明文与手工片段。--print 跳过这一步。

// placeholderToken 是预检用的占位令牌：形状与真令牌相同（sat_ 加 43 个 base64url 字符），算出来的改动与真令牌只差这一串。
var placeholderToken = "sat_" + strings.Repeat("x", 43)

// shownToken 是片段里代替令牌的字样：预检停下时还没有令牌。
const shownToken = "<令牌>"

// satchelEnv 是写进 runtime 环境的三个变量：CLI 的连接、令牌与 JSON 输出。
func satchelEnv(url, token string) [][2]string {
	return [][2]string{{EnvServer, url}, {EnvToken, token}, {"SATCHEL_OUTPUT", "json"}}
}

// runtimeEnv 是写 runtime 配置要用到的本机环境；测试里换成临时目录与假命令。
type runtimeEnv struct {
	home       string // 用户主目录
	codexHome  string // $CODEX_HOME，默认 ~/.codex
	hermesHome string // $HERMES_HOME，默认 ~/.hermes
	executable string // satchel 自己的绝对路径（claude mcp add 登记它）
	lookPath   func(string) (string, error)
	run        func(name string, args ...string) ([]byte, error)
}

func currentRuntimeEnv() (runtimeEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return runtimeEnv{}, v1.Wrap(v1.CodeConfig, "找不到用户主目录", err).WithNext("设置 HOME 后重试")
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return runtimeEnv{}, v1.Wrap(v1.CodeInternal, "找不到 satchel 可执行文件自己的路径", err)
	}
	env := runtimeEnv{home: home, executable: exe, lookPath: exec.LookPath, codexHome: os.Getenv("CODEX_HOME"), hermesHome: os.Getenv("HERMES_HOME"),
		run: func(name string, args ...string) ([]byte, error) { return exec.Command(name, args...).CombinedOutput() }}
	if env.codexHome == "" {
		env.codexHome = filepath.Join(home, ".codex")
	}
	if env.hermesHome == "" {
		env.hermesHome = filepath.Join(home, ".hermes")
	}
	return env, nil
}

// fileEdit 是对一个文件的改动：before 为 nil 表示文件还不存在；secret 表示改后的内容里有令牌明文
// （写入时去掉组与其他用户的权限）。
type fileEdit struct {
	path   string
	before []byte
	after  []byte
	secret bool
}

// initPlan 是一个 runtime 的全部改动：要写的文件、写完后要跑的命令、验证用的命令与给人的提示；
// previous 是跑命令之前 runtime 里原有的登记（claude mcp add 失败时交还给用户）。
type initPlan struct {
	edits    []fileEdit
	commands []initCommand
	verify   string
	notes    []string
	previous string
}

// initCommand 是写完文件后要跑的一条命令；mayFail 的失败不算错（claude mcp remove 在本来没有时会失败）。
type initCommand struct {
	args    []string
	mayFail bool
}

// runtimeSpec 是一个 runtime 的写法：plan 按主控地址与令牌算出改动（预检与正式写各算一次），snippet 是要手工加的片段。
type runtimeSpec struct {
	plan    func(env runtimeEnv, url, token string) (*initPlan, error)
	snippet func(env runtimeEnv, url, token string) string
}

var runtimeSpecs = map[string]runtimeSpec{
	"claude-code": {plan: planClaudeCode, snippet: snippetClaudeCode},
	"codex":       {plan: planCodex, snippet: snippetCodex},
	"hermes":      {plan: planHermes, snippet: snippetHermes},
}

// errUnsupported 是某个文件解析不了或写法不在支持范围内：planner 返回它，mcpInit 包成带片段的 config 错误。
type errUnsupported struct {
	path, why string
}

func (e errUnsupported) Error() string { return e.path + "：" + e.why }

func unsupportedf(path, format string, args ...any) error {
	return errUnsupported{path: path, why: fmt.Sprintf(format, args...)}
}

// runtimeTagRe 与 service/tokens 的 runtime 标签字符集相同。
var runtimeTagRe = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,64}$`)

// initToken 是 mcp init 用上的令牌：签发的或 --use-token 读的；明文只在 --print 的输出里出现。
type initToken struct {
	ID     *int64 `json:"id"`
	Name   string `json:"name,omitempty"`
	Preset string `json:"preset,omitempty"`
	Issued bool   `json:"issued"`
	Token  string `json:"token,omitempty"`
}

// initFile 是写过的一个文件：备份的路径；Mode 非空时说明权限被收紧了（如 0644 → 0600，文件里有令牌）。
type initFile struct {
	Path   string `json:"path"`
	Backup string `json:"backup,omitempty"`
	Mode   string `json:"mode,omitempty"`
}

// initOutput 是 mcp init 的输出。
type initOutput struct {
	Runtime  string     `json:"runtime"`
	URL      string     `json:"url"`
	Token    initToken  `json:"token"`
	Files    []initFile `json:"files"`
	Commands []string   `json:"commands"`
	Snippet  []string   `json:"snippet,omitempty"`
	Verify   string     `json:"verify"`
	Notes    []string   `json:"notes"`
	Printed  bool       `json:"printed"`
}

func mcpInit(ctx context.Context, inv *command.Invocation) (any, error) {
	env, err := currentRuntimeEnv()
	if err != nil {
		return nil, err
	}
	return runMCPInit(ctx, inv, env)
}

func runMCPInit(ctx context.Context, inv *command.Invocation, env runtimeEnv) (any, error) {
	// ① 检查。
	name := inv.String("runtime", "")
	spec, ok := runtimeSpecs[name]
	if !ok {
		return nil, usageError("--runtime 只能是 claude-code、codex 或 hermes，得到 %q", name)
	}
	conn, err := Connect(ctx)
	if err != nil {
		return nil, err
	}
	useToken, printOnly := inv.Bool("use-token"), inv.Bool("print")
	if !useToken {
		if err := canIssue(inv, conn); err != nil {
			return nil, err
		}
	}
	url, err := masterURL(ctx, inv, conn)
	if err != nil {
		return nil, err
	}
	tag := inv.String("name", "")
	if tag == "" {
		tag = defaultTag(name)
	}
	if !runtimeTagRe.MatchString(tag) {
		return nil, usageError("--name 只能是 1 到 64 个字母、数字或 . _ @ -（它同时是令牌的 runtime 标签），得到 %q", tag)
	}
	plaintextWarning(ctx, url, "runtime 以后会把令牌")
	if !printOnly {
		if _, err := spec.plan(env, url, placeholderToken); err != nil {
			return nil, preflightError(err, spec.snippet(env, url, shownToken))
		}
		if err := probeMaster(ctx, url); err != nil {
			return nil, err
		}
	}

	// ② 取令牌。新签的令牌先对写进配置的地址调一次 whoami，确认那里就是签发它的主控，再写配置。
	var tok initToken
	var plain string
	if useToken {
		plain, tok, err = readExistingToken(ctx, url)
	} else {
		plain, tok, err = issueToken(ctx, inv, conn, tag)
	}
	if err != nil {
		return nil, err
	}
	if tok.Issued && !printOnly {
		if err := verifyIssued(ctx, url, plain, *tok.ID); err != nil {
			return nil, v1.Newf(v1.CodePartialFailure, "令牌已签发（id %d），但写进 runtime 配置的地址 %s 不认这把令牌（%s），它多半不是签发令牌的那个主控；mcp init 没有写配置",
				*tok.ID, url, v1.AsError(err).Reason).WithNext(fmt.Sprintf("执行 satchel token revoke %d 吊销它，检查 --url 后重跑", *tok.ID))
		}
	}
	out := initOutput{Runtime: name, URL: url, Token: tok, Files: []initFile{}, Commands: []string{}, Notes: []string{}}

	// ③ 写配置。
	if printOnly {
		out.Printed = true
		out.Token.Token = plain
		out.Snippet = strings.Split(spec.snippet(env, url, plain), "\n")
		out.Verify, out.Notes = verifyAndNotes(name)
		return out, nil
	}
	plan, err := spec.plan(env, url, plain)
	removed := false
	if err == nil {
		out.Files, out.Commands, removed, err = applyPlan(env, plan)
	}
	if err != nil {
		previous := ""
		if removed {
			previous = plan.previous
		}
		return nil, writeFailure(err, tok, plain, spec.snippet(env, url, plain), out.Files, previous)
	}
	out.Verify, out.Notes = plan.verify, plan.notes
	return out, nil
}

// preflightError 把预检的失败包成 config：片段按行放进 state，文本形式也能直接照着加。
func preflightError(err error, snippet string) error {
	var u errUnsupported
	if errors.As(err, &u) {
		return v1.Newf(v1.CodeConfig, "%s：%s；mcp init 在签发令牌之前停下，没有改任何文件", u.path, u.why).
			WithState("snippet", strings.Split(snippet, "\n")).
			WithNext("按 state.snippet 手工加上（令牌用 satchel token create 签一把），或改好文件后重跑 satchel mcp init")
	}
	return err
}

// writeFailure 是令牌已到手、写配置却失败：签发的令牌是 partial_failure（带明文，免得签了没人知道）；--use-token 的是 config。
// previous 非空时是 runtime 里原有、已被 claude mcp remove 删掉的登记，交还给用户照着恢复。
func writeFailure(err error, tok initToken, plain, snippet string, written []initFile, previous string) error {
	reason := v1.AsError(err).Reason
	var u errUnsupported
	if errors.As(err, &u) {
		reason = u.Error()
	}
	var e *v1.Error
	if !tok.Issued {
		e = v1.Newf(v1.CodeConfig, "写 runtime 的配置失败：%s", reason).
			WithState("snippet", strings.Split(strings.ReplaceAll(snippet, plain, shownToken), "\n")).WithState("written", written).
			WithNext("按 state.snippet 手工加上")
	} else {
		e = v1.Newf(v1.CodePartialFailure, "令牌已签发（id %d），但写 runtime 的配置失败：%s", deref64(tok.ID), reason).
			WithState("token", plain).WithState("snippet", strings.Split(snippet, "\n")).WithState("written", written).
			WithNext(fmt.Sprintf("按 state.snippet 手工加上；不想要这把令牌就 satchel token revoke %d", deref64(tok.ID)))
	}
	if previous != "" {
		e = e.WithState("previous_registration", previous).
			WithNext(e.Next + "；原来的 satchel 登记已被删掉，内容在 state.previous_registration，可用 claude mcp add-json --scope user satchel '<它>' 恢复")
	}
	return e
}

// masterURL 决定写进 runtime 配置的主控地址：--url；不给就用 CLI 自己连的 server；都没有且本机 socket 在，
// 就按 serve 配置的监听地址推出本机地址（0.0.0.0 与 :: 推成 127.0.0.1）。
func masterURL(ctx context.Context, inv *command.Invocation, conn Connection) (string, error) {
	if raw, ok := inv.Flags["url"].(string); ok {
		u, err := normalizeServer(raw)
		if err != nil {
			return "", usageError("--url 的值 %q 不对：%s", raw, err)
		}
		return u, nil
	}
	if conn.Server != "" {
		return conn.Server, nil
	}
	if _, err := os.Stat(conn.Socket); err != nil {
		return "", v1.Newf(v1.CodeConfig, "不知道写进 runtime 配置的主控地址：没给 --url，没配远程主控，本机也没有主控的 socket（%s）", conn.Socket).
			WithNext("加上 --url <主控地址>，如 --url https://panel.example.com")
	}
	cfg, err := db.LoadServeConfig(DataDir(ctx), "")
	if err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return "", v1.Wrap(v1.CodeConfig, "serve 配置的监听地址 "+cfg.Listen+" 推不出本机地址", err).WithNext("加上 --url <主控地址>")
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

// defaultTag 是令牌名字与 runtime 标签的默认值 <runtime>@<主机名>：主机名里标签字符集以外的字符换成 -，总长截到 64。
func defaultTag(runtime string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	host = regexp.MustCompile(`[^A-Za-z0-9._-]`).ReplaceAllString(host, "-")
	tag := runtime + "@" + host
	if len(tag) > 64 {
		tag = tag[:64]
	}
	return tag
}

// readExistingToken 是 --use-token：从终端读一把已有的令牌，对写进配置的地址调一次 whoami，身份必须是令牌。
func readExistingToken(ctx context.Context, url string) (string, initToken, error) {
	raw, err := promptFrom(ctx)("已有的 API 令牌（输入不回显）")
	if errors.Is(err, ErrNoTerminal) {
		return "", initToken{}, v1.Wrap(v1.CodeBadRequest, "--use-token 要从终端读令牌，当前没有终端", err).
			WithNext("在有终端的会话里执行，或去掉 --use-token 让 mcp init 签一把新的")
	}
	if err != nil {
		return "", initToken{}, v1.Wrap(v1.CodeInternal, "从终端读取失败", err)
	}
	plain := strings.TrimSpace(raw)
	if err := checkToken(plain); err != nil {
		return "", initToken{}, v1.Newf(v1.CodeBadRequest, "输入的令牌不对：%s", err)
	}
	conn := Connection{Server: url, Token: plain}
	res, err := NewClient(tableOf(ctx), conn).Run(ctx, &command.Invocation{Path: []string{"whoami"}})
	if err != nil {
		return "", initToken{}, err
	}
	var id v1.Identity
	if raw, err := json.Marshal(res); err == nil {
		_ = json.Unmarshal(raw, &id)
	}
	if id.ActorKind != v1.ActorToken {
		return "", initToken{}, v1.Newf(v1.CodeUnauthenticated, "主控 %s 没有把这把令牌认成令牌身份（得到 %s）", url, id.ActorKind)
	}
	return plain, initToken{ID: id.TokenID}, nil
}

// canIssue 在签发之前、问密码之前检查 CLI 当前的连法：只有经本机 socket、不带令牌的本机管理员才签得了令牌
// （远程 CLI 没有人的身份，令牌不能签令牌），本机管理员还要用 --verify-user 指明令牌挂在哪个管理员名下。
func canIssue(inv *command.Invocation, conn Connection) error {
	if err := humanReachable("mcp init 签发令牌", conn); err != nil {
		return v1.AsError(err).WithNext("用 --use-token 输入一把已有的令牌；或在主控本机不带令牌、不带 --server 执行 satchel mcp init --verify-user <管理员用户名>")
	}
	if inv.String(command.VerifyUserFlag, "") == "" {
		return v1.New(v1.CodeHumanRequired, "本机管理员签发令牌要用 --verify-user 指明一个管理员账号，令牌挂在它名下；没有问密码").
			WithNext("加上 --verify-user <管理员用户名>")
	}
	return nil
}

// issueToken 经本机 socket 签一把令牌（canIssue 已经检查过连法）：token create 是人类专属命令，当场验证照常从终端读。
func issueToken(ctx context.Context, inv *command.Invocation, conn Connection, tag string) (string, initToken, error) {
	user := inv.String(command.VerifyUserFlag, "")
	ask := promptFrom(ctx)
	code := inv.String(command.VerifyCodeFlag, "")
	pw, err := ask("当场验证：请输入 " + user + " 的密码")
	if err == nil && code == "" {
		code, err = ask("两步验证码或恢复码（没开两步验证直接回车）")
	}
	if errors.Is(err, ErrNoTerminal) {
		return "", initToken{}, v1.Wrap(v1.CodeHumanRequired, "签发令牌要在终端里当场验证身份，当前没有终端", err).
			WithNext("在有终端的会话里执行，或用 --use-token 输入一把已有的令牌")
	}
	if err != nil {
		return "", initToken{}, v1.Wrap(v1.CodeInternal, "从终端读取失败", err)
	}
	create := &command.Invocation{Path: []string{"token", "create"},
		Flags:  map[string]any{"name": tag, "preset": inv.String("preset", "ops"), "runtime": tag},
		Verify: &command.Verification{Password: pw, Code: code, User: user}}
	res, err := NewClient(tableOf(ctx), conn).Run(ctx, create)
	if err != nil {
		return "", initToken{}, err
	}
	var created struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Preset string `json:"preset"`
		Token  string `json:"token"`
	}
	raw, _ := json.Marshal(res)
	if err := json.Unmarshal(raw, &created); err != nil || created.Token == "" {
		return "", initToken{}, v1.Newf(v1.CodeInternal, "token create 的输出里没有令牌：%s", raw)
	}
	return created.Token, initToken{ID: &created.ID, Name: created.Name, Preset: created.Preset, Issued: true}, nil
}

// probeMaster 在签发之前确认写进配置的地址上是一个 Satchel 主控：GET /api/v1/healthz 要是 200、status 为 ok、apiVersion 对得上。
// 不带任何凭据；它挡住写错、连不上与不是 Satchel 的地址。是不是签发令牌的那一台，由签发之后的 verifyIssued 核对。
func probeMaster(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+command.APIPrefix+"healthz", nil)
	if err != nil {
		return v1.Wrap(v1.CodeInternal, "构造请求失败", err)
	}
	client := Connection{Server: url}.HTTPClient()
	client.Timeout = 10 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return v1.Wrap(v1.CodeUnavailable, "连不上写进 runtime 配置的地址 "+url+"；mcp init 没有签发令牌", err).
			WithNext("检查 --url（或 CLI 连的主控地址）是否写对、主控是否在运行；主控用的是自签证书时，用 --print 拿到片段手工配置")
	}
	defer resp.Body.Close()
	var body struct {
		APIVersion string `json:"apiVersion"`
		Status     string `json:"status"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body) != nil ||
		body.Status != "ok" || body.APIVersion != v1.APIVersion {
		return v1.Newf(v1.CodeConfig, "写进 runtime 配置的地址 %s 上不是 Satchel 主控（%s 返回 %d）；mcp init 没有签发令牌", url, command.APIPrefix+"healthz", resp.StatusCode).
			WithNext("检查 --url 是否写对；主控挂在反代的子路径下时，地址要带上那段路径")
	}
	return nil
}

// verifyIssued 用刚签的令牌对写进配置的地址调一次 whoami：身份必须恰好是这把令牌，否则那个地址不是签发它的主控。
func verifyIssued(ctx context.Context, url, plain string, id int64) error {
	res, err := NewClient(tableOf(ctx), Connection{Server: url, Token: plain}).Run(ctx, &command.Invocation{Path: []string{"whoami"}})
	if err != nil {
		return err
	}
	var who v1.Identity
	if raw, err := json.Marshal(res); err == nil {
		_ = json.Unmarshal(raw, &who)
	}
	if who.ActorKind != v1.ActorToken || who.TokenID == nil || *who.TokenID != id {
		return v1.Newf(v1.CodeConfig, "whoami 返回的不是这把令牌的身份（actor_kind %s）", who.ActorKind)
	}
	return nil
}

// replacedTokenNote 是原来的配置里有一把别的令牌时给的提示：mcp init 不吊销它。
func replacedTokenNote(path string) string {
	return path + " 里原来有一把别的令牌，mcp init 没有吊销它；它若仍有效又不再用，用 satchel mcp status 或 token list 找到它的 id 后执行 satchel token revoke <id>（它的明文还留在备份里，备份权限 0600）"
}

// applyPlan 按计划写文件（改已有文件前先备份，原子写、保留原权限，新文件 0600、新目录 0700），再跑命令。
// 返回写过的文件（含备份）、跑过的命令，以及原有的登记是否已被删掉（mayFail 的那条 remove 成功了）；
// 失败时返回已经做过的那些。
func applyPlan(env runtimeEnv, plan *initPlan) (written []initFile, ran []string, removed bool, err error) {
	written, ran = []initFile{}, []string{}
	for _, e := range plan.edits {
		f, err := writeEdit(e)
		if err != nil {
			return written, ran, false, err
		}
		written = append(written, f)
	}
	for _, c := range plan.commands {
		line := strings.Join(c.args, " ")
		out, err := env.run(c.args[0], c.args[1:]...)
		switch {
		case err == nil && c.mayFail:
			removed = true
		case err != nil && !c.mayFail:
			return written, ran, removed, v1.Wrap(v1.CodeInternal, "执行 "+line+" 失败："+strings.TrimSpace(string(out)), err)
		}
		ran = append(ran, line)
	}
	return written, ran, removed, nil
}

func writeEdit(e fileEdit) (initFile, error) {
	path := e.path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved // 配置文件是符号链接（dotfiles 管理）时写到链接指向的文件，不把链接换成普通文件
	}
	f := initFile{Path: e.path}
	perm := fs.FileMode(0o600)
	if e.before != nil {
		info, err := os.Stat(path)
		if err != nil {
			return f, v1.Wrap(v1.CodeConfig, "读取 "+e.path+" 的权限失败", err)
		}
		perm = info.Mode().Perm()
		// 文件里会有令牌明文：去掉组与其他用户的权限（与登录文件同一个要求），属主的权限保留。
		if e.secret && perm&0o077 != 0 {
			f.Mode = fmt.Sprintf("%#o → %#o", perm, perm&^0o077)
			perm &^= 0o077
		}
		backup, err := backupFile(path, e.before)
		if err != nil {
			return f, v1.Wrap(v1.CodeConfig, "备份 "+e.path+" 失败", err)
		}
		f.Backup = backup
	} else if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return f, v1.Wrap(v1.CodeConfig, "建目录 "+filepath.Dir(path)+" 失败", err)
	}
	if err := writeFileAtomic(path, e.after, perm); err != nil {
		return f, v1.Wrap(v1.CodeConfig, "写 "+e.path+" 失败", err)
	}
	return f, nil
}

// backupFile 把改前的内容存成 <文件>.satchel-bak-<时间戳>（同一秒里重复就加序号），权限一律 0600：
// 改前的内容里可能有上一把令牌的明文。
func backupFile(path string, content []byte) (string, error) {
	base := path + ".satchel-bak-" + time.Now().Format("20060102-150405")
	for i := 0; ; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if _, err := f.Write(content); err != nil {
			f.Close()
			return "", err
		}
		if err := f.Chmod(0o600); err != nil {
			f.Close()
			return "", err
		}
		return name, f.Close()
	}
}

// readOptional 读一个可能不存在的文件：不存在返回 nil。
func readOptional(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, unsupportedf(path, "读不了（%v）", err)
	}
	if raw == nil {
		raw = []byte{}
	}
	return raw, nil
}

// verifyAndNotes 是 --print 时也要给的验证命令与提示（与各 runtime 的 plan 里的一致）。
func verifyAndNotes(runtime string) (string, []string) {
	switch runtime {
	case "claude-code":
		return claudeVerify, claudeNotes()
	case "codex":
		return codexVerify, codexNotes()
	}
	return hermesVerify, hermesNotes()
}

func renderMCPInit(w io.Writer, result any) error {
	out := result.(initOutput)
	var b strings.Builder
	if out.Printed {
		fmt.Fprintf(&b, "要给 %s 手工加的配置（没有写任何文件）：\n\n%s\n\n", out.Runtime, strings.Join(out.Snippet, "\n"))
	} else {
		fmt.Fprintf(&b, "已把 %s 接上主控 %s\n", out.Runtime, out.URL)
	}
	switch {
	case out.Token.Issued:
		fmt.Fprintf(&b, "令牌：#%d %s（%s）\n", deref64(out.Token.ID), out.Token.Name, out.Token.Preset)
	case out.Token.ID != nil:
		fmt.Fprintf(&b, "令牌：用的是已有的 #%d\n", *out.Token.ID)
	}
	for _, f := range out.Files {
		switch {
		case f.Backup != "" && f.Mode != "":
			fmt.Fprintf(&b, "改了 %s（备份 %s；文件里有令牌，权限从 %s）\n", f.Path, f.Backup, f.Mode)
		case f.Backup != "":
			fmt.Fprintf(&b, "改了 %s（备份 %s）\n", f.Path, f.Backup)
		default:
			fmt.Fprintf(&b, "新建 %s\n", f.Path)
		}
	}
	for _, c := range out.Commands {
		fmt.Fprintf(&b, "执行了 %s\n", c)
	}
	fmt.Fprintf(&b, "验证：%s\n", out.Verify)
	for _, n := range out.Notes {
		fmt.Fprintf(&b, "提示：%s\n", n)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func deref64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

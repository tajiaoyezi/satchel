// Package rest 是投影层的管理 REST 接口：路由从命令表推出，请求解码成 Invocation 交给执行链，
// 成功输出与 CLI --json 同一个对象，失败是四字段错误加折算的状态码（master-rest-api）。
// 用标准库 ServeMux；管理接口只有 /api/v1/，不兼容 mmwx 的 /api/admin/*。
package rest

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"

	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/service/auth"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// MaxBodyBytes 是 POST 请求体的上限。
const MaxBodyBytes = 1 << 20

// NewHandler 从命令表建路由。每条非本地命令一条路由（可选位置参数每少一个再登记一条），
// 加 GET /api/v1/healthz、三个会话入口（sessions 非 nil 时）与两个 404 兜底；方法不匹配也是 404 的 not_found。
// 同源检查不在这里：装配根用 SameOrigin 套在整棵路由外面（REST 与 /mcp 一起）。
func NewHandler(t *command.Table, runner command.Runner, sessions SessionAPI) http.Handler {
	mux := http.NewServeMux()
	for _, c := range t.Remote() {
		route := c.Route()
		for n := c.RequiredArgs(); n <= len(c.Args); n++ {
			mux.Handle(pathWithArgs(route.Path, c, n), commandHandler(c, route, n, runner, sessions))
		}
	}
	mux.HandleFunc(command.APIPrefix+"healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			WriteError(w, notFound(r))
			return
		}
		WriteResult(w, map[string]string{"status": "ok"})
	})
	if sessions != nil {
		mountSessionEndpoints(mux, sessions)
	}
	mux.HandleFunc(command.APIPrefix, func(w http.ResponseWriter, r *http.Request) { WriteError(w, notFound(r)) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { WriteError(w, notFound(r)) })
	return mux
}

// pathWithArgs 去掉路径模板末尾多余的可选参数段，只留前 n 个。
func pathWithArgs(path string, c *command.Command, n int) string {
	for i := len(c.Args) - 1; i >= n; i-- {
		path = strings.TrimSuffix(path, "/{"+c.Args[i].Name+"}")
	}
	return path
}

func notFound(r *http.Request) *v1.Error {
	return v1.Newf(v1.CodeNotFound, "没有这个接口：%s %s", r.Method, r.URL.Path).
		WithNext("接口清单见 satchel explain 或 docs/commands.md")
}

func commandHandler(c *command.Command, route command.REST, nargs int, runner command.Runner, sessions SessionAPI) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != route.Method {
			WriteError(w, notFound(r))
			return
		}
		inv, err := decode(c, r, nargs)
		if err != nil {
			WriteError(w, err)
			return
		}
		result, err := runner.Run(r.Context(), inv)
		if err != nil {
			WriteError(w, err)
			return
		}
		// 初始化向导建完第一个管理员顺手发一个会话，网页上接着往下走（master-setup-wizard）。这是唯一一处「命令结果后处理」。
		if sr, ok := result.(auth.SetupResult); ok && sessions != nil {
			token, expires, err := sessions.IssueSession(r.Context(), sr.Username, false)
			if err != nil {
				WriteError(w, err)
				return
			}
			setSessionCookie(w, r, token, expires)
		}
		WriteResult(w, result)
	})
}

// decode 把请求解成 Invocation：位置参数来自路径，GET 的 flag 来自查询参数，POST 的 flag 与 confirm 来自 JSON 体。
// 未登记的键、类型不对、文件路径类参数、太大的请求体都是 bad_request。
func decode(c *command.Command, r *http.Request, nargs int) (*command.Invocation, error) {
	inv := &command.Invocation{Path: c.Path, Flags: map[string]any{}}
	for i := 0; i < nargs; i++ {
		inv.Args = append(inv.Args, r.PathValue(c.Args[i].Name))
	}
	if c.List {
		inv.Page = &command.Page{}
	}
	if r.Method == http.MethodGet {
		return inv, decodeQuery(c, r, inv)
	}
	return inv, decodeBody(c, r, inv)
}

func fileFlagError(name string) *v1.Error {
	return v1.Newf(v1.CodeBadRequest, "参数 %s 是文件路径：文件只在 CLI 本地读取、内联后发给主控，主控不读服务端路径", name)
}

func isFileFlag(c *command.Command, name string) bool {
	for _, f := range c.FileFlags() {
		if f == name {
			return true
		}
	}
	for _, f := range command.FilePathFlags {
		if f == name {
			return true
		}
	}
	return false
}

func decodeQuery(c *command.Command, r *http.Request, inv *command.Invocation) error {
	for name, values := range r.URL.Query() {
		if isFileFlag(c, name) {
			return fileFlagError(name)
		}
		switch {
		case c.List && name == "limit":
			f := command.Flag{Name: "limit", Type: command.TypeInt}
			v, err := f.Parse(values[0])
			if err != nil {
				return err
			}
			// 显式给的 limit 就按给的算：0 与越界都拒绝，不静默换成默认值。
			if n := v.(int); n < 1 || n > command.MaxLimit {
				return v1.Newf(v1.CodeBadRequest, "limit 必须在 1 到 %d 之间，得到 %d", command.MaxLimit, n)
			}
			inv.Page.Limit = v.(int)
			continue
		case c.List && name == "cursor":
			inv.Page.Cursor = values[0]
			continue
		}
		f, ok := c.FlagByName(name)
		if !ok {
			return v1.Newf(v1.CodeBadRequest, "没有参数 %s", name).WithNext("参数清单见 satchel explain \"" + c.Name() + "\"")
		}
		if f.Type == command.TypeStrings {
			inv.Flags[name] = values
			continue
		}
		if len(values) > 1 {
			return v1.Newf(v1.CodeBadRequest, "参数 %s 只能给一次", name)
		}
		v, err := f.Parse(values[0])
		if err != nil {
			return err
		}
		inv.Flags[name] = v
	}
	return nil
}

func decodeBody(c *command.Command, r *http.Request, inv *command.Invocation) error {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return v1.New(v1.CodeBadRequest, "请求体必须是 application/json")
	}
	var body map[string]any
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, MaxBodyBytes))
	if err := dec.Decode(&body); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return v1.Newf(v1.CodeBadRequest, "请求体超过 %d 字节", MaxBodyBytes)
		}
		return v1.Wrap(v1.CodeBadRequest, "请求体不是合法的 JSON 对象", err)
	}
	if dec.More() {
		return v1.New(v1.CodeBadRequest, "请求体只能是一个 JSON 对象")
	}
	for name, raw := range body {
		if isFileFlag(c, name) {
			return fileFlagError(name)
		}
		if c.HumanOnly && isVerifyFlag(name) {
			// 当场验证的值进 inv.Verify，不进 Flags，永不进审计摘要；不是字符串就当没给。
			s, _ := raw.(string)
			if inv.Verify == nil {
				inv.Verify = &command.Verification{}
			}
			switch name {
			case command.VerifyPasswordFlag:
				inv.Verify.Password = s
			case command.VerifyCodeFlag:
				inv.Verify.Code = s
			case command.VerifyUserFlag:
				inv.Verify.User = s
			}
			continue
		}
		if name == "confirm" && c.Danger != "" {
			// 非字符串的 confirm 视为缺失（master-identity-and-authz「confirm 是字符串」），由 authz 报 confirm_required。
			if s, ok := raw.(string); ok {
				inv.Confirm = s
			}
			continue
		}
		f, ok := c.FlagByName(name)
		if !ok {
			return v1.Newf(v1.CodeBadRequest, "没有参数 %s", name).WithNext("参数清单见 satchel explain \"" + c.Name() + "\"")
		}
		v, err := f.FromJSON(raw)
		if err != nil {
			return err
		}
		inv.Flags[name] = v
	}
	return nil
}

func isVerifyFlag(name string) bool {
	for _, f := range command.VerifyFlags {
		if f == name {
			return true
		}
	}
	return false
}

// WriteResult 写成功响应：200 与带 apiVersion 的 JSON 对象。
func WriteResult(w http.ResponseWriter, result any) {
	out, err := v1.MarshalOutput(result)
	if err != nil {
		WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// WriteError 写四字段错误，状态码按 v1.HTTPStatusOf 折算。
func WriteError(w http.ResponseWriter, err error) {
	e := v1.AsError(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(v1.HTTPStatusOf(e.Code))
	_ = json.NewEncoder(w).Encode(e)
}

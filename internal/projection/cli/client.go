package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// socketPath 是数据目录下主控 unix socket 的路径。
func socketPath(dataDir string) string {
	return filepath.Join(dataDir, db.SocketFile)
}

// Client 是经主控执行命令的客户端：按命令表的 REST 映射构造请求发给主控，把响应或四字段错误原样带回。
// 连本机 socket 还是远程主控、带不带令牌由 Connection 决定（resolveConnection 按 flag、环境变量、登录文件取）。
type Client struct {
	table *command.Table
	conn  Connection
	http  *http.Client
}

// NewClient 按一种连法建客户端。
func NewClient(table *command.Table, conn Connection) *Client {
	return &Client{table: table, conn: conn, http: conn.HTTPClient()}
}

// Run 把调用对象发给主控。
func (c *Client) Run(ctx context.Context, inv *command.Invocation) (any, error) {
	cmd, ok := c.table.Lookup(inv.Name())
	if !ok {
		return nil, v1.Newf(v1.CodeInternal, "命令 %s 不在命令表里", inv.Name())
	}
	req, err := c.request(ctx, cmd, inv)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.unavailable(err)
	}
	if cmd.Shape == command.ShapeDownload && resp.StatusCode == http.StatusOK {
		// 下载类命令成功时 body 是文件本身：交给调用方流式写到本地，由它关。
		name := ""
		if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
			name = params["filename"]
		}
		return &command.File{Name: name, ContentType: resp.Header.Get("Content-Type"), Size: resp.ContentLength,
			Open: func() (io.ReadCloser, error) { return resp.Body, nil }}, nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, v1.Wrap(v1.CodeUnavailable, "读取主控响应失败", err).WithNext(c.conn.hint())
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, v1.Newf(v1.CodeConfig, "主控地址 %s 返回了重定向（%d，指向 %s），CLI 不跟随重定向", c.conn.BaseURL(), resp.StatusCode, resp.Header.Get("Location")).
			WithNext("把 --server、" + EnvServer + " 或登录文件里的地址改成重定向之后的那个（例如换成 https://）")
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out map[string]any
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, v1.Wrap(v1.CodeInternal, "主控返回的不是 JSON 对象", err)
		}
		return out, nil
	}
	var e v1.Error
	if err := json.Unmarshal(body, &e); err != nil || e.Code == "" {
		if c.conn.Server != "" && gatewayStatus(resp.StatusCode) {
			// 主控前面的代理在说后端不在或没按时回应：主控可能正在重启，也可能没在运行、或代理到主控的网络不通。
			// （socket 连法前面没有代理，不走这里。）
			next := "稍后重试；一直这样的话：" + c.conn.hint()
			if cmd.Class != command.ClassRead && cmd.Shape != command.ShapeDownload {
				next = "这条命令可能已经在主控上执行了，重试之前先查一下结果（长任务用 satchel job list）；" + next
			}
			return nil, v1.Wrap(v1.CodeUnavailable, fmt.Sprintf("%s 前面的代理返回了 %d，不是主控的四字段回应：主控可能正在重启、没在运行，或代理没等到它的回应",
				c.conn.BaseURL(), resp.StatusCode), unreachable{fmt.Errorf("HTTP %d", resp.StatusCode)}).WithNext(next)
		}
		return nil, v1.Newf(v1.CodeInternal, "主控返回了 %d，但不是四字段错误", resp.StatusCode)
	}
	return nil, &e
}

// request 按 REST 映射构造请求：位置参数替换路径模板里的 {name}（GET 的 flag 进查询参数，POST 的 flag、confirm 与当场验证的值进 JSON 体）。
func (c *Client) request(ctx context.Context, cmd *command.Command, inv *command.Invocation) (*http.Request, error) {
	route := cmd.Route()
	path := route.Path
	for i, a := range cmd.Args {
		val := inv.Arg(i)
		if val == "" && a.Optional {
			path = strings.TrimSuffix(path, "/{"+a.Name+"}")
			continue
		}
		path = strings.Replace(path, "{"+a.Name+"}", url.PathEscape(val), 1)
	}
	if route.Method == http.MethodGet || cmd.Shape == command.ShapeUpload {
		q := url.Values{}
		for name, v := range inv.Flags {
			for _, s := range stringValues(v) {
				q.Add(name, s)
			}
		}
		if inv.Page != nil {
			if inv.Page.Limit != 0 {
				q.Set("limit", strconv.Itoa(inv.Page.Limit))
			}
			if inv.Page.Cursor != "" {
				q.Set("cursor", inv.Page.Cursor)
			}
		}
		u := c.conn.BaseURL() + path
		if enc := q.Encode(); enc != "" {
			u += "?" + enc
		}
		if cmd.Shape == command.ShapeUpload {
			// 上传类命令：请求体是文件本身，flag 走查询参数（master-rest-api）。
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, inv.Body)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/zip")
			return req, nil
		}
		return http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	}
	body := map[string]any{}
	for name, v := range inv.Flags {
		if d, ok := v.(time.Duration); ok {
			v = d.String()
		}
		body[name] = v
	}
	if inv.Confirm != "" {
		body["confirm"] = inv.Confirm
	}
	// 当场验证的值（人类专属命令从终端读的）按 REST 的字段名进请求体，主控把它们放进 inv.Verify。
	if v := inv.Verify; v != nil {
		for name, val := range map[string]string{command.VerifyPasswordFlag: v.Password, command.VerifyCodeFlag: v.Code, command.VerifyUserFlag: v.User} {
			if val != "" {
				body[name] = val
			}
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "编码请求失败", err)
	}
	req, err := http.NewRequestWithContext(ctx, route.Method, c.conn.BaseURL()+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func stringValues(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case string:
		return []string{x}
	case time.Duration:
		return []string{x.String()}
	case int:
		return []string{strconv.Itoa(x)}
	case bool:
		return []string{strconv.FormatBool(x)}
	}
	return []string{strings.TrimSpace(strings.Trim(string(mustJSON(v)), `"`))}
}

func mustJSON(v any) []byte {
	raw, _ := json.Marshal(v)
	return raw
}

// unavailable 把连不上主控包成 unavailable，reason 按连法说明（socket 文件不在、没人监听、超时），next 按连法给提示。
func (c *Client) unavailable(err error) error {
	reason := "连不上主控"
	if c.conn.Server != "" {
		reason = "连不上主控 " + c.conn.Server
	}
	switch {
	case c.conn.Server == "" && errors.Is(err, fs.ErrNotExist):
		reason = "连不上主控：本机没有它的 socket 文件"
	case c.conn.Server == "" && errors.Is(err, syscall.ECONNREFUSED):
		reason = "连不上主控：socket 文件在，但没有进程在监听"
	case errors.Is(err, syscall.ECONNREFUSED):
		reason += "：连接被拒绝"
	case errors.Is(err, context.DeadlineExceeded):
		reason = "主控没有在限时内响应"
	}
	return v1.Wrap(v1.CodeUnavailable, reason, unreachable{err}).WithNext(c.conn.hint())
}

// ErrUnreachable 标出「连不上主控」这一种 unavailable（errors.Is）：请求没到主控（连接失败），或者主控前面的代理回了网关类状态码
// （gatewayStatus）而不是主控的四字段错误——与主控明确回应的 unavailable 区分开。跟长任务时遇到它接着查（主控可能在重启，如自升级）。
var ErrUnreachable = errors.New("连不上主控")

// gatewayStatus 报告状态码是不是代理说「后端不在或没回应」：502、503、504，以及 Cloudflare 回源失败的 520–524。
func gatewayStatus(code int) bool {
	switch code {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return code >= 520 && code <= 524
}

// unreachable 包着连不上的原始错误：文字就是原始错误（输出的「原因」不多一行），errors.Is 认得出 ErrUnreachable。
type unreachable struct{ err error }

func (u unreachable) Error() string        { return u.err.Error() }
func (u unreachable) Unwrap() error        { return u.err }
func (u unreachable) Is(target error) bool { return target == ErrUnreachable }

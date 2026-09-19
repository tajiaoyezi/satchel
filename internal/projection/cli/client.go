package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
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
// 本 change 只连本机 socket；--server / --token 的远程用法随 m1-04 加进来。
type Client struct {
	table *command.Table
	http  *http.Client
	base  string
	hint  string
}

// NewClient 建一个连本机 socket 的客户端。
func NewClient(table *command.Table, socket string) *Client {
	return &Client{
		table: table,
		base:  "http://satchel",
		hint:  "主控没在运行？在主控本机执行 systemctl status satchel 或 docker compose ps 查看；数据目录是 " + filepath.Dir(socket),
		http: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			}},
		},
	}
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
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, v1.Wrap(v1.CodeUnavailable, "读取主控响应失败", err).WithNext(c.hint)
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
		return nil, v1.Newf(v1.CodeInternal, "主控返回了 %d，但不是四字段错误", resp.StatusCode)
	}
	return nil, &e
}

// request 按 REST 映射构造请求：位置参数替换路径模板里的 {name}（GET 的 flag 进查询参数，POST 的 flag 与 confirm 进 JSON 体）。
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
	if route.Method == http.MethodGet {
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
		u := c.base + path
		if enc := q.Encode(); enc != "" {
			u += "?" + enc
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
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "编码请求失败", err)
	}
	req, err := http.NewRequestWithContext(ctx, route.Method, c.base+path, bytes.NewReader(raw))
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

// unavailable 把连不上 socket（文件不存在、连接被拒）包成 unavailable；别的传输错误也一样，reason 里说明。
func (c *Client) unavailable(err error) error {
	reason := "连不上主控"
	switch {
	case errors.Is(err, fs.ErrNotExist):
		reason = "连不上主控：本机没有它的 socket 文件"
	case errors.Is(err, syscall.ECONNREFUSED):
		reason = "连不上主控：socket 文件在，但没有进程在监听"
	case errors.Is(err, context.DeadlineExceeded):
		reason = "主控没有在限时内响应"
	}
	return v1.Wrap(v1.CodeUnavailable, reason, err).WithNext(c.hint)
}

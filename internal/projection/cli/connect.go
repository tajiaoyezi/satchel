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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 远程主控与令牌的环境变量（master-cli「经主控的命令连本机 socket 或远程主控」）。
const (
	EnvServer = "SATCHEL_SERVER"
	EnvToken  = "SATCHEL_TOKEN"
)

// Connection 是一次执行连主控的方式：Server 为空时连本机 Socket，否则经 HTTP(S) 连 Server（可带路径前缀）；
// Token 非空时每个请求带 Authorization: Bearer（经 socket 也带：本机配了令牌就按令牌的权限算）。
type Connection struct {
	Server string
	Token  string
	Socket string
}

// HTTPClient 是按这种连法拨号、并给每个请求带上令牌的 HTTP 客户端。不跟随重定向：
// 跟随会把 POST 变成 GET，令牌也可能被带到别的地址。
func (c Connection) HTTPClient() *http.Client {
	var transport http.RoundTripper = http.DefaultTransport
	if c.Server == "" {
		socket := c.Socket
		transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}
	}
	if c.Token != "" {
		transport = bearerTransport{token: c.Token, next: transport}
	}
	return &http.Client{
		Timeout:       60 * time.Second,
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// BaseURL 是请求路径（/api/v1/…、/mcp）前面的部分：本机 socket 是 http://satchel，远程是 server 地址。
func (c Connection) BaseURL() string {
	if c.Server == "" {
		return "http://satchel"
	}
	return c.Server
}

// hint 是连不上主控时的 next：本机 socket 提示查主控状态，远程提示检查地址与网络。
func (c Connection) hint() string {
	if c.Server == "" {
		return "主控没在运行？在主控本机执行 systemctl status satchel 或 docker compose ps 查看；数据目录是 " + filepath.Dir(c.Socket)
	}
	return "检查主控地址 " + c.Server + " 是否正确（--server、" + EnvServer + " 或登录文件）、主控是否在运行、网络与防火墙是否放行"
}

// bearerTransport 给每个请求带上令牌。
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.next.RoundTrip(r)
}

// connFlags 是根 flag --server / --token 的值与是否显式给出（给了空串也算给出）。
type connFlags struct {
	server, token       string
	serverSet, tokenSet bool
}

type connFlagsKey struct{}

func withConnFlags(ctx context.Context, f connFlags) context.Context {
	return context.WithValue(ctx, connFlagsKey{}, f)
}

func connFlagsOf(ctx context.Context) connFlags {
	f, _ := ctx.Value(connFlagsKey{}).(connFlags)
	return f
}

// connectingLocal 是会连主控的本地命令：只有它们接受显式的 --server / --token（master-cli「本地命令不接受 --server 与 --token」）。
var connectingLocal = map[string]bool{"login": true, "mcp stdio": true, "mcp init": true}

// Connect 解析本次执行连主控的方式；令牌经明文 HTTP 发往回环以外的地址时往 stderr 写一行提示。
// 一次执行只调它一次：经主控的命令在执行前调，会连主控的本地命令（mcp stdio、mcp init）在处理函数里调。
func Connect(ctx context.Context) (Connection, error) {
	conn, err := resolveConnection(connFlagsOf(ctx), DataDir(ctx))
	if err != nil {
		return Connection{}, err
	}
	warnPlaintext(stderrOf(ctx), conn)
	return conn, nil
}

// resolveConnection 按第 05 章的顺序取 server 与令牌：根 flag、环境变量、登录文件，各取第一个有的。
// 登录文件的令牌只发给登录文件里记的那个 server。形状不对时：flag 是 usage，环境变量与登录文件是 config。
func resolveConnection(f connFlags, dataDir string) (Connection, error) {
	conn := Connection{Socket: socketPath(dataDir)}
	var login *loginRecord
	loaded := false
	loadLogin := func() (*loginRecord, error) {
		if !loaded {
			var err error
			login, err = readLogin()
			if err != nil {
				return nil, err
			}
			loaded = true
		}
		return login, nil
	}
	switch env := os.Getenv(EnvServer); {
	case f.serverSet:
		s, err := normalizeServer(f.server)
		if err != nil {
			return Connection{}, usageError("--server 的值 %q 不对：%s", f.server, err)
		}
		conn.Server = s
	case env != "":
		s, err := normalizeServer(env)
		if err != nil {
			return Connection{}, v1.Newf(v1.CodeConfig, "环境变量 %s 的值 %q 不对：%s", EnvServer, env, err)
		}
		conn.Server = s
	default:
		l, err := loadLogin()
		if err != nil {
			return Connection{}, err
		}
		if l != nil {
			conn.Server = l.Server
		}
	}
	switch env := os.Getenv(EnvToken); {
	case f.tokenSet:
		if err := checkToken(f.token); err != nil {
			return Connection{}, usageError("--token 的值不对：%s", err)
		}
		conn.Token = f.token
	case env != "":
		if err := checkToken(env); err != nil {
			return Connection{}, v1.Newf(v1.CodeConfig, "环境变量 %s 的值不对：%s", EnvToken, err)
		}
		conn.Token = env
	default:
		l, err := loadLogin()
		if err != nil {
			return Connection{}, err
		}
		if l != nil && conn.Server == l.Server {
			conn.Token = l.Token
		}
	}
	return conn, nil
}

// normalizeServer 检查并归一主控地址：http 或 https、有 host、可带路径前缀，不带用户信息、查询与片段；去掉末尾斜杠。
func normalizeServer(raw string) (string, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", errors.New("不是合法的 URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return "", errors.New("要以 http:// 或 https:// 开头")
	case u.Host == "" || u.Hostname() == "":
		return "", errors.New("缺少主机名")
	case u.User != nil:
		return "", errors.New("不能带用户名或密码，令牌用 --token 或 " + EnvToken + " 给")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", errors.New("不能带查询参数或片段")
	case strings.HasSuffix(u.Host, ":"):
		return "", errors.New("端口是空的")
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/"), nil
}

// tokenRe 是 Bearer 令牌的字符集（RFC 6750 的 b64token）：Satchel 的令牌是 sat_ 加 base64url，都在里面。
var tokenRe = regexp.MustCompile(`^[A-Za-z0-9\-._~+/]+=*$`)

func checkToken(tok string) error {
	if !tokenRe.MatchString(tok) {
		return errors.New("令牌是空的，或含有空白与其它不该有的字符")
	}
	return nil
}

// warnPlaintext 在令牌经明文 HTTP 发往回环以外的地址时往 w 写一行提示，不阻塞。
func warnPlaintext(w io.Writer, c Connection) {
	if c.Token == "" || !strings.HasPrefix(c.Server, "http://") {
		return
	}
	u, err := url.Parse(c.Server)
	if err != nil {
		return
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return
	}
	fmt.Fprintf(w, "提示：令牌经明文 HTTP 发往 %s，同一网络上的人能截获它；请给主控配上 HTTPS\n", u.Host)
}

// loginRecord 是登录文件的内容。
type loginRecord struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

// LoginFile 是登录文件的路径：用户配置目录下的 satchel/login.json（Linux 上是 ~/.config/satchel/，
// macOS 上是 ~/Library/Application Support/satchel/）。
func LoginFile() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", v1.Wrap(v1.CodeConfig, "找不到用户配置目录，登录文件无处存放", err).WithNext("设置 HOME，或改用环境变量 " + EnvServer + " 与 " + EnvToken)
	}
	return filepath.Join(dir, "satchel", "login.json"), nil
}

// readLogin 读登录文件：不存在（或连用户配置目录都没有）返回 nil。unix 上文件对组或其他用户开放、内容不合形状都是 config。
func readLogin() (*loginRecord, error) {
	path, err := LoginFile()
	if err != nil {
		return nil, nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeConfig, "读取登录文件 "+path+" 失败", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, v1.Newf(v1.CodeConfig, "登录文件 %s 的权限是 %#o，对组或其他用户开放了，里面的令牌可能已被别人读到", path, info.Mode().Perm()).
			WithNext("执行 chmod 600 " + path + "；若怀疑令牌已泄露，用 satchel token revoke 吊销后重新 satchel login")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, v1.Wrap(v1.CodeConfig, "读取登录文件 "+path+" 失败", err)
	}
	var rec loginRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, v1.Wrap(v1.CodeConfig, "登录文件 "+path+" 不是合法的 JSON", err).WithNext("删掉它（satchel logout）后重新 satchel login")
	}
	server, err := normalizeServer(rec.Server)
	if err != nil {
		return nil, v1.Newf(v1.CodeConfig, "登录文件 %s 里的 server 不对：%s", path, err).WithNext("删掉它（satchel logout）后重新 satchel login")
	}
	if err := checkToken(rec.Token); err != nil {
		return nil, v1.Newf(v1.CodeConfig, "登录文件 %s 里的令牌不对：%s", path, err).WithNext("删掉它（satchel logout）后重新 satchel login")
	}
	rec.Server = server
	return &rec, nil
}

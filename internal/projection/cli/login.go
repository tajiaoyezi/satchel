package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

type tableKey struct{}

// withTable 把本次执行的命令表放进 ctx：会连主控的本地命令（login、mcp *）按它构造请求。
func withTable(ctx context.Context, t *command.Table) context.Context {
	return context.WithValue(ctx, tableKey{}, t)
}

func tableOf(ctx context.Context) *command.Table {
	if t, ok := ctx.Value(tableKey{}).(*command.Table); ok && t != nil {
		return t
	}
	return command.Catalog()
}

// loginOutput 是 login 的输出：连上的主控与令牌的身份、登录文件的路径，没有令牌明文。
type loginOutput struct {
	Server    string       `json:"server"`
	Actor     string       `json:"actor"`
	ActorKind v1.ActorKind `json:"actor_kind"`
	Role      v1.Role      `json:"role"`
	TokenID   *int64       `json:"token_id"`
	Scopes    []v1.Scope   `json:"scopes"`
	Danger    []v1.Danger  `json:"danger"`
	LoginFile string       `json:"login_file"`
}

// loginCommand 是 satchel login（master-cli「login 与 logout」）：server 取 --server 或 SATCHEL_SERVER，令牌取 --token
// 或从终端读；先用它对这个 server 调一次 whoami，身份是令牌才写登录文件。
func loginCommand(ctx context.Context, _ *command.Invocation) (any, error) {
	f := connFlagsOf(ctx)
	var server string
	switch env := os.Getenv(EnvServer); {
	case f.serverSet:
		s, err := normalizeServer(f.server)
		if err != nil {
			return nil, usageError("--server 的值 %q 不对：%s", f.server, err)
		}
		server = s
	case env != "":
		s, err := normalizeServer(env)
		if err != nil {
			return nil, v1.Newf(v1.CodeConfig, "环境变量 %s 的值 %q 不对：%s", EnvServer, env, err)
		}
		server = s
	default:
		return nil, usageError("login 要知道连哪个主控：用 --server 给出地址，或设环境变量 %s", EnvServer)
	}
	token := f.token
	if f.tokenSet {
		if err := checkToken(token); err != nil {
			return nil, usageError("--token 的值不对：%s", err)
		}
	} else {
		raw, err := promptFrom(ctx)("API 令牌（输入不回显）")
		if errors.Is(err, ErrNoTerminal) {
			return nil, v1.Wrap(v1.CodeBadRequest, "login 要从终端读令牌，当前没有终端", err).
				WithNext("脚本里不用 login：设环境变量 " + EnvServer + " 与 " + EnvToken + " 即可")
		}
		if err != nil {
			return nil, v1.Wrap(v1.CodeInternal, "从终端读取失败", err)
		}
		token = strings.TrimSpace(raw)
		if err := checkToken(token); err != nil {
			return nil, v1.Newf(v1.CodeBadRequest, "输入的令牌不对：%s", err)
		}
	}
	conn := Connection{Server: server, Token: token}
	plaintextWarning(ctx, server, "令牌会")
	res, err := NewClient(tableOf(ctx), conn).Run(ctx, &command.Invocation{Path: []string{"whoami"}})
	if err != nil {
		return nil, err
	}
	var id v1.Identity
	raw, _ := json.Marshal(res)
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "主控返回的身份对象解不开", err)
	}
	if id.ActorKind != v1.ActorToken {
		return nil, v1.Newf(v1.CodeUnauthenticated, "主控 %s 没有把这把令牌认成令牌身份（得到 %s），没有写登录文件", server, id.ActorKind)
	}
	path, err := writeLogin(loginRecord{Server: server, Token: token})
	if err != nil {
		return nil, err
	}
	return loginOutput{Server: server, Actor: id.Actor, ActorKind: id.ActorKind, Role: id.Role, TokenID: id.TokenID,
		Scopes: id.Scopes, Danger: id.Danger, LoginFile: path}, nil
}

func renderLogin(w io.Writer, result any) error {
	out := result.(loginOutput)
	id := int64(0)
	if out.TokenID != nil {
		id = *out.TokenID
	}
	danger := "无"
	if len(out.Danger) > 0 {
		danger = joinAny(out.Danger)
	}
	_, err := fmt.Fprintf(w, "已登录 %s：%s 的令牌 #%d（%s，危险类：%s）\n登录文件：%s\n", out.Server, out.Actor, id, joinAny(out.Scopes), danger, out.LoginFile)
	return err
}

func joinAny[T ~string](list []T) string {
	parts := make([]string, len(list))
	for i, v := range list {
		parts[i] = string(v)
	}
	return strings.Join(parts, "、")
}

// writeLogin 写登录文件：目录 0700、文件 0600，先写临时文件再改名。
func writeLogin(rec loginRecord) (string, error) {
	path, err := LoginFile()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", v1.Wrap(v1.CodeConfig, "建登录文件的目录 "+dir+" 失败", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", v1.Wrap(v1.CodeConfig, "设置目录 "+dir+" 的权限失败", err)
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", v1.Wrap(v1.CodeInternal, "编码登录文件失败", err)
	}
	if err := writeFileAtomic(path, append(raw, '\n'), 0o600); err != nil {
		return "", v1.Wrap(v1.CodeConfig, "写登录文件 "+path+" 失败", err)
	}
	return path, nil
}

// writeFileAtomic 先在同一目录写临时文件（权限 perm）再改名，写到一半失败不会留下半个文件。
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// logoutOutput 是 logout 的输出：登录文件原来在不在；令牌不因登出失效。
type logoutOutput struct {
	LoginFile string `json:"login_file"`
	Removed   bool   `json:"removed"`
	Note      string `json:"note"`
}

const logoutNote = "令牌在主控上仍然有效；要让它失效，用 satchel token revoke <id> 吊销（id 见 satchel token list）"

// logoutCommand 是 satchel logout：只删登录文件，不吊销令牌（吊销要当场验证，是另一件事）。
func logoutCommand(context.Context, *command.Invocation) (any, error) {
	path, err := LoginFile()
	if err != nil {
		return nil, err
	}
	err = os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return logoutOutput{LoginFile: path, Removed: false, Note: logoutNote}, nil
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeConfig, "删除登录文件 "+path+" 失败", err)
	}
	return logoutOutput{LoginFile: path, Removed: true, Note: logoutNote}, nil
}

func renderLogout(w io.Writer, result any) error {
	out := result.(logoutOutput)
	if !out.Removed {
		_, err := fmt.Fprintf(w, "没有登录文件 %s，什么都没做\n", out.LoginFile)
		return err
	}
	_, err := fmt.Fprintf(w, "已删除登录文件 %s\n%s\n", out.LoginFile, out.Note)
	return err
}

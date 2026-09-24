// Package authz 是横切层的权限：按命令表检查 scope、危险类、confirm 与人类专属（master-identity-and-authz「检查顺序与错误码」）。
// 检查只在服务端做，CLI 与 MCP 不自行判断权限。
package authz

import (
	"context"
	"regexp"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// HumanVerifier 是第 05 章七组的当场验证：人类专属命令对本机管理员与用户身份要在同一请求里验密码与第二因素。
// 生产装配传 service/auth 的 Verifier；传 nil 时人类专属命令一律 human_required（没有验证入口）。
type HumanVerifier interface {
	Verify(ctx context.Context, inv *command.Invocation) error
}

var dangerLabels = map[v1.Danger]string{
	v1.DangerDelete: "删除类", v1.DangerRestart: "重启类", v1.DangerPermission: "权限类",
	v1.DangerBatch: "批量类", v1.DangerExec: "节点执行类", v1.DangerMaster: "主控自身类",
}

// Wrap 给执行链套上权限检查，顺序固定：身份 → 人类专属 → scope → 危险类 → confirm → 执行。
// 请求带了无效凭据（authn 标的 SourceInvalid）最先拒，连不要身份的命令也拒。
func Wrap(t *command.Table, verifier HumanVerifier, next command.Runner) command.Runner {
	return command.RunnerFunc(func(ctx context.Context, inv *command.Invocation) (any, error) {
		if v1.CredentialSourceFrom(ctx) == v1.SourceInvalid {
			return nil, InvalidCredential()
		}
		cmd, ok := t.Lookup(inv.Name())
		if !ok {
			return nil, v1.Newf(v1.CodeNotFound, "没有命令 %s", inv.Name())
		}
		if cmd.Class == command.ClassLocal {
			return nil, v1.Newf(v1.CodeBadRequest, "%s 是本地命令，只在 CLI 里有、不经主控", cmd.Name())
		}
		// 不要身份的命令（初始化向导）对谁都开放：跳过身份与 scope 检查；表校验保证它没有人类专属与危险类。
		if cmd.Anonymous {
			return next.Run(ctx, inv)
		}
		id := v1.IdentityFrom(ctx)
		if id.IsAnonymous() {
			return nil, v1.New(v1.CodeUnauthenticated, "没有身份：请登录、经主控本机的 unix socket 调用，或带上令牌").
				WithNext("网页上登录；在主控本机以 root 或运行主控的用户执行 satchel；远程用 satchel login 或 --server 加 --token")
		}
		if cmd.HumanOnly {
			if id.ActorKind == v1.ActorToken || id.ActorKind == v1.ActorSystem {
				return nil, v1.Newf(v1.CodeHumanRequired, "%s 是只有人能做的操作，令牌一律不能做", cmd.Name()).
					WithNext("在网页或 CLI 上由管理员本人执行")
			}
			if verifier == nil {
				return nil, v1.Newf(v1.CodeHumanRequired, "%s 是只有人能做的操作，需要当场验证身份，本版本还没有验证入口", cmd.Name())
			}
			if err := verifier.Verify(ctx, inv); err != nil {
				return nil, err
			}
		}
		scope, _ := cmd.Scope()
		if !id.HasScope(scope) {
			return nil, v1.Newf(v1.CodeForbidden, "%s 需要 %s 权限，当前身份没有", cmd.Name(), scope).
				WithState("required_scope", string(scope)).WithState("scopes", id.Scopes)
		}
		if cmd.Danger != "" {
			if !id.HasDanger(cmd.Danger) {
				return nil, v1.Newf(v1.CodeForbidden, "%s 属于危险操作的%s，当前身份没有开这一类", cmd.Name(), dangerLabels[cmd.Danger]).
					WithState("required_danger", string(cmd.Danger)).WithState("danger", id.Danger).
					WithNext("在「API 令牌」页给这把令牌打开对应的危险类")
			}
			if err := checkConfirm(cmd, inv); err != nil {
				return nil, err
			}
		}
		return next.Run(ctx, inv)
	})
}

// InvalidCredential 是请求带了无效凭据时的错误（authz 第 ① 步，MCP 的 satchel_explain 同用）。
// reason 不区分令牌是不存在、已吊销、已过期还是签发者已停用，免得给探测令牌的人当回显。
func InvalidCredential() *v1.Error {
	return v1.New(v1.CodeUnauthenticated, "请求带的凭据无效：Authorization 头要是 Bearer 加一把有效的令牌，这把令牌不存在、已吊销、已过期，或签发者已停用").
		WithNext("换一把有效的令牌；令牌由管理员在主控本机（satchel token create）或网页上签发")
}

// decimalRe 是 count 口径接受的形状：规范的非负十进制（没有前导零、正负号、空白）。
var decimalRe = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// checkConfirm 按口径核对 confirm 字符串：object 与指定位置参数的值逐字相等（不去空白）；count 必须是规范的十进制数，
// 与受影响数量相等的比对随 M2 的 plan 交付（那时才有数量可比）。
func checkConfirm(cmd *command.Command, inv *command.Invocation) error {
	switch cmd.Confirm.Kind {
	case command.ConfirmObject:
		expected := ""
		for i, a := range cmd.Args {
			if a.Name == cmd.Confirm.Arg {
				expected = inv.Arg(i)
			}
		}
		if inv.Confirm == "" || inv.Confirm != expected {
			return v1.Newf(v1.CodeConfirmRequired, "%s 是危险操作，需要用 confirm 填对象名确认", cmd.Name()).
				WithState("kind", string(command.ConfirmObject)).WithState("expected", expected).
				WithNext("重新执行并加上 --confirm " + expected)
		}
	case command.ConfirmCount:
		if !decimalRe.MatchString(inv.Confirm) {
			return v1.Newf(v1.CodeConfirmRequired, "%s 是危险操作，需要用 confirm 填本次受影响的数量（十进制整数）确认", cmd.Name()).
				WithState("kind", string(command.ConfirmCount)).WithState("given", inv.Confirm).
				WithNext("先 plan 看受影响数量，再用 --confirm <数量> 执行")
		}
	}
	return nil
}

package command

import "strings"

// WebEntries 是编进二进制的网页提供的入口（master-web「错误提示里的网页入口」）：Login 是有没有登录页，
// Pages 是命令路径到提供它的页面名（一条命令出现在几页时取页面清单里的第一页）。
type WebEntries struct {
	Login bool
	Pages map[string]string
}

// webEntries 由 cmd/satchel 的 main 在执行任何命令之前用编进二进制的页面清单设置一次，之后只读；
// 没设置时等于这个二进制没有网页，错误提示里一律不提网页。
var webEntries WebEntries

// SetWebEntries 设置网页提供的入口（main 与测试用）。
func SetWebEntries(e WebEntries) { webEntries = e }

// WebLogin 报告网页有没有登录页。
func WebLogin() bool { return webEntries.Login }

// WebPage 返回提供这条命令的网页页面名；没有页面提供它、或二进制里没有网页时 ok 为假。
func WebPage(name string) (page string, ok bool) {
	page, ok = webEntries.Pages[name]
	return page, ok
}

// WebHint 是错误提示里指向网页的半句「在网页的「<页面名>」页<动作>」；没有页面提供这条命令时是空串。
func WebHint(name, action string) string {
	page, ok := WebPage(name)
	if !ok {
		return ""
	}
	return "在网页的「" + page + "」页" + action
}

// HumanOnlyNext 是人类专属命令被令牌或 MCP 拒绝时的下一步：有网页入口时先给网页的那一页，再给主控本机的做法。
// account 下的命令只能由账号本人登录后做（本机管理员不是账号），没有主控本机的做法。
func HumanOnlyNext(name string) string {
	if strings.HasPrefix(name, "account ") {
		if hint := WebHint(name, "操作"); hint != "" {
			return "由账号本人登录网页，" + hint
		}
		return "由账号本人登录后用会话操作（本机管理员不是账号，主控本机的 CLI 做不了）"
	}
	local := "在主控本机不带令牌执行 satchel " + name + " --verify-user <管理员账号>"
	if hint := WebHint(name, "用管理员账号操作"); hint != "" {
		return hint + "；或" + local
	}
	return local
}

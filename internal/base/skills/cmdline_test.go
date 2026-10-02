package skills

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/satchel/satchel/internal/command"
)

// check 把一段正文放进一份 skill，用真实命令表跑格式与命令行检查。
func check(t *testing.T, body string) ([]Ref, []Problem) {
	t.Helper()
	fsys := fstest.MapFS{"satchel-x/SKILL.md": {Data: []byte(skillMD("satchel-x", `description: "测试"`, body))}}
	skills, problems, err := Load(fsys)
	if err != nil || len(problems) != 0 {
		t.Fatalf("格式检查不该失败：%v %v", err, problems)
	}
	return CheckCommands(command.Catalog(), skills)
}

// mustFail 断言这段正文的检查失败，每条失败点名 satchel-x，并且失败信息里有 want。
func mustFail(t *testing.T, body, want string) {
	t.Helper()
	_, problems := check(t, body)
	if len(problems) == 0 {
		t.Fatalf("应当失败：%q", body)
	}
	var all []string
	for _, p := range problems {
		if p.Skill != "satchel-x" || p.Line == 0 {
			t.Errorf("失败信息应当点名 skill 与行号：%+v", p)
		}
		all = append(all, p.String())
	}
	if !strings.Contains(strings.Join(all, "\n"), want) {
		t.Fatalf("失败信息应当含「%s」：%q\n%s", want, body, strings.Join(all, "\n"))
	}
}

// mustPass 断言这段正文通过检查，返回解析出的命令。
func mustPass(t *testing.T, body string) []string {
	t.Helper()
	refs, problems := check(t, body)
	if len(problems) != 0 {
		t.Fatalf("应当通过：%q\n%v", body, problems)
	}
	var cmds []string
	for _, r := range refs {
		cmds = append(cmds, r.Command)
	}
	return cmds
}

// master-skills「写了不存在的命令」：行内代码、sudo 前缀、编号步骤下缩进的代码块，都点名行号与原文。
func TestUnknownCommands(t *testing.T) {
	mustFail(t, "用 `satchel backup delete old.zip` 删。\n", "backup 下没有 delete")
	mustFail(t, "`sudo satchel db stat`\n", "db 下没有 stat")
	_, problems := check(t, "1. 看迁移状态：\n\n   ```\n   sudo satchel db stauts\n   ```\n")
	if len(problems) != 1 || problems[0].Line != 9 || !strings.Contains(problems[0].Text, "satchel db stauts") {
		t.Fatalf("缩进的代码块里的命令行要被检查并报出行号：%v", problems)
	}
}

// master-skills「CLI 上没有的 flag」：七处各自点名那个 flag；--json 不算错。
func TestFlagsMustExistOnCLI(t *testing.T) {
	for body, want := range map[string]string{
		"```\n$ satchel backup create --keep 3 --json\n```\n":            "--keep 不是 backup create",
		"`satchel database test --host db --password x`\n":               "--password 在 CLI 上不存在：密码只从终端读",
		"`satchel token revoke <id> --verify-password x`\n":              "--verify-password 在 CLI 上不存在：密码只从终端读",
		"`satchel db status --server https://x`\n":                       "--server 只对经主控的命令",
		"`satchel token update <id> --presett ops`\n":                    "--presett",
		"`satchel update apply <版本号> --confirm=<版本号> --chanel stable`\n": "--chanel",
		"`satchel update apply <版本号> --confirm`\n":                       "--confirm 缺少值",
	} {
		mustFail(t, body, want)
	}
	if _, problems := check(t, "```\n$ satchel backup create --keep 3 --json\n```\n"); len(problems) != 1 || strings.Contains(problems[0].Reason, "--json") {
		t.Fatalf("只有 --keep 算错，--json 不算：%v", problems)
	}
}

// master-skills「位置参数多了」：多了失败；引号里是一个参数；少写必填参数不算错。
func TestPositionalCount(t *testing.T) {
	mustFail(t, "`satchel explain backup create`\n", "explain 最多 1 个位置参数")
	mustFail(t, "`satchel schedule list runs`\n", "schedule list 最多 0 个位置参数")
	got := mustPass(t, "`satchel explain \"backup create\"` 与 `satchel backup restore`\n")
	if strings.Join(got, ",") != "explain,backup restore" {
		t.Fatalf("应当解析出 explain 与 backup restore：%v", got)
	}
}

// master-skills「哪些算 satchel 命令行」：四种开头与两行的代码块各解析出一条命令；服务名、MCP 服务器名里的 satchel 不算。
func TestWhichLinesAreCommandLines(t *testing.T) {
	body := "`sudo satchel db status`、`sudo -u satchel satchel db unlock`、`docker compose exec satchel satchel db status`、" +
		"`SATCHEL_OUTPUT=json satchel overview`\n\n```\nsatchel backup create --no-wait\nsatchel job get <job_id>\n```\n\n" +
		"`journalctl -u satchel -n 200`、`rc-service satchel status`、`codex mcp get satchel --json`\n"
	got := mustPass(t, body)
	if strings.Join(got, ",") != "db status,db unlock,db status,overview,backup create,job get" {
		t.Fatalf("解析出的命令不对：%v", got)
	}
}

// master-skills「写法超出范围」：都失败，提示改成简单写法（占位符里的 | 与引号也算）。
func TestOutOfRangeWriting(t *testing.T) {
	for _, body := range []string{
		"`satchel job list --json | jq '.items[0]'`\n",
		"`satchel security ban <ip> [--permanent]`\n",
		"`sudo satchel … --verify-user <管理员账号>`\n",
		"`satchel update check  # 看最新版本`\n",
		"`satchel job get $(satchel job list)`\n",
		"`satchel backup restore satchel-backup-*.zip`\n",
		"`satchel token update <id> --name \"ops bot`\n",
		"```\nsatchel settings set \\\n```\n",
		"`satchel token update <id> --preset <readonly|ops|full>`\n",
		"`satchel backup restore <backup\">`\n",
		"`satchel update apply <version'> --confirm <version>`\n",
	} {
		mustFail(t, body, "改成简单写法")
	}
	mustFail(t, "`satchel backup　list`\n", "只认半角空格与制表符")
	mustFail(t, "~~~\nsatchel overview\n~~~\n", "不用 ~~~")
	mustFail(t, "运行 `satchel overview 看总览。\n", "反引号配不成对")
}

// master-skills「帮助与 explain 的参数」：前六处通过；拼错的 explain 目标、拼错的子命令与占位符带 --help 失败。
func TestHelpAndExplain(t *testing.T) {
	got := mustPass(t, "`satchel --help`、`satchel backup --help`、`satchel token update <id> --help`、`satchel explain`、"+
		"`satchel explain \"settings set\"`、`satchel explain Task`\n")
	if strings.Join(got, ",") != "token update,explain,explain,explain" {
		t.Fatalf("帮助行只有落在命令上的才算引用：%v", got)
	}
	mustFail(t, "`satchel explain \"backup crate\"`\n", "既不是命令也不是 kind")
	mustFail(t, "`satchel bakup --help`\n", "没有子命令 bakup")
	mustFail(t, "`satchel token updte --help`\n", "token 下没有 updte")
	mustFail(t, "`satchel <命令> --help`\n", "带 --help 时命令路径的位置上要是命令或分组")
}

// 命令路径的每一段都是不带空白的非空词：引号括起的几个词、空串都不算路径段，真实的 CLI 同样认不出。
func TestPathWordsMustBePlain(t *testing.T) {
	mustFail(t, "`satchel \"backup list\"`\n", "satchel 后面要紧跟命令路径")
	mustFail(t, "`satchel 'token revoke' 3`\n", "satchel 后面要紧跟命令路径")
	mustFail(t, "`satchel backup \"\" list`\n", "backup 是分组")
	mustFail(t, "`satchel overview \"\" \"\"`\n", "overview 最多 0 个位置参数")
	if got := mustPass(t, "`satchel \"overview\"`\n"); strings.Join(got, ",") != "overview" {
		t.Fatalf("引号括起的一个词仍是路径段：%v", got)
	}
}

// 只写到分组、根 flag 写在命令路径前面，都点出原因。
func TestNoCommandMessages(t *testing.T) {
	mustFail(t, "`satchel backup`\n", "backup 是分组，要写到具体的子命令")
	mustFail(t, "`satchel --json overview`\n", "satchel 后面要紧跟命令路径")
}

// 编进二进制的真实 skills：格式检查与命令行检查都通过（覆盖检查见 delivered_test.go）。
// 写 skills 时每写完一份就靠它检查那一份。
func TestEmbeddedSkillsPassChecks(t *testing.T) {
	skills, problems, err := Load(Files())
	if err != nil {
		t.Fatal(err)
	}
	_, cmdProblems := CheckCommands(command.Catalog(), skills)
	for _, p := range append(problems, cmdProblems...) {
		t.Error(p)
	}
}

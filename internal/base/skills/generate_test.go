package skills

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/satchel/satchel/internal/command"
)

// testTable 是一张测试用的小命令表：两条普通命令与一条隐藏命令，再加上 extra。
func testTable(extra ...*command.Command) *command.Table {
	return command.MustNew(append([]*command.Command{
		{Path: []string{"overview"}, Summary: "测试", Class: command.ClassRead},
		{Path: []string{"backup", "list"}, Summary: "测试", Class: command.ClassRead},
		{Path: []string{"debug", "dump"}, Summary: "测试", Class: command.ClassRead, Hidden: true},
	}, extra...)...)
}

// testSkills 是一份测试用的 skills：satchel-a 讲两条命令，satchel-b 讲一条；description 里带一个 |。
func testSkills() fstest.MapFS {
	return fstest.MapFS{
		"satchel-a/SKILL.md": {Data: []byte(skillMD("satchel-a", `description: "看总览 | 列备份"`,
			"先跑 `satchel overview`，再看备份：\n\n```\nsatchel backup list --json\n```\n"))},
		"satchel-b/SKILL.md": {Data: []byte(skillMD("satchel-b", `description: "只看总览"`, "`satchel overview`\n"))},
	}
}

// master-skills「新命令没写进 skill」：测试用命令表多一条 demo ping、skills 不变，覆盖检查点名 demo ping，生成也失败。
// 隐藏命令不要求有 skill 讲到。
func TestCoverageNamesNewCommand(t *testing.T) {
	skills, problems, err := Load(testSkills())
	if err != nil || len(problems) != 0 {
		t.Fatalf("测试用 skills 应当合格：%v %v", err, problems)
	}
	refs, problems := CheckCommands(testTable(), skills)
	if len(problems) != 0 {
		t.Fatalf("测试用 skills 的命令行应当通过：%v", problems)
	}
	if missing := Coverage(testTable(), refs); len(missing) != 0 {
		t.Fatalf("没加新命令时不该有缺的（隐藏命令不算）：%v", missing)
	}
	extra := testTable(&command.Command{Path: []string{"demo", "ping"}, Summary: "测试", Class: command.ClassRead})
	if missing := Coverage(extra, refs); strings.Join(missing, ",") != "demo ping" {
		t.Fatalf("覆盖检查应当只点名 demo ping：%v", missing)
	}
	if _, err := Generate(extra, testSkills()); err == nil || !strings.Contains(err.Error(), "命令 demo ping 没有被任何 skill 讲到") {
		t.Fatalf("有命令没被讲到时生成应当失败并点名它：%v", err)
	}
}

// 生成函数对一份测试数据的输出：skills 一节按名字排序、| 转义；命令一节按命令路径排序、不列隐藏命令，引用它的 skill 排序后用顿号连接。
func TestGenerateOutput(t *testing.T) {
	got, err := Generate(testTable(), testSkills())
	if err != nil {
		t.Fatal(err)
	}
	want := "<!-- 由 go generate ./internal/command/ 从命令表与 internal/base/skills/files/ 生成，不要手改。 -->\n" +
		"# 命令 × skills 对照表\n\n" +
		"skills 是写给 AI 的操作手册：源文件在 `internal/base/skills/files/`，编进 satchel 二进制，`satchel mcp init` 把它们装进 runtime 的 skills 目录。" +
		"命令表里每一条非隐藏命令都要被至少一个 skill 讲到；skills 里写的每一条 satchel 命令行都要在这个二进制的 CLI 上解析得了。" +
		"这两条由 `go test ./internal/base/skills/` 与 CI 守着（master-skills）。\n\n" +
		"## skills\n\n| skill | 什么时候用 |\n|---|---|\n" +
		"| `satchel-a` | 看总览 \\| 列备份 |\n" +
		"| `satchel-b` | 只看总览 |\n" +
		"\n## 命令\n\n| 命令 | 讲到它的 skill |\n|---|---|\n" +
		"| `backup list` | `satchel-a` |\n" +
		"| `overview` | `satchel-a`、`satchel-b` |\n"
	if string(got) != want {
		t.Fatalf("对照表不对：\n%s\n---- want ----\n%s", got, want)
	}
}

// skills 有格式或命令行问题时生成失败，错误里列出每一处问题，不生成一份不完整的表。
func TestGenerateFailsOnProblems(t *testing.T) {
	fsys := testSkills()
	fsys["satchel-c/SKILL.md"] = &fstest.MapFile{Data: []byte(skillMD("satchel-c", `description: 不加引号`, "正文\n"))}
	fsys["satchel-b/SKILL.md"] = &fstest.MapFile{Data: []byte(skillMD("satchel-b", `description: "只看总览"`, "`satchel overview`、`satchel backup lst`\n"))}
	doc, err := Generate(testTable(), fsys)
	if err == nil || doc != nil {
		t.Fatalf("有问题时应当失败且不生成：%v", err)
	}
	for _, want := range []string{"skills 检查不通过", "satchel-c", "双引号", "satchel-b 第 6 行", "backup 下没有 lst"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误里应当有「%s」：%v", want, err)
		}
	}
}

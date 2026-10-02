package skills

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/satchel/satchel/internal/command"
)

// 这里的测试用编进二进制的真实 skills 与真实命令表（master-skills「M1 交付的 skills」与对照表）。

// master-skills「七份 skills 都在」：恰好七份，每份都通过格式检查。
func TestSevenSkillsDelivered(t *testing.T) {
	skills, problems, err := Load(Files())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
	var names []string
	for _, s := range skills {
		names = append(names, s.Name)
	}
	want := "satchel-access,satchel-backup,satchel-basics,satchel-database,satchel-settings,satchel-troubleshoot,satchel-upgrade"
	if strings.Join(names, ",") != want {
		t.Fatalf("应当恰好是七份 skills：%v", names)
	}
}

// master-skills「basics 写明几条通用规则」。
func TestBasicsGeneralRules(t *testing.T) {
	raw, err := fs.ReadFile(Files(), "satchel-basics/"+FileName)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"satchel overview", "satchel explain", "satchel_explain", "--confirm", "human_required",
		"--verify-user", "forbidden", "satchel job get", "satchel token list", "satchel_run"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("satchel-basics 里应当有 %s", want)
		}
	}
}

// master-skills「当前的命令都有 skill」：每条非隐藏命令都至少被一个 skill 引用，包括 overview、人类专属的 token create
// 与本地命令 serve；同一份 skills 配上多一条 demo ping 的命令表时，覆盖检查只点名 demo ping（「新命令没写进 skill」）。
func TestCurrentCommandsCovered(t *testing.T) {
	skills, _, err := Load(Files())
	if err != nil {
		t.Fatal(err)
	}
	refs, problems := CheckCommands(command.Catalog(), skills)
	for _, p := range problems {
		t.Error(p)
	}
	if missing := Coverage(command.Catalog(), refs); len(missing) != 0 {
		t.Fatalf("这些命令没有被任何 skill 讲到：%v", missing)
	}
	used := map[string]bool{}
	for _, r := range refs {
		used[r.Command] = true
	}
	visible := 0
	for _, c := range command.Catalog().All() {
		if !c.Hidden {
			visible++
		}
	}
	if visible != 55 || !used["overview"] || !used["token create"] || !used["serve"] {
		t.Fatalf("应当覆盖 55 条非隐藏命令，含 overview、token create、serve：%d %v", visible, used)
	}
	extra := command.MustNew(append(command.Catalog().All(), &command.Command{Path: []string{"demo", "ping"}, Summary: "测试", Class: command.ClassRead})...)
	if missing := Coverage(extra, refs); strings.Join(missing, ",") != "demo ping" {
		t.Fatalf("覆盖检查应当只点名 demo ping：%v", missing)
	}
}

// master-skills「生成物与命令表和 skills 一致」与「改了 skill 没重新生成」：仓库里的 docs/skills.md 等于当前的生成结果；
// 在 satchel-basics 里删掉对 backup list 的引用（satchel-backup 仍引用它）后生成结果就不同。
func TestSkillsDocUpToDate(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(DocsPath)))
	if err != nil {
		t.Fatalf("读不到 %s：%v（运行 go generate ./internal/command/）", DocsPath, err)
	}
	want, err := Generate(command.Catalog(), Files())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, want) {
		t.Fatalf("%s 与命令表和 skills 不一致，请运行 go generate ./internal/command/ 并提交", DocsPath)
	}

	edited := fstest.MapFS{}
	if err := fs.WalkDir(Files(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(Files(), path)
		edited[path] = &fstest.MapFile{Data: data}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	basics := "satchel-basics/" + FileName
	if !bytes.Contains(edited[basics].Data, []byte("`satchel backup list`")) {
		t.Fatal("satchel-basics 应当引用 backup list")
	}
	edited[basics].Data = bytes.ReplaceAll(edited[basics].Data, []byte("`satchel backup list`"), []byte("备份列表"))
	changed, err := Generate(command.Catalog(), edited)
	if err != nil {
		t.Fatalf("satchel-backup 仍引用 backup list，生成应当成功：%v", err)
	}
	if bytes.Equal(changed, onDisk) {
		t.Fatal("改了 skill 之后生成结果应当不同")
	}
}

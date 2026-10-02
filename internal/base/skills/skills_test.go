package skills

import (
	"bytes"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

// skillMD 拼一份 SKILL.md：frontmatter 的两行原样给出，正文跟在后面。
func skillMD(name, descLine, body string) string {
	return "---\nname: " + name + "\n" + descLine + "\n---\n\n" + body
}

// problemsOf 用一份只有一个 skill 的 FS 跑加载，返回问题的原因。
func problemsOf(t *testing.T, dir string, files map[string]string) []string {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, content := range files {
		fsys[dir+"/"+name] = &fstest.MapFile{Data: []byte(content)}
	}
	_, problems, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, p := range problems {
		if p.Skill != dir {
			t.Errorf("问题应当点名 %s：%v", dir, p)
		}
		reasons = append(reasons, p.Reason)
	}
	return reasons
}

// master-skills「格式不合被测试拦下」：十一种情况各自失败并点名 satchel-x 与原因；description 恰好 1024 个汉字时通过。
func TestFormatProblems(t *testing.T) {
	ok := skillMD("satchel-x", `description: "用来测试"`, "正文\n")
	if got := problemsOf(t, "satchel-x", map[string]string{"SKILL.md": ok}); len(got) != 0 {
		t.Fatalf("合格的 skill 不该有问题：%v", got)
	}
	exact := skillMD("satchel-x", `description: "`+strings.Repeat("汉", 1024)+`"`, "正文\n")
	if got := problemsOf(t, "satchel-x", map[string]string{"SKILL.md": exact}); len(got) != 0 {
		t.Fatalf("description 恰好 1024 个汉字应当通过：%v", got)
	}
	long := strings.Repeat("一行\n", 496) // 加上 frontmatter 与空行的五行，共 501 行
	name33 := "satchel-" + strings.Repeat("a", 25)
	cases := map[string]struct {
		dir   string
		files map[string]string
		want  string
	}{
		"name 不等于目录名":        {"satchel-x", map[string]string{"SKILL.md": skillMD("satchel-y", `description: "x"`, "正文\n")}, "name 要等于目录名"},
		"没有 description":     {"satchel-x", map[string]string{"SKILL.md": "---\nname: satchel-x\n---\n\n正文\n"}, "缺 description"},
		"description 1025 字": {"satchel-x", map[string]string{"SKILL.md": skillMD("satchel-x", `description: "`+strings.Repeat("汉", 1025)+`"`, "正文\n")}, "超过 1024"},
		"description 里有尖括号":  {"satchel-x", map[string]string{"SKILL.md": skillMD("satchel-x", `description: "升到 <版本号>"`, "正文\n")}, "不能有 < 或 >"},
		"description 没加双引号":  {"satchel-x", map[string]string{"SKILL.md": skillMD("satchel-x", `description: 用来测试`, "正文\n")}, "双引号"},
		"description 里有 ---": {"satchel-x", map[string]string{"SKILL.md": skillMD("satchel-x", `description: "前 --- 后"`, "正文\n")}, "不能有 ---"},
		"多了 version 字段":      {"satchel-x", map[string]string{"SKILL.md": "---\nname: satchel-x\ndescription: \"x\"\nversion: \"1\"\n---\n\n正文\n"}, "只能有 name 与 description"},
		"第一行之前有空行":           {"satchel-x", map[string]string{"SKILL.md": "\n" + ok}, "第一行要是 ---"},
		"全文 501 行":           {"satchel-x", map[string]string{"SKILL.md": skillMD("satchel-x", `description: "x"`, long)}, "超过 500 行"},
		"多了 notes.md":        {"satchel-x", map[string]string{"SKILL.md": ok, "notes.md": "x\n"}, "只能有 SKILL.md"},
		"名字 33 个字符":          {name33, map[string]string{"SKILL.md": skillMD(name33, `description: "x"`, "正文\n")}, "超过 32 个字符"},
	}
	if len(cases) != 11 {
		t.Fatalf("应当是十一种情况：%d", len(cases))
	}
	for label, tc := range cases {
		got := problemsOf(t, tc.dir, tc.files)
		if !strings.Contains(strings.Join(got, "；"), tc.want) {
			t.Errorf("%s：应当报出「%s」，得到 %v", label, tc.want, got)
		}
	}
}

// 名字不合规则（不以 satchel- 开头、有大写或连续连字符）同样拦下。
func TestNameRule(t *testing.T) {
	for _, dir := range []string{"basics", "satchel-Basics", "satchel--x", "satchel-x-"} {
		got := problemsOf(t, dir, map[string]string{"SKILL.md": skillMD(dir, `description: "x"`, "正文\n")})
		if !strings.Contains(strings.Join(got, "；"), "名字要以 satchel- 开头") {
			t.Errorf("%s 应当不合名字规则：%v", dir, got)
		}
	}
}

// master-skills「二进制里带着全部 skills」：编进二进制的文件与仓库里 files/ 下的文件集合相同、逐字节相同
// （go:embed 不收 . 与 _ 开头的文件，这类文件会在这里被发现）。
func TestEmbeddedMatchesFiles(t *testing.T) {
	if diff := compareFS(os.DirFS("files"), Files()); diff != "" {
		t.Fatal(diff)
	}
	// 核对本身：源目录多一个 _draft.md 时要报出来。
	src := fstest.MapFS{"satchel-x/SKILL.md": {Data: []byte("a")}, "satchel-x/_draft.md": {Data: []byte("b")}}
	bin := fstest.MapFS{"satchel-x/SKILL.md": {Data: []byte("a")}}
	if diff := compareFS(src, bin); !strings.Contains(diff, "_draft.md") {
		t.Fatalf("应当点名 _draft.md 不在二进制里：%q", diff)
	}
}

// compareFS 比较两份 FS 的普通文件：集合与内容都要相同，返回差异的说明。
func compareFS(src, bin fs.FS) string {
	files := func(fsys fs.FS) map[string][]byte {
		out := map[string][]byte{}
		_ = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				out[path], _ = fs.ReadFile(fsys, path)
			}
			return nil
		})
		return out
	}
	a, b := files(src), files(bin)
	var diffs []string
	for path, content := range a {
		got, ok := b[path]
		switch {
		case !ok:
			diffs = append(diffs, path+" 不在二进制里")
		case !bytes.Equal(got, content):
			diffs = append(diffs, path+" 与二进制里的不同")
		}
	}
	for path := range b {
		if _, ok := a[path]; !ok {
			diffs = append(diffs, path+" 只在二进制里")
		}
	}
	return strings.Join(diffs, "；")
}

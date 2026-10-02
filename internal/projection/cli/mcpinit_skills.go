package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/satchel/satchel/internal/base/skills"
	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// mcp init 写 skills（master-mcp，design 第 6、7 条）：编进二进制的 skills 写进 runtime 的用户级 skills 目录，
// 每个 skill 一个 <名字>/SKILL.md。只写 Satchel 自己的文件：内容相同的不写，不同的直接覆盖、不留备份；同名目录里别的文件不动。

// claudeConfigEnv 是 Claude Code 的配置目录环境变量。
const claudeConfigEnv = "CLAUDE_CONFIG_DIR"

// claudeConfigDir 确定 Claude Code 的配置目录：环境变量 CLAUDE_CONFIG_DIR，没设时 ~/.claude；fromEnv 报告是不是环境变量给的。
// 它设了却是空值或不是绝对路径（Claude Code 按字面使用，不展开 ~），或者进程环境里没设、~/.claude/settings.json 的 env 里设了它
// （这样设时 Claude Code 只把 settings.json 与 skills 换过去，MCP 登记仍在 ~/.claude.json），都是 config：Satchel 不替用户猜目录。
// 为此读 ~/.claude/settings.json：不存在当作没设，读不了或不是合法 JSON 也是 config。
func claudeConfigDir(home string, lookup func(string) (string, bool)) (dir string, fromEnv bool, err error) {
	if v, ok := lookup(claudeConfigEnv); ok {
		if v == "" || !filepath.IsAbs(v) {
			return "", false, v1.Newf(v1.CodeConfig, "环境变量 %s 是 %q：Claude Code 按字面使用它（不展开 ~），mcp init 不替你猜是哪个目录；没有签发令牌，没有写任何文件", claudeConfigEnv, v).
				WithNext("把 " + claudeConfigEnv + " 设成绝对路径，或去掉它，再重跑")
		}
		return filepath.Clean(v), true, nil
	}
	dir = filepath.Join(home, ".claude")
	path := filepath.Join(dir, "settings.json")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return dir, false, nil
	}
	if err != nil {
		return "", false, v1.Wrap(v1.CodeConfig, "读不了 "+path+"；没有签发令牌，没有写任何文件", err).WithNext("修好 " + path + " 的权限后重跑")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return dir, false, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", false, v1.Newf(v1.CodeConfig, "%s 不是合法的 JSON 对象（%v）；没有签发令牌，没有写任何文件", path, err).WithNext("修好 " + path + " 后重跑")
	}
	var env map[string]json.RawMessage
	if json.Unmarshal(top["env"], &env) == nil {
		if v, ok := env[claudeConfigEnv]; ok {
			return "", false, v1.Newf(v1.CodeConfig, "%s 的 env 里设了 %s（%s）：这样设时 Claude Code 只把 settings.json 与 skills 换到那个目录，MCP 登记仍在 ~/.claude.json；没有签发令牌，没有写任何文件",
				path, claudeConfigEnv, v).
				WithNext("把它从 " + path + " 的 env 里删掉，改设在启动 Claude Code 的环境里（如 shell 的配置文件），再在那个环境里重跑")
		}
	}
	return dir, false, nil
}

// resolveRuntimeEnv 补上只对某个 runtime 有用、要先检查的环境：claude-code 的配置目录。完整接入、--print、--skills-only
// 都在签发令牌与写文件之前调它一次，之后 settings.json、登记文件、片段与 skills 目录都用这一个结果。
func resolveRuntimeEnv(env *runtimeEnv, runtime string) error {
	if runtime != "claude-code" {
		return nil
	}
	dir, fromEnv, err := claudeConfigDir(env.home, os.LookupEnv)
	if err != nil {
		return err
	}
	env.claudeDir, env.claudeSet = dir, fromEnv
	return nil
}

// skillsDir 是 runtime 的用户级 skills 目录：Claude Code 是配置目录下的 skills/；Codex 是 ~/.agents/skills/（0.95 起的用户级目录，
// 跟着 HOME 走、与 CODEX_HOME 无关）；Hermes 是 $HERMES_HOME/skills/。
func skillsDir(env runtimeEnv, runtime string) string {
	switch runtime {
	case "claude-code":
		return filepath.Join(env.claudeDir, "skills")
	case "codex":
		return filepath.Join(env.home, ".agents", "skills")
	}
	return filepath.Join(env.hermesHome, "skills")
}

// errSkillsPath 是 skills 的位置写不了：被普通文件占着、是符号链接或读不了。它与 --skills-only 做的检查相同，所以提示修好路径后重跑。
type errSkillsPath struct {
	path, why string
}

func (e errSkillsPath) Error() string { return e.path + "：" + e.why }

// skillsPathError 把 errSkillsPath 包成 config，reason 后面接上 stopped（停在哪一步）；别的错误原样返回。
func skillsPathError(err error, stopped string) error {
	var p errSkillsPath
	if errors.As(err, &p) {
		return v1.Newf(v1.CodeConfig, "skills 的位置 %s；%s", p.Error(), stopped).WithNext("修好 " + p.path + " 后重跑")
	}
	return err
}

// planSkills 算出要写的 skills：编进二进制的每份 SKILL.md 对应 <dir>/<名字>/SKILL.md，内容相同的列为没变、不生成改动。
// skills 目录本身可以是符号链接（照常跟随）；它被普通文件占着、是指向不存在位置的符号链接或读不了，<名字> 或其中的 SKILL.md
// 是符号链接或不是目录与普通文件，已有的文件读不了，都返回 errSkillsPath。目录没有写权限要到写的时候才知道。
func planSkills(dir string) (edits []skillEdit, unchanged []string, err error) {
	info, err := os.Stat(dir)
	switch {
	case err == nil && !info.IsDir():
		return nil, nil, errSkillsPath{dir, "不是目录"}
	case errors.Is(err, fs.ErrNotExist):
		if link, lerr := os.Lstat(dir); lerr == nil && link.Mode()&fs.ModeSymlink != 0 {
			return nil, nil, errSkillsPath{dir, "是指向不存在位置的符号链接"}
		}
	case err != nil:
		return nil, nil, errSkillsPath{dir, fmt.Sprintf("读不了（%v）", err)}
	}
	list, problems, err := skills.Load(skills.Files())
	if err != nil || len(problems) > 0 {
		return nil, nil, v1.Newf(v1.CodeInternal, "编进二进制的 skills 读不出来：%v %v", err, problems)
	}
	for _, s := range list {
		sub := filepath.Join(dir, s.Name)
		path := filepath.Join(sub, skills.FileName)
		if info, err := os.Lstat(sub); err == nil && (info.Mode()&fs.ModeSymlink != 0 || !info.IsDir()) {
			return nil, nil, errSkillsPath{sub, "是符号链接或普通文件，mcp init 只写自己的目录"}
		}
		before, err := readSkillFile(path)
		if err != nil {
			return nil, nil, err
		}
		after := []byte(s.Content)
		if before != nil && bytes.Equal(before, after) {
			unchanged = append(unchanged, path)
			continue
		}
		edits = append(edits, skillEdit{name: s.Name, path: path, content: after})
	}
	return edits, unchanged, nil
}

// readSkillFile 读一份已有的 SKILL.md：不存在返回 nil；是符号链接、不是普通文件或读不了返回 errSkillsPath。
func readSkillFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errSkillsPath{path, fmt.Sprintf("读不了（%v）", err)}
	}
	if !info.Mode().IsRegular() {
		return nil, errSkillsPath{path, "是符号链接或不是普通文件，mcp init 只写自己的文件"}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errSkillsPath{path, fmt.Sprintf("读不了（%v）", err)}
	}
	if raw == nil {
		raw = []byte{}
	}
	return raw, nil
}

// initSkills 是写 skills 的结果：skills 目录、写了的与没变的文件。
type initSkills struct {
	Dir       string   `json:"dir"`
	Written   []string `json:"written"`
	Unchanged []string `json:"unchanged"`
}

// skillEdit 是要写的一份 SKILL.md：skill 名（也是目录名）、完整路径与新内容。
type skillEdit struct {
	name, path string
	content    []byte
}

// writeSkills 按计划写 skills，返回写到哪一步的结果（失败时 Written 是已经写了的）。内容不同就覆盖、不留备份（这些文件归
// Satchel 管）；新文件 0600、新目录 0700，已有的文件保留原权限。写入不顺着符号链接走到别处：skills 目录用 os.Root 固定成句柄
// （目录本身是符号链接时照常跟随），预检之后有人把 satchel-* 目录或其中的 SKILL.md 换成符号链接，这里停下。
func writeSkills(dir string, edits []skillEdit, unchanged []string) (initSkills, error) {
	res := initSkills{Dir: dir, Written: []string{}, Unchanged: append([]string{}, unchanged...)}
	if len(edits) == 0 {
		return res, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, v1.Wrap(v1.CodeConfig, "建目录 "+dir+" 失败", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return res, v1.Wrap(v1.CodeConfig, "打开 "+dir+" 失败", err)
	}
	defer root.Close()
	for _, e := range edits {
		if err := writeSkill(root, e.name, filepath.Join(dir, e.name), e.content); err != nil {
			return res, err
		}
		res.Written = append(res.Written, e.path)
	}
	return res, nil
}

// writeSkill 在 skills 目录的句柄里写 <name>/SKILL.md：<name> 用 Lstat 核对是目录（不是符号链接）后固定成子句柄，
// 并确认子句柄就是刚核对的那个目录；临时文件建在子句柄里，写完改名成 SKILL.md（改名替换的是这个名字本身，不跟随链接）。
// sub 是 <name> 的完整路径，只用于报错。
func writeSkill(root *os.Root, name, sub string, content []byte) error {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if err := root.Mkdir(name, 0o700); err != nil {
			return v1.Wrap(v1.CodeConfig, "建目录 "+sub+" 失败", err)
		}
		info, err = root.Lstat(name)
	}
	if err != nil {
		return v1.Wrap(v1.CodeConfig, "读不了 "+sub, err)
	}
	if !info.IsDir() {
		return errSkillsPath{sub, "是符号链接或普通文件，mcp init 只写自己的目录"}
	}
	d, err := root.OpenRoot(name)
	if err != nil {
		return v1.Wrap(v1.CodeConfig, "打开 "+sub+" 失败", err)
	}
	defer d.Close()
	if opened, err := d.Stat("."); err != nil || !os.SameFile(info, opened) {
		return errSkillsPath{sub, "在写入的过程中被换掉了"}
	}
	path := filepath.Join(sub, skills.FileName)
	perm := fs.FileMode(0o600)
	switch old, err := d.Lstat(skills.FileName); {
	case err == nil && !old.Mode().IsRegular():
		return errSkillsPath{path, "是符号链接或不是普通文件，mcp init 只写自己的文件"}
	case err == nil:
		perm = old.Mode().Perm()
	case !errors.Is(err, fs.ErrNotExist):
		return v1.Wrap(v1.CodeConfig, "读不了 "+path, err)
	}
	tmp, err := writeTemp(d, "."+skills.FileName+".tmp-", content, perm)
	if err != nil {
		return v1.Wrap(v1.CodeConfig, "写 "+path+" 失败", err)
	}
	if err := d.Rename(tmp, skills.FileName); err != nil {
		_ = d.Remove(tmp)
		return v1.Wrap(v1.CodeConfig, "写 "+path+" 失败", err)
	}
	return nil
}

// writeTemp 在目录句柄 d 里新建一个以 prefix 开头的临时文件（O_EXCL，名字撞上就换一个），写入 content 并落盘，返回它的名字。
func writeTemp(d *os.Root, prefix string, content []byte, perm fs.FileMode) (string, error) {
	for i := 0; ; i++ {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return "", err
		}
		name := prefix + hex.EncodeToString(suffix[:])
		f, err := d.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) && i < 10 {
			continue
		}
		if err != nil {
			return "", err
		}
		err = f.Chmod(perm)
		if err == nil {
			_, err = f.Write(content)
		}
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = d.Remove(name)
			return "", err
		}
		return name, nil
	}
}

// skillsErrReason 是写 skills 失败的原因：位置不对时点名那个路径。
func skillsErrReason(err error) string {
	var p errSkillsPath
	if errors.As(err, &p) {
		return p.Error()
	}
	return v1.AsError(err).Reason
}

// skillsNotes 是装 skills 之后给人的提示：什么时候生效、这几个目录归 Satchel 管、升级之后怎么更新。
func skillsNotes(runtime, dir string) []string {
	var live string
	switch runtime {
	case "claude-code":
		live = "skills 装在 " + dir + "：重启 Claude Code 之后生效；运行中的 Claude Code 通常几秒内自动发现，skills 目录是这次新建的就要执行 /reload-skills"
	case "codex":
		live = "skills 装在 " + dir + "：重启 Codex 之后生效；运行中的 Codex 通常几秒内的下一轮就能用上；Codex 0.95 之前的版本不读这个目录"
	default:
		live = "skills 装在 " + dir + "：重启 Hermes 之后生效，交互界面、gateway 这些长期运行的进程都要重启；不重启时可以在会话里执行 /reload-skills 先用上"
	}
	return []string{live,
		dir + " 下的 satchel-* 目录归 Satchel 管，改了会被下次 mcp init 覆盖（同一目录里别的文件不动）",
		"升级 satchel 之后执行 satchel mcp init --runtime " + runtime + " --skills-only 更新 skills"}
}

// skillsOnlyFlags 是不能与 --skills-only 同时给的 mcp init 自己的 flag（按 Flags 里出现与否判断，--preset 的默认值不算）。
var skillsOnlyFlags = []string{"use-token", "print", "url", "name", "preset", command.VerifyUserFlag, command.VerifyCodeFlag}

// skillsOnlyOutput 是 --skills-only 的输出：没有主控地址与令牌。
type skillsOnlyOutput struct {
	Runtime string     `json:"runtime"`
	Skills  initSkills `json:"skills"`
	Notes   []string   `json:"notes"`
}

// runSkillsOnly 是 --skills-only：只写 skills，不连主控、不检查 CLI 的连法、不签发也不读令牌、不改配置文件、不执行 runtime 的命令。
// 出错一律是 config（与别的 flag 混用是 usage）。
func runSkillsOnly(ctx context.Context, inv *command.Invocation, env runtimeEnv, runtime string) (any, error) {
	for _, name := range skillsOnlyFlags {
		if _, ok := inv.Flags[name]; ok {
			return nil, usageError("--skills-only 只和 --runtime 一起用，不能同时给 --%s", name)
		}
	}
	if f := connFlagsOf(ctx); f.serverSet || f.tokenSet {
		return nil, usageError("--skills-only 不连主控，不能同时给 --server 或 --token")
	}
	if err := resolveRuntimeEnv(&env, runtime); err != nil {
		return nil, err
	}
	dir := skillsDir(env, runtime)
	edits, unchanged, err := planSkills(dir)
	if err != nil {
		return nil, skillsPathError(err, "没有写任何文件")
	}
	res, err := writeSkills(dir, edits, unchanged)
	if err != nil {
		return nil, v1.Wrap(v1.CodeConfig, "写 skills 失败："+skillsErrReason(err), err).
			WithState("skills", res).WithNext("修好 " + dir + " 后重跑")
	}
	return skillsOnlyOutput{Runtime: runtime, Skills: res, Notes: skillsNotes(runtime, dir)}, nil
}

// renderSkills 是 skills 结果的文本形式：目录、写了的每个文件与没变的个数。
func renderSkills(b *strings.Builder, s initSkills) {
	fmt.Fprintf(b, "skills 目录：%s（写了 %d 个文件，没变 %d 个）\n", s.Dir, len(s.Written), len(s.Unchanged))
	for _, path := range s.Written {
		fmt.Fprintf(b, "写了 %s\n", path)
	}
}

func renderSkillsOnly(w io.Writer, out skillsOnlyOutput) error {
	var b strings.Builder
	fmt.Fprintf(&b, "已把 skills 装进 %s\n", out.Runtime)
	renderSkills(&b, out.Skills)
	for _, n := range out.Notes {
		fmt.Fprintf(&b, "提示：%s\n", n)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

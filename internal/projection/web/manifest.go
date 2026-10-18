package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"sync"

	"github.com/satchel/satchel/internal/command"
)

// ManifestFile 是网页产物里的页面清单（master-web「页面清单与命令表一致」）。
const ManifestFile = "satchel-web.json"

// Manifest 是页面清单：satchel-web 的版本、每页的「可见操作 → CLI 命令」对照表，以及网页调用的每条命令的 REST 方法与路径。
type Manifest struct {
	Version  string           `json:"version"`
	Pages    []Page           `json:"pages"`
	Commands map[string]Route `json:"commands"`
}

// Page 是清单里的一页。
type Page struct {
	ID      string   `json:"id"`
	Path    string   `json:"path"`
	Title   string   `json:"title"`
	Actions []Action `json:"actions"`
}

// Action 是一页上的一个可见操作：对应一条命令，或者一个会话入口（登录、两步验证第二步、登出、验证码配置不是命令）。
type Action struct {
	Label   string `json:"label"`
	Command string `json:"command,omitempty"`
	Session string `json:"session,omitempty"`
}

// Route 是网页调用一条命令用的 REST 方法与路径。
type Route struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// sessionEntries 是清单里能写的会话入口（master-web-session、master-login-protection）。
var sessionEntries = map[string]bool{"login": true, "two-factor": true, "logout": true, "captcha": true}

// LoadManifest 读网页产物里的页面清单；产物里没有清单时（开发构建）返回 nil、没有错误。
func LoadManifest(files fs.FS) (*Manifest, error) {
	raw, err := fs.ReadFile(files, ManifestFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("%s 不是合法的 JSON：%w", ManifestFile, err)
	}
	return &m, nil
}

var (
	embeddedOnce     sync.Once
	embeddedManifest *Manifest
	embeddedErr      error
)

// Embedded 是编进二进制的页面清单（只解析一次）；开发构建返回 nil。
func Embedded() (*Manifest, error) {
	embeddedOnce.Do(func() { embeddedManifest, embeddedErr = LoadManifest(Dist()) })
	return embeddedManifest, embeddedErr
}

// Entries 是清单提供的网页入口，给错误提示用（master-web「错误提示里的网页入口」）：有没有登录页，以及每条命令
// 由哪一页提供（出现在几页时取清单里的第一页）。m 为 nil 时等于没有网页。
func (m *Manifest) Entries() command.WebEntries {
	if m == nil {
		return command.WebEntries{}
	}
	e := command.WebEntries{Pages: map[string]string{}}
	for _, p := range m.Pages {
		if p.ID == "login" {
			e.Login = true
		}
		for _, a := range p.Actions {
			if _, seen := e.Pages[a.Command]; a.Command != "" && !seen {
				e.Pages[a.Command] = p.Title
			}
		}
	}
	return e
}

// CheckManifest 按 master-web 的规则核对清单与命令表，返回问题清单（没有问题时为空）：
// 清单里的每条命令都在命令表里、不是本地命令、不是隐藏命令，方法与路径与命令表推出的一致；每一页至少一个可见操作，
// 每个操作恰好写了命令或会话入口之一，对照表里的命令都登记了方法与路径；required 里的页面都在清单里。
func CheckManifest(t *command.Table, m *Manifest, required []string) []string {
	var problems []string
	bad := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	checkCommand := func(where, name string) bool {
		c, ok := t.Lookup(name)
		switch {
		case !ok:
			bad("%s：命令表里没有 %s", where, name)
		case c.Class == command.ClassLocal:
			bad("%s：%s 是本地命令，网页调不了", where, name)
		case c.Hidden:
			bad("%s：%s 是隐藏命令", where, name)
		default:
			return true
		}
		return false
	}
	ids := map[string]bool{}
	for _, p := range m.Pages {
		if ids[p.ID] {
			bad("页面 %s 出现了两次", p.ID)
		}
		ids[p.ID] = true
		if len(p.Actions) == 0 {
			bad("页面 %s 没有可见操作", p.ID)
		}
		for _, a := range p.Actions {
			where := "页面 " + p.ID + " 的「" + a.Label + "」"
			switch {
			case (a.Command == "") == (a.Session == ""):
				bad("%s 要恰好写命令或会话入口之一", where)
			case a.Session != "":
				if !sessionEntries[a.Session] {
					bad("%s：没有会话入口 %s", where, a.Session)
				}
			default:
				if checkCommand(where, a.Command) {
					if _, ok := m.Commands[a.Command]; !ok {
						bad("%s：%s 没有登记方法与路径", where, a.Command)
					}
				}
			}
		}
	}
	names := make([]string, 0, len(m.Commands))
	for name := range m.Commands {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		got := m.Commands[name]
		if !checkCommand("登记的命令", name) {
			continue
		}
		c, _ := t.Lookup(name)
		if want := c.Route(); got.Method != want.Method || got.Path != want.Path {
			bad("%s 登记成 %s %s，命令表推出的是 %s %s", name, got.Method, got.Path, want.Method, want.Path)
		}
	}
	for _, id := range required {
		if !ids[id] {
			bad("清单里缺页面 %s", id)
		}
	}
	return problems
}

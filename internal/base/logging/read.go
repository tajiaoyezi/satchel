package logging

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/satchel/satchel/internal/base/db"
)

// tailChunk 是从文件末尾往前读时每块的大小。
const tailChunk = 64 << 10

// Tail 从文件末尾往前读出最多 n 行，按文件里的先后顺序返回；末尾写到一半、还没有换行的那一行不算。
// 文件不存在时返回 os.ErrNotExist 包装的错误。
func Tail(path string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var chunks [][]byte // 从后往前读到的块
	newlines := 0
	pos := info.Size()
	for pos > 0 && newlines <= n {
		step := int64(tailChunk)
		if pos < step {
			step = pos
		}
		pos -= step
		b := make([]byte, step)
		if _, err := f.ReadAt(b, pos); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		chunks = append(chunks, b)
		newlines += bytes.Count(b, []byte{'\n'})
	}
	var buf bytes.Buffer
	for i := len(chunks) - 1; i >= 0; i-- {
		buf.Write(chunks[i])
	}
	data := buf.Bytes()
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return nil, nil
	}
	lines := strings.Split(string(data[:end]), "\n")
	if pos > 0 {
		lines = lines[1:] // 没读到文件开头时，第一段可能只是一行的后半截
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// Entry 是拆开的一行日志。拆不开时只有 Raw。
type Entry struct {
	Time  string            `json:"time,omitempty"`
	Level string            `json:"level,omitempty"`
	Msg   string            `json:"msg,omitempty"`
	Attrs map[string]string `json:"attrs,omitempty"`
	Raw   string            `json:"raw,omitempty"`
}

// Parse 把一行 slog 文本格式（time=… level=… msg=… 键=值 …）拆开：值可以是不带空格的裸值，或 Go 风格的带引号字符串。
// 缺 time、level、msg 任何一个，或格式对不上，就只给原文。
func Parse(line string) Entry {
	raw := Entry{Raw: line}
	attrs := map[string]string{}
	s := line
	for {
		s = strings.TrimLeft(s, " ")
		if s == "" {
			break
		}
		eq := strings.IndexByte(s, '=')
		if eq <= 0 || strings.ContainsAny(s[:eq], " \"") {
			return raw
		}
		key := s[:eq]
		s = s[eq+1:]
		var val string
		if strings.HasPrefix(s, `"`) {
			q, err := strconv.QuotedPrefix(s)
			if err != nil {
				return raw
			}
			if val, err = strconv.Unquote(q); err != nil {
				return raw
			}
			s = s[len(q):]
		} else if sp := strings.IndexByte(s, ' '); sp >= 0 {
			val, s = s[:sp], s[sp:]
		} else {
			val, s = s, ""
		}
		attrs[key] = val
	}
	e := Entry{Time: attrs["time"], Level: attrs["level"], Msg: attrs["msg"]}
	for _, k := range []string{"time", "level", "msg"} {
		if _, ok := attrs[k]; !ok {
			return raw
		}
		delete(attrs, k)
	}
	if len(attrs) > 0 {
		e.Attrs = attrs
	}
	return e
}

// File 是 logs/ 下的一个日志文件。
type File struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	Active   bool      `json:"active"`
}

// Files 列出 dir 下的日志文件：当前文件 satchel.log 与 lumberjack 轮转下来的 satchel-*.log，别的文件不列。
// 当前文件排第一，其余按修改时间从新到旧。目录不存在时返回空列表。
func Files(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	backup := strings.TrimSuffix(db.LogFile, ".log") + "-"
	var out []File
	for _, e := range entries {
		name := e.Name()
		active := name == db.LogFile
		if !e.Type().IsRegular() || !(active || strings.HasPrefix(name, backup) && strings.HasSuffix(name, ".log")) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // 列目录与取信息之间被轮转删掉了
		}
		out = append(out, File{Name: name, Size: info.Size(), Modified: info.ModTime(), Active: active})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Active != out[j].Active {
			return out[i].Active
		}
		return out[i].Modified.After(out[j].Modified)
	})
	return out, nil
}

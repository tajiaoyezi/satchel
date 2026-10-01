package selfupdate

import (
	"strconv"
	"strings"
)

// semver 是解析后的语义化版本（MAJOR.MINOR.PATCH[-PRE][+BUILD]）；ok 为假表示不是合法的语义化版本（如开发版的 dev）。
type semver struct {
	nums [3]int
	pre  []string
	ok   bool
}

func parse(v string) semver {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	v, _, _ = strings.Cut(v, "+")
	core, pre, hasPre := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	var out semver
	if len(parts) != 3 {
		return out
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || strconv.Itoa(n) != p {
			return out
		}
		out.nums[i] = n
	}
	if hasPre {
		if pre == "" {
			return out
		}
		out.pre = strings.Split(pre, ".")
		for _, id := range out.pre {
			if id == "" {
				return out
			}
		}
	}
	out.ok = true
	return out
}

// Valid 报告 v 是不是合法的语义化版本（`v` 前缀可有可无）。
func Valid(v string) bool { return parse(v).ok }

// Normalize 去掉 `v` 前缀与首尾空白。
func Normalize(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }

// Compare 按语义化版本比较 a 与 b：a 小于、等于、大于 b 时分别返回 -1、0、1。
// 预发布段排在同号正式版之前，逐段比较：纯数字的按数值、比非数字的小，非数字的按字典序，段少的小。
// 两边都要是合法的版本，调用方先用 Valid 检查；不合法的一边当作最小。
func Compare(a, b string) int {
	x, y := parse(a), parse(b)
	switch {
	case !x.ok && !y.ok:
		return 0
	case !x.ok:
		return -1
	case !y.ok:
		return 1
	}
	for i := range x.nums {
		if x.nums[i] != y.nums[i] {
			return sign(x.nums[i] - y.nums[i])
		}
	}
	switch {
	case len(x.pre) == 0 && len(y.pre) == 0:
		return 0
	case len(x.pre) == 0:
		return 1
	case len(y.pre) == 0:
		return -1
	}
	for i := 0; i < len(x.pre) && i < len(y.pre); i++ {
		if c := comparePre(x.pre[i], y.pre[i]); c != 0 {
			return c
		}
	}
	return sign(len(x.pre) - len(y.pre))
}

func comparePre(a, b string) int {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		return sign(ai - bi)
	case aerr == nil:
		return -1
	case berr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

package selfupdate

import "github.com/satchel/satchel/pkg/release"

// Valid 报告 v 是不是合法的语义化版本（`v` 前缀可有可无）。
func Valid(v string) bool { return release.Valid(v) }

// Normalize 去掉 `v` 前缀与首尾空白。
func Normalize(v string) string { return release.Normalize(v) }

// IsDev 报告 v 是不是开发版构建的版本号：不合法，或带有 -dev 后缀。
func IsDev(v string) bool { return release.IsDev(v) }

// Compare 按语义化版本比较 a 与 b。
func Compare(a, b string) int { return release.Compare(a, b) }

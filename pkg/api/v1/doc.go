// Package v1 是契约层：Satchel 对外的资源类型（kind、spec、status）、
// 四字段错误与退出码的定义，satchel-agent 也引用它。
// 本包只能引用标准库与第三方库，不能引用本仓库 internal/ 下的包；pkg/ 下的包（api/v1、以后与 satchel-agent 共用的 securechan、xrpc）
// 之间可以互引，但都不能碰 internal/——pkg/ 会被 satchel-agent 以 module 引用（第 02 章）。
package v1

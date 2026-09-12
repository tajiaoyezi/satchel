package v1

import (
	"errors"
	"fmt"
)

// Code 是机器可读的错误码。
type Code string

// 错误码。退出码的对应关系见 exitCodes；没列在那里的一律退出码 1。
const (
	CodeBadRequest            Code = "bad_request"
	CodeUnsupportedAPIVersion Code = "unsupported_api_version"
	CodeUnknownField          Code = "unknown_field"
	CodeFieldNotApplyable     Code = "field_not_applyable"
	CodeUnauthenticated       Code = "unauthenticated"
	CodeForbidden             Code = "forbidden"
	CodeNotFound              Code = "not_found"
	CodeVersionConflict       Code = "version_conflict"
	CodeConfirmRequired       Code = "confirm_required"
	CodePartialFailure        Code = "partial_failure"
	CodeHumanRequired         Code = "human_required"
	CodeNameTaken             Code = "name_taken"
	CodeAppendOnly            Code = "append_only"
	CodeDatabase              Code = "database"
	CodeInternal              Code = "internal"
)

// Error 是第 05 章功能⑤的四字段错误，REST、CLI、MCP 三个投影同一份。
// Reason 必须是人话，不能是未包装的内部错误文本；原始错误只在 Unwrap 链里。
type Error struct {
	Code   Code           `json:"code"`
	Reason string         `json:"reason"`
	State  map[string]any `json:"state"`
	Next   string         `json:"next"`
	cause  error
}

// New 构造一个错误。
func New(code Code, reason string) *Error {
	return &Error{Code: code, Reason: reason}
}

// Newf 是带格式化的 New。
func Newf(code Code, format string, args ...any) *Error {
	return New(code, fmt.Sprintf(format, args...))
}

// Wrap 构造一个包着底层错误的错误：cause 只能经 errors.Unwrap 拿到，不进 Reason。
func Wrap(code Code, reason string, cause error) *Error {
	return &Error{Code: code, Reason: reason, cause: cause}
}

func (e *Error) Error() string {
	if e.Reason == "" {
		return string(e.Code)
	}
	return string(e.Code) + ": " + e.Reason
}

// Unwrap 返回被包装的底层错误。
func (e *Error) Unwrap() error { return e.cause }

// WithState 记一项当前相关状态。
func (e *Error) WithState(key string, value any) *Error {
	if e.State == nil {
		e.State = map[string]any{}
	}
	e.State[key] = value
	return e
}

// WithNext 写建议的下一步。
func (e *Error) WithNext(next string) *Error {
	e.Next = next
	return e
}

// 退出码，第 05 章初版表。以后只能加码，不能改已有码的含义。
const (
	ExitOK              = 0
	ExitFailure         = 1 // 一般失败
	ExitUsage           = 2 // 用法错误
	ExitUnauthenticated = 3 // 认证失败
	ExitForbidden       = 4 // 权限不足
	ExitNotFound        = 5 // 对象不存在
	ExitVersionConflict = 6 // 版本冲突
	ExitConfirmRequired = 7 // 危险操作未获授权
	ExitPartialFailure  = 8 // 部分失败
	ExitHumanRequired   = 9 // 人类专属操作未验证身份
)

var exitCodes = map[Code]int{
	CodeBadRequest:            ExitUsage,
	CodeUnsupportedAPIVersion: ExitUsage,
	CodeUnknownField:          ExitUsage,
	CodeFieldNotApplyable:     ExitUsage,
	CodeUnauthenticated:       ExitUnauthenticated,
	CodeForbidden:             ExitForbidden,
	CodeNotFound:              ExitNotFound,
	CodeVersionConflict:       ExitVersionConflict,
	CodeConfirmRequired:       ExitConfirmRequired,
	CodePartialFailure:        ExitPartialFailure,
	CodeHumanRequired:         ExitHumanRequired,
}

// ExitCodeOf 把错误折算成进程退出码：nil 为 0，不是本包错误或没登记退出码的一律 1。
func ExitCodeOf(err error) int {
	if err == nil {
		return ExitOK
	}
	var e *Error
	if errors.As(err, &e) {
		if code, ok := exitCodes[e.Code]; ok {
			return code
		}
	}
	return ExitFailure
}

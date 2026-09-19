package v1

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Code 是机器可读的错误码。
type Code string

// 错误码。退出码的对应关系见 exitCodes；没列在那里的一律退出码 1。
// name_taken 只用于自然键冲突；其它唯一约束冲突与前置条件不满足用 conflict；
// usage 只给命令行用法错误（未知子命令、未知 flag、多余参数），退出码 2 只由它产生；
// bad_request 是请求内容不对（含 CHECK / 外键 / NOT NULL 违反），config 是配置文件或环境变量不合法，两者都退出码 1。
const (
	CodeUsage                 Code = "usage"
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
	CodeConflict              Code = "conflict"
	CodeAppendOnly            Code = "append_only"
	CodeSchemaMismatch        Code = "schema_mismatch"
	CodeDatabase              Code = "database"
	CodeConfig                Code = "config"
	CodeInternal              Code = "internal"
	// unsupported_platform：在只保证客户端的平台上要求起主控（第 09 章）；unavailable：CLI 连不上主控。两者退出码 1。
	CodeUnsupportedPlatform Code = "unsupported_platform"
	CodeUnavailable         Code = "unavailable"
)

// allCodes 是全部错误码，新加错误码要同时加进来：测试用它保证每个码都有 HTTP 状态码。
var allCodes = []Code{
	CodeUsage, CodeBadRequest, CodeUnsupportedAPIVersion, CodeUnknownField, CodeFieldNotApplyable,
	CodeUnauthenticated, CodeForbidden, CodeNotFound, CodeVersionConflict, CodeConfirmRequired,
	CodePartialFailure, CodeHumanRequired, CodeNameTaken, CodeConflict, CodeAppendOnly,
	CodeSchemaMismatch, CodeDatabase, CodeConfig, CodeInternal, CodeUnsupportedPlatform, CodeUnavailable,
}

// Codes 返回全部错误码，按声明顺序。
func Codes() []Code { return append([]Code(nil), allCodes...) }

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

// MarshalJSON 保证四个字段总在、类型不漂：state 为空时输出 {} 而不是 null。
func (e *Error) MarshalJSON() ([]byte, error) {
	state := e.State
	if state == nil {
		state = map[string]any{}
	}
	return json.Marshal(struct {
		Code   Code           `json:"code"`
		Reason string         `json:"reason"`
		State  map[string]any `json:"state"`
		Next   string         `json:"next"`
	}{e.Code, e.Reason, state, e.Next})
}

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
	CodeUsage:           ExitUsage,
	CodeUnauthenticated: ExitUnauthenticated,
	CodeForbidden:       ExitForbidden,
	CodeNotFound:        ExitNotFound,
	CodeVersionConflict: ExitVersionConflict,
	CodeConfirmRequired: ExitConfirmRequired,
	CodePartialFailure:  ExitPartialFailure,
	CodeHumanRequired:   ExitHumanRequired,
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

// httpStatuses 是错误码到 HTTP 状态码的折算表（resource-model「Structured error shape」）。
// 状态码只给通用 HTTP 工具看，客户端以 code 为准；没列的一律 500。
var httpStatuses = map[Code]int{
	CodeUsage:                 http.StatusBadRequest,
	CodeBadRequest:            http.StatusBadRequest,
	CodeUnknownField:          http.StatusBadRequest,
	CodeFieldNotApplyable:     http.StatusBadRequest,
	CodeUnsupportedAPIVersion: http.StatusBadRequest,
	CodeConfig:                http.StatusBadRequest,
	CodeUnauthenticated:       http.StatusUnauthorized,
	CodeForbidden:             http.StatusForbidden,
	CodeHumanRequired:         http.StatusForbidden,
	CodeNotFound:              http.StatusNotFound,
	CodeVersionConflict:       http.StatusConflict,
	CodeConflict:              http.StatusConflict,
	CodeNameTaken:             http.StatusConflict,
	CodeAppendOnly:            http.StatusConflict,
	CodeConfirmRequired:       http.StatusPreconditionRequired,
	CodeUnavailable:           http.StatusServiceUnavailable,
}

// HTTPStatusOf 把错误码折算成 HTTP 状态码；没登记的（internal、database、schema_mismatch、partial_failure、unsupported_platform）是 500。
func HTTPStatusOf(code Code) int {
	if status, ok := httpStatuses[code]; ok {
		return status
	}
	return http.StatusInternalServerError
}

// AsError 把任何 error 规范成四字段错误：已是四字段的原样返回，其它包成 internal。
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Wrap(CodeInternal, "命令执行失败", err)
}

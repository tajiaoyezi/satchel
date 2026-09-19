package v1

import (
	"bytes"
	"encoding/json"
)

// MarshalOutput 把一条命令的成功结果编成对外的 JSON 对象并注入 apiVersion。
// CLI 的 --json、REST 的响应 body、MCP satchel_run 的返回文本都用它，三处因此是同一个对象。
// 载荷必须能编码成 JSON 对象（结构体或 map），别的形状报 internal。
func MarshalOutput(payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, Wrap(CodeInternal, "编码 JSON 输出失败", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, Wrap(CodeInternal, "JSON 输出必须是对象", err)
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	fields["apiVersion"] = json.RawMessage(`"` + APIVersion + `"`)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(fields); err != nil {
		return nil, Wrap(CodeInternal, "编码 JSON 输出失败", err)
	}
	return buf.Bytes(), nil
}

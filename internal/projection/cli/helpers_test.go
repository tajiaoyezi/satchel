package cli

import "encoding/json"

// jsonUnmarshalFields 把已解成键值的 JSON 对象再解到结构体里。
func jsonUnmarshalFields(fields map[string]json.RawMessage, into any) error {
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

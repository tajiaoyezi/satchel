package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// writeError 把四字段错误写到 stderr：JSON 时是恰好四个键的对象；文本时逐行 code、reason、state、next，
// 底层原因（Unwrap 链）另起一行，只给人看。
func writeError(w io.Writer, e *v1.Error, asJSON bool) {
	if asJSON {
		_ = json.NewEncoder(w).Encode(e)
		return
	}
	fmt.Fprintf(w, "错误 %s：%s\n", e.Code, e.Reason)
	keys := make([]string, 0, len(e.State))
	for k := range e.State {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch v := e.State[k].(type) {
		case []string:
			fmt.Fprintf(w, "%s：\n", k)
			for _, item := range v {
				fmt.Fprintf(w, "  %s\n", item)
			}
		default:
			fmt.Fprintf(w, "%s：%v\n", k, v)
		}
	}
	if e.Next != "" {
		fmt.Fprintf(w, "下一步：%s\n", e.Next)
	}
	if cause := errors.Unwrap(e); cause != nil {
		fmt.Fprintf(w, "原因：%v\n", cause)
	}
}

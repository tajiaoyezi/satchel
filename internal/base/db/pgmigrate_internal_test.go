package db

import (
	"testing"
	"time"

	"github.com/satchel/satchel/internal/base/schema"
)

// 取值转换：认得的写法都转成 UTC；认不出的时间写法报错（迁移因此失败并点名），不塞一个错的时间。
func TestConvertValue(t *testing.T) {
	want := time.Date(2026, 9, 27, 1, 2, 3, 0, time.UTC)
	for _, s := range []string{"2026-09-27 01:02:03", "2026-09-27T01:02:03Z", "2026-09-27 09:02:03+08:00", "2026-09-27 01:02:03.000000000+00:00"} {
		got, err := convertValue(schema.TypeTime, s)
		if err != nil || !got.(time.Time).Equal(want) {
			t.Errorf("%q：%v %v", s, got, err)
		}
	}
	if _, err := convertValue(schema.TypeTime, "yesterday"); err == nil {
		t.Error("认不出的时间应当报错")
	}
	if v, _ := convertValue(schema.TypeBool, int64(1)); v != true {
		t.Error("1 应当转成 true")
	}
	if v, _ := convertValue(schema.TypeInt, nil); v != nil {
		t.Error("NULL 原样")
	}
	if _, err := convertValue(schema.TypeInt, "x"); err == nil {
		t.Error("类型不对应当报错")
	}
}

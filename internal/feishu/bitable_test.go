package feishu

import "testing"

// FieldText 需要能读多维表格里各种形态的值：单选返回裸字符串（名单「状态」列常见），
// 多行文本是富文本数组，公式是 {value:[...]}，人员是 [{id,name}]。
func TestFieldText(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, ""},
		{"单选纯字符串", "离职", "离职"},
		{"单选空字符串", "", ""},
		{"多行文本富文本数组", []any{
			map[string]any{"text": "在职", "type": "text"},
		}, "在职"},
		{"富文本多段用空格拼接", []any{
			map[string]any{"text": "王"},
			map[string]any{"text": "五"},
		}, "王 五"},
		{"数字单选编号", float64(3), "3"},
		{"公式字段", map[string]any{"type": float64(1), "value": []any{
			map[string]any{"text": "2024-01-31"},
		}}, "2024-01-31"},
		{"人员字段优先 en_name", []any{
			map[string]any{"id": "ou_x", "en_name": "Zhang San", "name": "张三"},
		}, "Zhang San"},
		{"人员字段退回 name", []any{
			map[string]any{"id": "ou_x", "name": "张三"},
		}, "张三"},
	}
	for _, c := range cases {
		if got := FieldText(c.in); got != c.want {
			t.Fatalf("%s: FieldText(%v) = %q, 期望 %q", c.name, c.in, got, c.want)
		}
	}
}

// 飞书多维表格（Bitable）读取：把「人员映射表」拉成本地身份映射。
//
// 用途：企微只有 userid（通讯录同步助手 secret 读不到姓名），
// 而多维表格里有「企微 userid → 飞书人员 → 中文姓名」的权威映射。
//
// 权限要点（实测踩过）：
//   - 必须开通 bitable:app:readonly（应用身份）
//   - 还必须把应用加为该多维表格的「文档应用」，否则一律 91403 Forbidden
//   - 两个都满足后用 tenant_access_token 访问
package feishu

import (
	"fmt"
	"net/url"
)

// BitableRecord 多维表格的一条记录。
type BitableRecord struct {
	RecordID string                 `json:"record_id"`
	Fields   map[string]interface{} `json:"fields"`
}

// BitableField 字段元信息。
type BitableField struct {
	FieldID   string `json:"field_id"`
	FieldName string `json:"field_name"`
	Type      int    `json:"type"`
	UIType    string `json:"ui_type"`
}

// SearchRecords 查询多维表格记录（无过滤条件，最多 500 条/页，自动翻页）。
func (c *Client) SearchRecords(token, appToken, tableID string) ([]BitableRecord, error) {
	var all []BitableRecord
	pageToken := ""
	for {
		q := url.Values{}
		q.Set("page_size", "500")
		q.Set("user_id_type", "open_id")
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		u := baseURL + "/bitable/v1/apps/" + appToken + "/tables/" + tableID + "/records/search?" + q.Encode()

		var out struct {
			apiResp
			Data struct {
				Items     []BitableRecord `json:"items"`
				HasMore   bool            `json:"has_more"`
				PageToken string          `json:"page_token"`
			} `json:"data"`
		}
		// 该接口是 POST 且需要空 body
		if err := c.postJSON(u, map[string]any{}, token, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Data.Items...)
		if !out.Data.HasMore || out.Data.PageToken == "" {
			break
		}
		pageToken = out.Data.PageToken
	}
	return all, nil
}

// ListBitableFields 列出多维表格字段（用于排查字段名不一致）。
func (c *Client) ListBitableFields(token, appToken, tableID string) ([]BitableField, error) {
	var out struct {
		apiResp
		Data struct {
			Items []BitableField `json:"items"`
		} `json:"data"`
	}
	u := baseURL + "/bitable/v1/apps/" + appToken + "/tables/" + tableID + "/fields?page_size=100"
	if err := c.getWithToken(u, token, &out); err != nil {
		return nil, err
	}
	return out.Data.Items, nil
}

// FieldText 从多维表格字段值里取纯文本。
// 飞书字段值形态多样：文本是 [{text,type}]，人员是 [{id,name,...}]，
// 公式是 {type,value:[...]}，单选是字符串。统一抽成文本。
func FieldText(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []interface{}:
		out := ""
		for _, item := range x {
			if s := FieldText(item); s != "" {
				if out != "" {
					out += " "
				}
				out += s
			}
		}
		return out
	case map[string]interface{}:
		// 公式字段：{"type":1,"value":[...]}
		if val, ok := x["value"]; ok {
			return FieldText(val)
		}
		// 人员字段优先中文名
		if n, ok := x["en_name"].(string); ok && n != "" {
			return n
		}
		if n, ok := x["name"].(string); ok && n != "" {
			return n
		}
		if t, ok := x["text"].(string); ok {
			return t
		}
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// FieldUser 从人员字段里取飞书用户信息（open_id / name）。
func FieldUser(v interface{}) (id, name string) {
	arr, ok := v.([]interface{})
	if !ok || len(arr) == 0 {
		return "", ""
	}
	m, ok := arr[0].(map[string]interface{})
	if !ok {
		return "", ""
	}
	id, _ = m["id"].(string)
	if n, _ := m["en_name"].(string); n != "" {
		name = n
	} else if n, _ := m["name"].(string); n != "" {
		name = n
	}
	return id, name
}

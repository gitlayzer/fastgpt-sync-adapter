package feishu

import (
	"fmt"
	"net/url"
)

// ListCompanyUserIDs 列出全公司员工的 open_id（应用身份）。
// 需要应用身份权限 contact:contact.base:readonly，且应用的通讯录范围为全部成员。
// 按部门取员工的接口只返回直属成员，所以先递归列出全部子部门，再逐个部门取。任何一步失败都返回错误，不返回半份名单。
func (c *Client) ListCompanyUserIDs(tenantToken string) ([]string, error) {
	depts := []string{"0"} // 根部门
	pageToken := ""
	for {
		q := url.Values{}
		q.Set("department_id_type", "open_department_id")
		q.Set("fetch_child", "true")
		q.Set("page_size", "50")
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		var out struct {
			apiResp
			Data struct {
				HasMore   bool   `json:"has_more"`
				PageToken string `json:"page_token"`
				Items     []struct {
					OpenDepartmentID string `json:"open_department_id"`
				} `json:"items"`
			} `json:"data"`
		}
		u := fmt.Sprintf("%s/contact/v3/departments/0/children?%s", baseURL, q.Encode())
		if err := c.getWithToken(u, tenantToken, &out); err != nil {
			return nil, err
		}
		for _, d := range out.Data.Items {
			if d.OpenDepartmentID != "" {
				depts = append(depts, d.OpenDepartmentID)
			}
		}
		if !out.Data.HasMore || out.Data.PageToken == "" {
			break
		}
		pageToken = out.Data.PageToken
	}

	seen := map[string]bool{}
	var ids []string
	for _, dept := range depts {
		pageToken := ""
		for {
			q := url.Values{}
			q.Set("department_id", dept)
			q.Set("department_id_type", "open_department_id")
			q.Set("user_id_type", "open_id")
			q.Set("page_size", "50")
			if pageToken != "" {
				q.Set("page_token", pageToken)
			}
			var out struct {
				apiResp
				Data struct {
					HasMore   bool   `json:"has_more"`
					PageToken string `json:"page_token"`
					Items     []struct {
						OpenID string `json:"open_id"`
					} `json:"items"`
				} `json:"data"`
			}
			u := fmt.Sprintf("%s/contact/v3/users/find_by_department?%s", baseURL, q.Encode())
			if err := c.getWithToken(u, tenantToken, &out); err != nil {
				return nil, err
			}
			for _, item := range out.Data.Items {
				if item.OpenID != "" && !seen[item.OpenID] {
					seen[item.OpenID] = true
					ids = append(ids, item.OpenID)
				}
			}
			if !out.Data.HasMore || out.Data.PageToken == "" {
				break
			}
			pageToken = out.Data.PageToken
		}
	}
	return ids, nil
}

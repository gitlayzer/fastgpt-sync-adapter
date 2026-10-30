package feishu

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// 按部门取员工只返回直属成员：要先列全部子部门（含翻页），再逐个部门取（含翻页），同一个人只算一次。
func TestListCompanyUserIDs(t *testing.T) {
	c := New("cli_test", "secret")
	c.HTTP = &http.Client{Transport: stubTransport(func(r *http.Request) string {
		q := r.URL.Query()
		switch {
		case strings.Contains(r.URL.Path, "/departments/0/children"):
			if q.Get("fetch_child") != "true" {
				t.Fatalf("应递归取子部门: %s", r.URL)
			}
			if q.Get("page_token") == "" {
				return `{"code":0,"data":{"has_more":true,"page_token":"p2","items":[{"open_department_id":"od1"}]}}`
			}
			return `{"code":0,"data":{"items":[{"open_department_id":"od2"}]}}`
		case q.Get("department_id") == "0":
			return `{"code":0,"data":{"items":[{"open_id":"ou_root"}]}}`
		case q.Get("department_id") == "od1" && q.Get("page_token") == "":
			return `{"code":0,"data":{"has_more":true,"page_token":"u2","items":[{"open_id":"ou_1"}]}}`
		case q.Get("department_id") == "od1":
			return `{"code":0,"data":{"items":[{"open_id":"ou_4"},{"open_id":"ou_2"}]}}`
		default:
			return `{"code":0,"data":{"items":[{"open_id":"ou_2"},{"open_id":"ou_3"}]}}`
		}
	})}
	got, err := c.ListCompanyUserIDs("tt")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[ou_root ou_1 ou_4 ou_2 ou_3]" {
		t.Fatalf("员工不对: %v", got)
	}
}

// 任何一步失败都返回错误，不返回半份名单。
func TestListCompanyUserIDsFailure(t *testing.T) {
	c := New("cli_test", "secret")
	c.HTTP = &http.Client{Transport: stubTransport(func(r *http.Request) string {
		if strings.Contains(r.URL.Path, "/departments/0/children") {
			return `{"code":0,"data":{"items":[{"open_department_id":"od1"}]}}`
		}
		if r.URL.Query().Get("department_id") == "od1" {
			return `{"code":99991672,"msg":"Access denied"}`
		}
		return `{"code":0,"data":{"items":[{"open_id":"ou_root"}]}}`
	})}
	if got, err := c.ListCompanyUserIDs("tt"); err == nil {
		t.Fatalf("应返回错误，实际 %v", got)
	}
}

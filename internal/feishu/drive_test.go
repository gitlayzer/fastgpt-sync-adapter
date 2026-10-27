package feishu

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// stubTransport 拦截飞书请求，按调用方给的函数返回响应体。
type stubTransport func(*http.Request) string

func (f stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(f(r))), Header: http.Header{}}, nil
}

// 文件夹超过一页：按 next_page_token 翻到第二页，两页的文件都要返回。
func TestListFolderChildrenPaging(t *testing.T) {
	var tokens []string
	c := New("cli_test", "secret")
	c.HTTP = &http.Client{Transport: stubTransport(func(r *http.Request) string {
		pt := r.URL.Query().Get("page_token")
		tokens = append(tokens, pt)
		switch {
		case len(tokens) > 5:
			return `{"code":1061002,"msg":"翻页没有前进，请求次数过多"}`
		case pt == "":
			return `{"code":0,"data":{"files":[{"token":"f1","type":"docx"},{"token":"f2","type":"docx"}],"has_more":true,"next_page_token":"p2"}}`
		case pt == "p2":
			return `{"code":0,"data":{"files":[{"token":"f3","type":"docx"}],"has_more":false}}`
		}
		return `{"code":1061002,"msg":"未知的 page_token"}`
	})}

	files, err := c.ListFolderChildren("ut", "fld")
	if err != nil {
		t.Fatalf("ListFolderChildren: %v（请求过的 page_token: %q）", err, tokens)
	}
	if len(files) != 3 || files[2].Token != "f3" {
		t.Fatalf("应返回两页共 3 个文件，实际 %d 个", len(files))
	}
	if len(tokens) != 2 || tokens[1] != "p2" {
		t.Fatalf("第二次请求应带 page_token=p2，实际 %q", tokens)
	}
}

// 说还有下一页却没给翻页标记：报错，不返回半截列表。
func TestListFolderChildrenMissingPageToken(t *testing.T) {
	n := 0
	c := New("cli_test", "secret")
	c.HTTP = &http.Client{Transport: stubTransport(func(r *http.Request) string {
		if n++; n > 5 {
			return `{"code":1061002,"msg":"翻页没有前进，请求次数过多"}`
		}
		return `{"code":0,"data":{"files":[{"token":"f1","type":"docx"}],"has_more":true}}`
	})}
	if files, err := c.ListFolderChildren("ut", "fld"); err == nil {
		t.Fatalf("应报错，实际返回 %d 个文件", len(files))
	}
	if n != 1 {
		t.Fatalf("缺翻页标记时应在第 1 次请求后就停下，实际请求了 %d 次", n)
	}
}

package feishu

import (
	"strings"
	"testing"
)

func TestAuthorizeURL(t *testing.T) {
	u := AuthorizeURL("cli_test", "http://127.0.0.1:8089/oauth/feishu/callback", "st1")
	if !strings.HasPrefix(u, "https://accounts.feishu.cn/open-apis/authen/v1/authorize?") {
		t.Fatalf("前缀不对: %s", u)
	}
	if !strings.Contains(u, "client_id=cli_test") {
		t.Fatalf("应为 client_id 参数: %s", u)
	}
	if !strings.Contains(u, "response_type=code") {
		t.Fatalf("缺 response_type: %s", u)
	}
	// scope 里必须含 offline_access（否则飞书不返回 refresh_token）
	if !strings.Contains(u, "offline_access") {
		t.Fatalf("scope 缺 offline_access: %s", u)
	}
	if !strings.Contains(u, "drive") {
		t.Fatalf("scope 缺 drive: %s", u)
	}
	// 逐项校验：飞书只授予链接里显式请求的 scope，漏一个就少一个，
	// 而症状是「后台明明开了权限但 token 里没有」，极难排查。
	for _, want := range []string{
		"offline_access",
		"drive%3Adrive",
		"drive%3Aexport%3Areadonly",
		"wiki%3Awiki%3Areadonly",
		"docx%3Adocument%3Areadonly",
		"space%3Adocument%3Aretrieve",
		"contact%3Auser.employee_id%3Areadonly",
		"search%3Adocs%3Aread",
		"auth%3Auser.id%3Aread",
		"mail%3Auser_mailbox.message%3Areadonly",
		"mail%3Auser_mailbox.message.body%3Aread",
		"minutes%3Aminutes",
		"minutes%3Aminutes.search%3Aread",
		"minutes%3Aminutes.transcript%3Aexport",
		"vc%3Anote%3Aread",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("scope 缺 %s: %s", want, u)
		}
	}
	if !strings.Contains(u, "state=st1") {
		t.Fatalf("缺 state: %s", u)
	}
	// redirect_uri 必须编码
	if !strings.Contains(u, "redirect_uri=http%3A%2F%2F127.0.0.1") {
		t.Fatalf("redirect_uri 未编码: %s", u)
	}
}

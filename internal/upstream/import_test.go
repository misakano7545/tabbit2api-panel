package upstream

import (
	"strings"
	"testing"
)

// 用户手里可能是 cURL、单行 Cookie 头、-b 参数或纯 cookie 串：都要能挖出 cookie。
func TestExtractCookie(t *testing.T) {
	want := "token=abc; user_id=u-1"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "devtools copy as curl",
			in:   `curl 'https://web.tabbit.ai/api/v0/user/base-info' -H 'Cookie: token=abc; user_id=u-1' -H 'Accept: */*'`,
			want: want,
		},
		{
			name: "single header line",
			in:   `Cookie: token=abc; user_id=u-1`,
			want: want,
		},
		{
			name: "curl -b",
			in:   `curl -b 'token=abc; user_id=u-1' https://web.tabbit.ai/`,
			want: want,
		},
		{
			name: "plain cookie",
			in:   `token=abc; user_id=u-1`,
			want: want,
		},
		{
			name: "bare jwt untouched",
			in:   "eyJhbGciOiJSUzI1NiJ9.e30.sig",
			want: "eyJhbGciOiJSUzI1NiJ9.e30.sig",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractCookie(tc.in); got != tc.want {
				t.Fatalf("extractCookie = %q，期望 %q", got, tc.want)
			}
		})
	}
	if !strings.Contains(extractCookie(cases[0].in), "token=abc") {
		t.Fatal("cURL 里应能取到 token")
	}
}

package upstream

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/misakano7545/tabbit2api-panel/internal/auth"
)

// 昵称口径：上游字段叫 display_name；为空时退到 JWT 的 name；都没有才用 uid。
// 旧实现只认 nickname/name/user_name，于是面板永远显示 uid。
func TestUserInfoNickname(t *testing.T) {
	cases := []struct {
		name string
		body string
		jwt  string
		want string
	}{
		{
			name: "display_name 优先",
			body: `{"user_info":{"id":"u-1","display_name":"小明"}}`,
			want: "小明",
		},
		{
			name: "display_name 为空 → JWT name",
			body: `{"user_info":{"id":"u-1","display_name":""}}`,
			jwt:  fakeJWT(`{"id":"u-1","name":"user_4b24e854fd25"}`),
			want: "user_4b24e854fd25",
		},
		{
			name: "都没有 → 留空（由调用方保留原值）",
			body: `{"user_info":{"id":"u-1","display_name":""}}`,
			jwt:  fakeJWT(`{"id":"u-1"}`),
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v0/user/base-info", func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			client := New()
			client.SetHosts(srv.URL, srv.URL)
			token := tc.jwt
			if token == "" {
				token = "not-a-jwt"
			}
			account := &auth.Auth{Data: auth.Data{Token: token, UID: "u-1", Realm: auth.RealmIntl}}
			_, nick, _, err := client.GetUserInfo(account)
			if err != nil {
				t.Fatalf("GetUserInfo: %v", err)
			}
			if tc.want == "" {
				if nick != "" {
					t.Fatalf("昵称 = %q，期望留空", nick)
				}
				return
			}
			if nick != tc.want {
				t.Fatalf("昵称 = %q，期望 %q", nick, tc.want)
			}
		})
	}
}

// 拿到真名要写回账号对象（面板刷新后 SaveAtomic 才能落盘）。
func TestUserInfoWritesNicknameBack(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/user/base-info", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"user_info":{"id":"u-1","display_name":"小明"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := New()
	client.SetHosts(srv.URL, srv.URL)
	account := &auth.Auth{Data: auth.Data{Token: "cookie", UID: "u-1", Nickname: "u-1", Realm: auth.RealmIntl}}
	if _, _, _, err := client.GetUserInfo(account); err != nil {
		t.Fatalf("GetUserInfo: %v", err)
	}
	if got := account.Snapshot().Nickname; got != "小明" {
		t.Fatalf("账号对象昵称 = %q，期望写回 小明", got)
	}
}

// fakeJWT 造一个未签名 JWT（只要 payload 段，不验签的解析路径就够用）。
func fakeJWT(payload string) string {
	const header = "eyJhbGciOiJub25lIn0" // {"alg":"none"}
	return header + "." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + "."
}

package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 登录态在 Set-Cookie 里：要按行解析，只留 5 个关键键，其余（统计/一次性）丢掉。
//
// 这里用真实抓包观察到的 Set-Cookie 形状（含 Domain/Path/Expires/HttpOnly/SameSite），
// 因为最容易出错的就是 "Expires=Wed, 21 Oct 2026 07:28:00 GMT" 里那个逗号——
// 按逗号切分会把一条 cookie 切成两条。
func TestCookieFromSetCookie(t *testing.T) {
	jwt := "eyJhbGciOiJSUzI1NiJ9.payload.sig"
	header := http.Header{}
	header.Add("Set-Cookie", "token="+jwt+"; Domain=web.tabbit.com; Path=/; Expires=Wed, 21 Oct 2026 07:28:00 GMT; HttpOnly; Secure")
	header.Add("Set-Cookie", "user_id=u-123; Path=/")
	header.Add("Set-Cookie", "SAPISID=u-123; Path=/")
	header.Add("Set-Cookie", "managed=tab_browser; Path=/")
	header.Add("Set-Cookie", "NEXT_LOCALE=zh; Path=/")
	header.Add("Set-Cookie", "_gcl_au=1.1.123; Path=/")

	got := cookieFromSetCookie(header)
	want := "token=" + jwt + "; user_id=u-123; SAPISID=u-123; managed=tab_browser; NEXT_LOCALE=zh"
	if got != want {
		t.Fatalf("cookie = %q\n期望 %q", got, want)
	}
	if strings.Contains(got, "_gcl_au") {
		t.Error("统计类 cookie 不该带进登录态")
	}
}

// 登录接口只回 access_token、没有 Set-Cookie 时，用 body 拼一份可用的 cookie。
func TestSmsLoginFallsBackToAccessToken(t *testing.T) {
	const jwt = "eyJhbGciOiJSUzI1NiJ9.eyJpZCI6InUtOSIsIm5hbWUiOiJ1c2VyX2FiYyJ9.sig"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == SmsLoginPath {
			_, _ = io.WriteString(w, `{"success":true,"user_id":"u-9","access_token":"`+jwt+`"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.SetHosts(srv.URL, srv.URL)
	res, err := c.SmsLogin(context.Background(), "cn", "13800000000", "123456", "uuid-1")
	if err != nil {
		t.Fatalf("SmsLogin: %v", err)
	}
	if !strings.Contains(res.Cookie, "token="+jwt) {
		t.Fatalf("cookie 里没有 token: %q", res.Cookie)
	}
	if !strings.Contains(res.Cookie, "user_id=u-9") {
		t.Errorf("cookie 里没有 user_id: %q", res.Cookie)
	}
	if res.UID != "u-9" {
		t.Errorf("uid = %q", res.UID)
	}
}

// 上游用 code=非0 + error 表达「发送太频繁」这类业务失败：要回人话，而不是当成功。
func TestSendLoginCodeSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"code":1005,"error":"验证码发送过于频繁，请稍后再试","data":null}`)
	}))
	defer srv.Close()

	c := New()
	c.SetHosts(srv.URL, srv.URL)
	err := c.SendLoginCode(context.Background(), "cn", "13800000000", "uuid-1")
	if err == nil {
		t.Fatal("code 非 0 应返回错误")
	}
	var codeErr *LoginCodeError
	if !asLoginCodeError(err, &codeErr) {
		t.Fatalf("错误类型 = %T，期望 *LoginCodeError", err)
	}
	if !strings.Contains(codeErr.Msg, "频繁") {
		t.Errorf("错误文案 = %q，应保留上游原话", codeErr.Msg)
	}
}

// 上游的 FastAPI 校验错误形如 {"detail":{"msg":"Value error, 手机号格式错误"}}：
// 面板要显示「手机号格式错误」，不要带英文前缀。
func TestLoginErrorTextStripsValidatorPrefix(t *testing.T) {
	raw := []byte(`{"detail":[{"type":"value_error","loc":["body","mobile"],"msg":"Value error, 手机号格式错误"}]}`)
	if got := loginErrorText(422, raw); got != "手机号格式错误" {
		t.Fatalf("登录错误文案 = %q", got)
	}
}

// asLoginCodeError 是 errors.As 的小包装（避免测试里再导 errors）。
func asLoginCodeError(err error, target **LoginCodeError) bool {
	for err != nil {
		if e, ok := err.(*LoginCodeError); ok {
			*target = e
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

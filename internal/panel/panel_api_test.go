package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/misakano7545/tabbit2api-panel/internal/auth"
	"github.com/misakano7545/tabbit2api-panel/internal/pool"
	"github.com/misakano7545/tabbit2api-panel/internal/upstream"
)

// newTestPanel 组装一个带单账号、指向假上游的面板。
func newTestPanel(t *testing.T) (*Panel, *upstream.Client, *pool.Pool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/sign-key":
			_, _ = io.WriteString(w, "test-sign-key")
		case "/proxy/v1/model_config/models":
			_, _ = io.WriteString(w, `{"models":[{"display_name":"GPT-5.5","model_access_type":"free_unlimited",
				"supports_images":true,"support_thinking":true,"context_window":200000,"max_tokens":32000,
				"display_multiplier":1}]}`)
		case "/api/commerce/quota/v1/usage":
			_, _ = io.WriteString(w, `{"usage_percentage":12.5,"member_level":"free"}`)
		case "/api/commerce/activity/v1/sign-in/status":
			_, _ = io.WriteString(w, `{"results":[{"scene_code":"daily_sign_in","activity_open":true,"signed_today":false}]}`)
		case "/api/commerce/activity/v1/sign-in":
			_, _ = io.WriteString(w, `{"results":[{"scene_code":"daily_sign_in","sign_in_result":"success","usage_reward_result":"skipped"}]}`)
		case "/api/v0/user/base-info":
			_, _ = io.WriteString(w, `{"user_info":{"user_id":"u-1","nickname":"测试号"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.SetHosts(srv.URL, srv.URL)

	p := pool.New("")
	p.Add(&auth.Auth{Data: auth.Data{
		Token: "dt-1", UID: "u-1", Nickname: "测试号", Realm: auth.RealmIntl,
		ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(),
	}})
	h := &apiShim{up: up}
	pn := New(Config{
		Pool: p, Upstream: up, AuthDir: t.TempDir(), Version: "test",
		Models: h.models, Usage: nil,
	})
	return pn, up, p
}

// smsFake 记录短信登录请求的关键字段，供断言用。
type smsFake struct {
	mu         sync.Mutex
	lastMobile string
	lastUUID   string
	lastCode   string
}

func (f *smsFake) LastLoginMobile() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastMobile
}

// newTestPanelWithSMS 组装一个「支持短信登录」的假上游 + 面板。
//
// 假上游按真实契约回：send-verification-code 返回 {code:0}，oauth/login 用
// Set-Cookie 下发 token（登录态就在响应头里，这是本仓必须读 header 的原因）。
func newTestPanelWithSMS(t *testing.T) (*Panel, *smsFake, *pool.Pool) {
	t.Helper()
	fake := &smsFake{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/proxy/v0/oauth/send-verification-code":
			var body struct {
				Mobile string `json:"mobile"`
				UUID   string `json:"uuid"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fake.mu.Lock()
			fake.lastMobile, fake.lastUUID = body.Mobile, body.UUID
			fake.mu.Unlock()
			_, _ = io.WriteString(w, `{"code":0,"error":null,"data":null}`)
		case "/proxy/v0/oauth/login":
			var body struct {
				Mobile  string `json:"mobile"`
				SmsCode string `json:"smsCode"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fake.mu.Lock()
			fake.lastMobile, fake.lastCode = body.Mobile, body.SmsCode
			fake.mu.Unlock()
			w.Header().Add("Set-Cookie", "token=sms-token-value; Path=/; HttpOnly")
			w.Header().Add("Set-Cookie", "user_id=sms-uid; Path=/")
			_, _ = io.WriteString(w, `{"success":true,"message":"登录成功","user_id":"sms-uid","access_token":"sms-token-value"}`)
		case "/api/v0/user/base-info":
			_, _ = io.WriteString(w, `{"user_info":{"id":"sms-uid","display_name":"短信号"}}`)
		case "/chat/sign-key":
			_, _ = io.WriteString(w, "test-sign-key")
		case "/api/commerce/quota/v1/usage":
			_, _ = io.WriteString(w, `{"usage_percentage":5,"member_level":"free"}`)
		case "/proxy/v1/model_config/models":
			_, _ = io.WriteString(w, `{"models":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	up := upstream.New()
	up.SetHosts(srv.URL, srv.URL)
	p := pool.New("")
	pn := New(Config{Pool: p, Upstream: up, AuthDir: t.TempDir(), Version: "test"})
	return pn, fake, p
}

// apiShim 把 /v1/models 的形状借给面板（面板只要一个 []map）。
type apiShim struct{ up *upstream.Client }

func (s *apiShim) models() []map[string]any {
	out := []map[string]any{}
	for _, mi := range s.up.Catalog() {
		out = append(out, map[string]any{"id": mi.Realm + ":" + mi.ID, "name": mi.Name})
	}
	return out
}

func call(t *testing.T, pn *Panel, method, path, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = "127.0.0.1:5555" // 空密钥 = 只信本机；测试请求要落在回环上
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestOverview(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	code, body := call(t, pn, http.MethodGet, "/panel/api/overview", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["total"].(float64) != 1 {
		t.Errorf("total = %v", body["total"])
	}
	rows := body["accounts"].([]any)
	if len(rows) != 1 {
		t.Fatalf("账号行数 = %d", len(rows))
	}
	row := rows[0].(map[string]any)
	if row["uid"] != "u-1" {
		t.Errorf("uid 没有回显: %v", row["uid"])
	}
}

func TestPackages(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	code, body := call(t, pn, http.MethodGet, "/panel/api/packages", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	list := body["accounts"].([]any)
	first := list[0].(map[string]any)
	if first["remain"].(float64) != 87 { // 100 - 12.5 已用
		t.Errorf("remain = %v（应为剩余百分比）", first["remain"])
	}
	if first["size"].(float64) != 100 {
		t.Errorf("size = %v（百分比制的分母恒为 100）", first["size"])
	}
	// Tabbit 没有积分包：不要再编「剩余额度/已用额度」两个假包出来。
	if packs := first["packages"].([]any); len(packs) != 0 {
		t.Errorf("packages = %v（百分比制应返回空列表）", packs)
	}
	// 已用百分比原值要透出，面板据此画进度条与提示。
	if used := first["used_percent"].(float64); used != 12.5 {
		t.Errorf("used_percent = %v（应为上游 usage_percentage 原值）", used)
	}
}

func TestCheckinStatusEndpoint(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	code, body := call(t, pn, http.MethodGet, "/panel/api/checkin_status", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["unchecked"].(float64) != 1 {
		t.Errorf("unchecked = %v（本地没签到时应为 1）", body["unchecked"])
	}
}

// 导入：Tabbit 只认 cookie 串（token=…; user_id=…），导入后要落盘入池。
func TestImportCookie(t *testing.T) {
	pn, _, p := newTestPanel(t)
	code, body := call(t, pn, http.MethodPost, "/panel/api/accounts/import",
		"token=dummy.jwt.value; user_id=u-1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %v", code, body)
	}
	if body["action"] != "imported" {
		t.Errorf("action = %v", body["action"])
	}
	if _, ok := p.Status("u-1"); !ok {
		t.Errorf("导入后账号应进池")
	}
}

// 短信登录：发送验证码（start）由面板调上游；这里只验证「空手机号被拒」。
func TestLoginStartRequiresMobile(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	code, body := call(t, pn, http.MethodPost, "/panel/api/login/start", `{"realm":"cn"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(fmt.Sprint(body["error"]), "手机号") {
		t.Errorf("错误文案应提示填手机号: %v", body["error"])
	}
}

// 短信登录第二步：验证码换 cookie → 落盘入池。假上游用 Set-Cookie 下发 token。
func TestLoginVerifySavesAccount(t *testing.T) {
	pn, up, p := newTestPanelWithSMS(t)
	code, body := call(t, pn, http.MethodPost, "/panel/api/login/start",
		`{"realm":"cn","mobile":"13800000000"}`)
	if code != http.StatusOK {
		t.Fatalf("start status = %d, body = %v", code, body)
	}
	id := fmt.Sprint(body["id"])
	if id == "" || id == "<nil>" {
		t.Fatalf("没有拿到会话 id: %v", body)
	}
	code, body = call(t, pn, http.MethodPost, "/panel/api/login/verify",
		`{"id":"`+id+`","code":"123456"}`)
	if code != http.StatusOK {
		t.Fatalf("verify status = %d, body = %v", code, body)
	}
	if fmt.Sprint(body["realm"]) != "cn" {
		t.Errorf("realm = %v（应为本站点 cn）", body["realm"])
	}
	if _, ok := p.Status("sms-uid"); !ok {
		t.Fatalf("登录后应把账号入池（现有 %v）", p.List())
	}
	if got := up.LastLoginMobile(); got != "13800000000" {
		t.Errorf("上游收到的手机号 = %q", got)
	}
}

// 登录方式与地区绑定：手机号只服务国内站，国际站走凭证导入。
// 越界组合在服务端就被挡住，这里验证拒绝的理由是人话（指向正确做法），
// 而不是把上游的 noBetaPermission 直接甩给用户。
func TestLoginMethodRealmBinding(t *testing.T) {
	pn, _, _ := newTestPanelWithSMS(t)
	cases := []struct {
		name string
		path string
		body string
		want string
	}{
		{"手机号登录跑国际版", "/panel/api/login/start", `{"realm":"intl","mobile":"14085551234"}`, "国内版"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := call(t, pn, http.MethodPost, tc.path, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("status = %d（越界组合应被拒）", code)
			}
			msg := fmt.Sprint(body["error"])
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("错误文案 = %q，应指向正确的地区", msg)
			}
		})
	}
}

// 国内版手机号登录仍然可用（不要因为加了绑定把正常路径也挡了）。
func TestSMSLoginStillWorksForCN(t *testing.T) {
	pn, fake, _ := newTestPanelWithSMS(t)
	code, body := call(t, pn, http.MethodPost, "/panel/api/login/start",
		`{"realm":"cn","mobile":"13800000000"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %v", code, body)
	}
	if got := fake.LastLoginMobile(); got != "13800000000" {
		t.Errorf("上游收到手机号 = %q", got)
	}
}

// token 直登：一条 cookie token → userinfo 探活 → 落盘入池。
func TestLoginBareToken(t *testing.T) {
	pn, _, p := newTestPanel(t)
	code, body := call(t, pn, http.MethodPost, "/panel/api/login/refresh",
		`{"token":"dummy.jwt.value","realm":"intl"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body = %v", code, body)
	}
	if body["uid"] != "u-1" {
		t.Errorf("uid = %v", body["uid"])
	}
	if _, ok := p.Status("u-1"); !ok {
		t.Errorf("账号应进池")
	}
}

func TestLoginRejectsEmptyToken(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	code, _ := call(t, pn, http.MethodPost, "/panel/api/login/refresh", `{"token":""}`)
	if code != http.StatusBadRequest {
		t.Errorf("空 token 应 400，得到 %d", code)
	}
}

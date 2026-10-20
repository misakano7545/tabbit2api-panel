package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	packs := first["packages"].([]any)
	if len(packs) == 0 {
		t.Fatalf("额度构成为空")
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

// Tabbit 没有设备码授权：login/start 如实报错，用户改走「导入凭证」。
func TestDeviceLoginUnsupported(t *testing.T) {
	pn, _, _ := newTestPanel(t)
	code, body := call(t, pn, http.MethodPost, "/panel/api/login/start", `{"realm":"cn"}`)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(fmt.Sprint(body["error"]), "导入账号") {
		t.Errorf("错误文案应指向导入账号: %v", body["error"])
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

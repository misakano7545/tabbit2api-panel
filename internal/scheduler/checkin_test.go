package scheduler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/misakano7545/tabbit2api-panel/internal/auth"
	"github.com/misakano7545/tabbit2api-panel/internal/pool"
	"github.com/misakano7545/tabbit2api-panel/internal/upstream"
)

// Tabbit 的签到是真实接口（sign-in/status + sign-in）：调度器要真的去签，
// 并且把上游返回的 sign_in_result 如实翻译成结果，而不是永远 skipped。
func TestCheckinCallsUpstream(t *testing.T) {
	signed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/commerce/activity/v1/sign-in/status":
			state := "false"
			if signed {
				state = "true"
			}
			_, _ = io.WriteString(w, `{"results":[{"scene_code":"daily_sign_in","activity_open":true,"signed_today":`+state+`}]}`)
		case "/api/commerce/activity/v1/sign-in":
			signed = true
			_, _ = io.WriteString(w, `{"results":[{"scene_code":"daily_sign_in","sign_in_result":"success","signed_days":1,"usage_reward_result":"skipped"}]}`)
		case "/api/commerce/quota/v1/usage":
			_, _ = io.WriteString(w, `{"usage_percentage":0,"member_level":"free"}`)
		case "/chat/sign-key":
			_, _ = io.WriteString(w, "test-sign-key")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	up := upstream.New()
	up.SetHosts(srv.URL, srv.URL)
	p := pool.New("")
	p.Add(&auth.Auth{Data: auth.Data{
		Token: "dt", UID: "u-1", Realm: auth.RealmIntl,
		ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(),
	}})
	s := New(Config{Pool: p, Upstream: up, CheckinEnabled: true, KeepaliveEnabled: true})

	res, err := s.CheckinUID("u-1")
	if err != nil {
		t.Fatalf("CheckinUID 出错: %v", err)
	}
	if res.Status == "skipped" {
		t.Fatalf("状态 = %q：上游有签到接口，不该再跳过", res.Status)
	}
	if !signed {
		t.Fatalf("没有真的调用 sign-in 接口（状态 %q）", res.Status)
	}
}

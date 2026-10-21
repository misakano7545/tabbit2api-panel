package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/misakano7545/tabbit2api-panel/internal/auth"
)

// 额度响应里带 remaining_reset_minutes 时，要换算成绝对重置时刻；周期字段原样透出。
// 前端据此显示「6天21小时后重置」——存绝对时刻而不是分钟数，两次刷新之间倒计时才不会停。
func TestQuotaResetAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"user_id":"u-1","member_level":"free","usage_percentage":"8.07%",
			"remaining_reset_minutes":9634,"current_cycle_start":"2026.10.20","current_cycle_end":"2026.10.27",
			"unused_reset_coupon_count":2}`))
	}))
	defer srv.Close()

	c := New()
	c.SetHosts(srv.URL, srv.URL)
	account := &auth.Auth{Data: auth.Data{Token: "t", UID: "u-1", Realm: auth.RealmCN}}
	usage, err := c.UserEntUsageDetail(account)
	if err != nil {
		t.Fatalf("UserEntUsageDetail: %v", err)
	}
	if usage.UsedPercent != 8.07 {
		t.Errorf("used = %v", usage.UsedPercent)
	}
	if usage.Remain != 91 {
		t.Errorf("remain = %v（100-8.07 取整）", usage.Remain)
	}
	want := time.Now().Add(9634 * time.Minute).Unix()
	if diff := usage.ResetAt - want; diff > 5 || diff < -5 {
		t.Errorf("reset_at = %d，期望约 %d（remaining_reset_minutes 换算）", usage.ResetAt, want)
	}
	if usage.CycleStart != "2026.10.20" || usage.CycleEnd != "2026.10.27" {
		t.Errorf("cycle = %q~%q", usage.CycleStart, usage.CycleEnd)
	}
	if usage.ResetCoupons != 2 {
		t.Errorf("coupons = %d", usage.ResetCoupons)
	}
}

// 只有小时字段时也要能算出来（上游个别环境只给 remaining_reset_hours）。
func TestQuotaResetAtHoursOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage_percentage":"1%","remaining_reset_hours":"167.86"}`))
	}))
	defer srv.Close()

	c := New()
	c.SetHosts(srv.URL, srv.URL)
	account := &auth.Auth{Data: auth.Data{Token: "t", UID: "u-1", Realm: auth.RealmIntl}}
	usage, err := c.UserEntUsageDetail(account)
	if err != nil {
		t.Fatalf("UserEntUsageDetail: %v", err)
	}
	if usage.ResetAt == 0 {
		t.Fatal("remaining_reset_hours 没被换算成 reset_at")
	}
}

// 两个字段都缺时退到 current_cycle_end（按当天 00:00 起算次日），不要把「无重置时间」抛给用户。
func TestQuotaResetAtCycleEndFallback(t *testing.T) {
	today := time.Now()
	end := today.Format("2006.1.2")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := json.Marshal(map[string]any{
			"usage_percentage": "0.5%", "current_cycle_end": end,
		})
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	c := New()
	c.SetHosts(srv.URL, srv.URL)
	account := &auth.Auth{Data: auth.Data{Token: "t", UID: "u-1", Realm: auth.RealmCN}}
	usage, err := c.UserEntUsageDetail(account)
	if err != nil {
		t.Fatalf("UserEntUsageDetail: %v", err)
	}
	if usage.ResetAt == 0 {
		t.Fatal("没有 remaining_* 时应退到 current_cycle_end")
	}
	if usage.ResetAt <= time.Now().Unix() {
		t.Errorf("reset_at = %d 应晚于现在", usage.ResetAt)
	}
}

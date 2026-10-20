package scheduler

import (
	"testing"
	"time"

	"github.com/misakano7545/tabbit2api-panel/internal/auth"
	"github.com/misakano7545/tabbit2api-panel/internal/pool"
	"github.com/misakano7545/tabbit2api-panel/internal/upstream"
)

// Tabbit 没有签到：CheckinUID 要如实回 skipped，且不能因此去打额度接口
// （上游没开签到不等于账号有问题，把整轮判成失败会让面板误报）。
func TestCheckinSkippedOnTabbit(t *testing.T) {
	up := upstream.New()
	up.SetHosts("http://127.0.0.1:1", "http://127.0.0.1:1") // 任何真实调用都会立刻失败
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
	if res.Status != "skipped" {
		t.Fatalf("状态 = %q，应如实回 skipped", res.Status)
	}
}

// account.go 账号侧口径：额度快照、登录态探活、凭证准备。
//
// Tabbit 的额度只有一个接口：GET /api/commerce/quota/v1/usage?user_id=…
// 返回 usage_percentage（已用百分比）与 member_level（免费/Pro）。它没有签到、
// 没有积分包、没有续期接口——所以这里把「额度」统一表达成百分比，不编造金额。
package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/misakano7545/tabbit2api-panel/internal/auth"
)

// 计量单位：Tabbit 只给用量百分比。
const (
	UnitPercent = "%"
	UnitCredit  = UnitPercent // 基线符号（面板额度页沿用）
	UnitUSD     = "USD"
)

// EntUsage 是额度快照（面板/调度器共用）。
type EntUsage struct {
	Remain  int64 `json:"remain"`
	Total   int64 `json:"total"`
	Expire  int64 `json:"expire,omitempty"`
	Daily   int64 `json:"daily,omitempty"`
	Monthly int64 `json:"monthly,omitempty"`
	Reward  int64 `json:"reward,omitempty"`
	Topup   int64 `json:"topup,omitempty"`
	// Tabbit 没有美元口径，恒 0 / 空。
	Unit     string  `json:"unit,omitempty"`
	UsedUSD  float64 `json:"used_usd,omitempty"`
	LimitUSD float64 `json:"limit_usd,omitempty"`
	// UsedPercent 是上游 usage_percentage 原值（0-100）。
	UsedPercent float64 `json:"used_percent,omitempty"`
	MemberLevel string  `json:"member_level,omitempty"`
}

// ExpireAt 返回到期时刻（毫秒）。
func (u EntUsage) ExpireAt() int64 { return u.Expire }

// RemainUSD 是面板的美元口径占位：Tabbit 只有百分比，恒 0。
func (u EntUsage) RemainUSD() float64 { return 0 }

// CreditPackage 是面板额度页的打包形状。
type CreditPackage struct {
	Name    string `json:"name"`
	Credits int64  `json:"credits"`
	Unit    string `json:"unit"`
}

// FetchQuota 返回原始额度节点（面板排障用）。
func (c *Client) FetchQuota(a *auth.Auth) (map[string]any, error) {
	base := c.BaseFor(a.Realm())
	query := url.Values{}
	if uid := a.Snapshot().UID; uid != "" {
		query.Set("user_id", uid)
	}
	url := base + QuotaPath
	if encoded := query.Encode(); encoded != "" {
		url += "?" + encoded
	}
	req, err := c.signedRequest(http.MethodGet, url, "", "/", a, c.baseHeaders(a, "/"))
	if err != nil {
		return nil, err
	}
	status, body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	return payloadOf(status, body, "额度查询")
}

// FetchPlan 返回额度节点：Tabbit 没有独立订阅接口，member_level 就在额度响应里。
func (c *Client) FetchPlan(a *auth.Auth) (map[string]any, error) { return c.FetchQuota(a) }

// UserEntUsageDetail 查一次额度并归一成 EntUsage（Remain 是剩余百分比）。
func (c *Client) UserEntUsageDetail(a *auth.Auth) (EntUsage, error) {
	data, err := c.FetchQuota(a)
	if err != nil {
		return EntUsage{}, err
	}
	// 上游可能把额度包一层 data。
	if inner, ok := data["data"].(map[string]any); ok {
		for key, value := range inner {
			if _, exists := data[key]; !exists {
				data[key] = value
			}
		}
	}
	used, ok := percentOf(data["usage_percentage"])
	if !ok {
		used, ok = percentOf(data["usage"])
	}
	if !ok {
		return EntUsage{}, &Error{Kind: ErrServer, Status: 502,
			Msg: "Tabbit 额度响应里没有 usage_percentage"}
	}
	remain := int64(100 - used)
	if remain < 0 {
		remain = 0
	}
	usage := EntUsage{
		Unit:        UnitPercent,
		Remain:      remain,
		Total:       100,
		UsedPercent: used,
		MemberLevel: text(data, "member_level", "plan", "plan_name"),
	}
	a.SetQuota(used)
	return usage, nil
}

// UserEntUsage 是老接口形状（remain,total,expire）。
func (c *Client) UserEntUsage(a *auth.Auth) (remain, total, expire int64, err error) {
	u, err := c.UserEntUsageDetail(a)
	return u.Remain, u.Total, u.Expire, err
}

// BalancePackages 返回面板额度页需要的明细。
func (c *Client) BalancePackages(a *auth.Auth) ([]CreditPackage, EntUsage, error) {
	usage, err := c.UserEntUsageDetail(a)
	if err != nil {
		return nil, EntUsage{}, err
	}
	packs := []CreditPackage{
		{Name: "剩余额度", Credits: usage.Remain, Unit: UnitPercent},
		{Name: "已用额度", Credits: int64(usage.UsedPercent + 0.5), Unit: UnitPercent},
	}
	return packs, usage, nil
}

// CreditPackages 是 BalancePackages 的老签名。
func (c *Client) CreditPackages(a *auth.Auth) ([]CreditPackage, int64, int64, error) {
	packs, usage, err := c.BalancePackages(a)
	return packs, usage.Remain, usage.Expire, err
}

// CheckinStatus 是基线的「签到状态」接口：Tabbit 没有签到，enable=false 让调度器
// 如实跳过整轮（而不是每轮空打一次上游然后报「复核未通过」）。
func (c *Client) CheckinStatus(*auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
	return false, 0, false, nil
}

// CheckinClaim 在 Tabbit 上等价于「重新查一次额度」：没有可领取的东西，不伪造收益。
func (c *Client) CheckinClaim(a *auth.Auth) error {
	_, err := c.UserEntUsageDetail(a)
	return err
}

// PrepareAccount 把面板粘贴的内容变成账号：账号 JSON、cookie 串或裸 token。
//
// payload 来自面板的导入端点：
//
//	{"token":"eyJ…","user_id":"…","realm":"intl"}
//	{"cookie":"token=…; user_id=…"}
func (c *Client) PrepareAccount(_ context.Context, payload map[string]any, realm string) (*auth.Auth, error) {
	raw := text(payload, "token", "cookie", "access_token", "text")
	if raw == "" {
		return nil, &Error{Kind: ErrClient, Status: 400,
			Msg: "无法识别的内容：请粘贴 Tabbit 账号 JSON、cookie 串（token=…; user_id=…）或裸 token"}
	}
	if cookie := text(payload, "cookie"); cookie != "" {
		raw = cookie
	}
	body := raw
	if !strings.HasPrefix(strings.TrimSpace(body), "{") {
		// 裸 token / cookie 串交给 auth.Parse 的自由格式分支。
		account, err := auth.Parse([]byte(body))
		if err != nil {
			return nil, &Error{Kind: ErrClient, Status: 400, Msg: err.Error()}
		}
		if realm != "" {
			account.Data.Realm = normalizeRealm(realm)
		}
		return c.verifyAccount(account)
	}
	account, err := auth.Parse([]byte(body))
	if err != nil {
		return nil, &Error{Kind: ErrClient, Status: 400, Msg: err.Error()}
	}
	if realm != "" {
		account.Data.Realm = normalizeRealm(realm)
	}
	return c.verifyAccount(account)
}

// verifyAccount 用 base-info 验证 cookie 可用，并补齐昵称。
func (c *Client) verifyAccount(account *auth.Auth) (*auth.Auth, error) {
	uid, nickname, _, err := c.GetUserInfo(account)
	if err != nil {
		return nil, err
	}
	if uid != "" {
		account.UID = uid
	}
	if nickname != "" {
		account.Nickname = nickname
	}
	if account.Nickname == "" {
		account.Nickname = account.UID
	}
	if err := account.CompleteIdentity(""); err != nil {
		return nil, &Error{Kind: ErrClient, Status: 400, Msg: err.Error()}
	}
	return account, nil
}

// normalizeRealm 归一化面板传来的地区名。
func normalizeRealm(realm string) string {
	if strings.EqualFold(strings.TrimSpace(realm), auth.RealmCN) {
		return auth.RealmCN
	}
	return auth.RealmIntl
}

// DeviceLogin Tabbit 没有设备码流程：面板「添加账号」点它会拿到这条提示，
// 让人改用「导入账号」粘贴 cookie。
type DeviceLogin struct {
	url   string
	realm string
}

// ErrDeviceLoginUnsupported 说明 Tabbit 不走设备码授权。
var ErrDeviceLoginUnsupported = errors.New(
	"Tabbit 没有设备码授权：请在 Tabbit 浏览器里登录后按 F12 → Application → Cookies → https://web.tabbit.ai，" +
		"复制 token 与 user_id（或整串 cookie）粘贴到「导入账号」")

// NewDeviceLogin 固定返回不支持（保留基线调用点：面板 login.go 原样复用）。
func NewDeviceLogin(client *Client, realm, _ string) (*DeviceLogin, error) {
	if client == nil {
		return nil, ErrDeviceLoginUnsupported
	}
	return &DeviceLogin{url: client.BaseFor(realm), realm: normalizeRealm(realm)}, ErrDeviceLoginUnsupported
}

// State 返回会话标识（基线面板拿它当 id）。
func (d *DeviceLogin) State() string { return "tabbit-manual" }

// Realm 返回地区。
func (d *DeviceLogin) Realm() string { return d.realm }

// AuthURL 返回登录页地址，供人工打开。
func (d *DeviceLogin) AuthURL() string { return d.url }

// Poll 不轮询：Tabbit 靠人工粘贴 cookie。
func (d *DeviceLogin) Poll(context.Context) (*auth.Auth, bool, error) {
	return nil, false, ErrDeviceLoginUnsupported
}

// ParseCallbackCode 在 Tabbit 没有回调链接，保留符号让基线调用点编译通过。
func ParseCallbackCode(string) (code, state string, ok bool) { return "", "", false }

// AuthorizeURL 返回对应站点的登录页（面板展示给人手动打开用）。
func (c *Client) AuthorizeURL(realm string) string {
	return c.BaseFor(realm) + "/login"
}

// percentOf 把 "12.5" / "12.5%" / 12.5 归一成 0-100 的百分比。
func percentOf(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	case int:
		return float64(v), true
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f, true
		}
	case string:
		trimmed := strings.TrimSuffix(strings.TrimSpace(v), "%")
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// int64Of 把数字/数字字符串归一成 int64。
func int64Of(value any) (int64, bool) {
	switch v := value.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, true
		}
		if f, err := v.Float64(); err == nil {
			return int64(f), true
		}
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%g", &f); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}

// floatOf 把数字/数字字符串归一成 float64。
func floatOf(value any) (float64, bool) {
	switch v := value.(type) {
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f, true
		}
	case float64:
		return v, true
	case int64:
		return float64(v), true
	case int:
		return float64(v), true
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%g", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

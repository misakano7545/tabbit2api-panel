// account.go 账号侧口径：额度快照、签到、登录态探活、凭证准备。
//
// Tabbit 的额度接口是 GET /api/commerce/quota/v1/usage?user_id=…&timezone=…
// （只按 user_id 查，不带 cookie 也能读；缺 user_id 直接 422），返回
// usage_percentage（已用百分比）与 member_level（free / standard / pro）。
// 签到是真实存在的运营功能：/api/commerce/activity/v1/sign-in{/status}。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/misakano7545/tabbit2api-panel/internal/auth"
)

// 计量单位：Tabbit 的额度就是**百分比**（上游只给 usage_percentage，没有积分/金额）。
const (
	UnitPercent = "%"
	UnitCredit  = UnitPercent // 基线符号（面板额度页沿用）
	UnitUSD     = "USD"       // 历史遗留：Tabbit 不用
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
	// ResetAt 是下一次额度重置时刻（Unix 秒）；0 = 上游没给。
	// Tabbit 是百分比制：Remain/UsedPercent 就是全部信息，ResetAt 只用于展示倒计时。
	ResetAt int64 `json:"reset_at,omitempty"`
	// ResetCoupons 是「额度重置券」张数（上游 unused_reset_coupon_count）。
	ResetCoupons int64 `json:"reset_coupons,omitempty"`
	// CycleStart / CycleEnd 是上游原始周期字符串（形如 2026.10.21 / 2026.10.28），
	// 面板直接展示「本周期 10.21–10.28」，不做时区换算。
	CycleStart string `json:"cycle_start,omitempty"`
	CycleEnd   string `json:"cycle_end,omitempty"`
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
	query.Set("timezone", timezone())
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

// timezone 返回额度接口要的时区名（上游按它算「今天」与重置时刻）。
func timezone() string {
	name := time.Local.String()
	if name == "" || strings.EqualFold(name, "Local") {
		return "Asia/Shanghai"
	}
	return name
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
	// 重置倒计时：上游给 remaining_reset_minutes（分钟）；个别响应只给小时。
	// 这里换算成**绝对时刻**存下来，前端据此实时倒计时——存「还剩多少分钟」会在两次
	// 刷新之间越走越慢（面板上显示的倒计时会停住），绝对时刻不会。
	if minutes, ok := int64Of(data["remaining_reset_minutes"]); ok && minutes > 0 {
		usage.ResetAt = c.now().Add(time.Duration(minutes) * time.Minute).Unix()
	} else if hours, ok := int64Of(data["remaining_reset_hours"]); ok && hours > 0 {
		usage.ResetAt = c.now().Add(time.Duration(hours) * time.Hour).Unix()
	}
	// cycle_end 是「本次额度周期结束日」（形如 2026.10.28），没有 remaining_* 时当兜底：
	// 上游的周期是按日切的，缺时刻就按当天 00:00 估，至少不会显示「无重置时间」。
	if usage.ResetAt == 0 {
		if end := text(data, "current_cycle_end", "currentCycleEnd"); end != "" {
			if t, err := time.ParseInLocation("2006.1.2", end, time.Local); err == nil {
				usage.ResetAt = t.AddDate(0, 0, 1).Unix()
			}
		}
	}
	usage.CycleStart = text(data, "current_cycle_start", "currentCycleStart")
	usage.CycleEnd = text(data, "current_cycle_end", "currentCycleEnd")
	if coupons, ok := int64Of(data["unused_reset_coupon_count"]); ok {
		usage.ResetCoupons = coupons
	}
	a.SetQuota(used)
	return usage, nil
}

// UserEntUsage 是老接口形状（remain,total,expire）。
func (c *Client) UserEntUsage(a *auth.Auth) (remain, total, expire int64, err error) {
	u, err := c.UserEntUsageDetail(a)
	return u.Remain, u.Total, u.Expire, err
}

// BalancePackages 返回额度构成。
//
// Tabbit 的额度是**百分比制**（usage_percentage），没有积分包/金额包这个概念，
// 所以这里返回空列表 + 归一好的 EntUsage，由面板直接画「剩余/已用」而不是编出
// 两个假包（曾经的「剩余额度/已用额度」两个包就是这么来的，读起来像积分）。
func (c *Client) BalancePackages(a *auth.Auth) ([]CreditPackage, EntUsage, error) {
	usage, err := c.UserEntUsageDetail(a)
	if err != nil {
		return nil, EntUsage{}, err
	}
	return nil, usage, nil
}

// CreditPackages 是 BalancePackages 的老签名。
func (c *Client) CreditPackages(a *auth.Auth) ([]CreditPackage, int64, int64, error) {
	packs, usage, err := c.BalancePackages(a)
	return packs, usage.Remain, usage.Expire, err
}

// CheckinStatus 查签到状态：GET /api/commerce/activity/v1/sign-in/status。
//
// 上游按 scene_code 分别记：daily_sign_in（每日签到）与 desktop_pet（养宠）。
// 网关只关心 daily_sign_in：activity_open=false 时返回 enable=false，让调度器
// 如实跳过整轮，而不是每轮空打一次上游。
func (c *Client) CheckinStatus(a *auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
	data, err := c.signInStatus(a)
	if err != nil {
		return false, 0, true, err
	}
	results, _ := data["results"].([]any)
	for _, raw := range results {
		entry, _ := raw.(map[string]any)
		if entry == nil || text(entry, "scene_code") != "daily_sign_in" {
			continue
		}
		return text(entry, "signed_today") == "true" || boolOf(entry["signed_today"]),
			0, boolOf(entry["activity_open"]), nil
	}
	return false, 0, true, nil
}

// CheckinClaim 真正签一次：POST /api/commerce/activity/v1/sign-in。
//
// body（实测）: {"request_no":"<uuid>","scene_codes":["daily_sign_in"],"usage_percentage":"0.00%"}
// 返回里的 usage_reward_result 是奖励口径（"skipped" = 没中奖），不是额度，
// 所以这里只报告「签没签成功」，不编造收益。
func (c *Client) CheckinClaim(a *auth.Auth) error {
	percent := "0.00%"
	if used, err := c.UserEntUsageDetail(a); err == nil {
		percent = fmt.Sprintf("%.2f%%", used.UsedPercent)
	}
	body, err := json.Marshal(map[string]any{
		"request_no":       randomUUID(),
		"scene_codes":      []string{"daily_sign_in"},
		"usage_percentage": percent,
	})
	if err != nil {
		return &Error{Kind: ErrServer, Status: 500, Msg: "构造签到请求失败"}
	}
	data, err := c.postJSON(a, SignInPath, string(body), "/member/usage", "签到")
	if err != nil {
		return err
	}
	results, _ := data["results"].([]any)
	for _, raw := range results {
		entry, _ := raw.(map[string]any)
		if entry == nil || text(entry, "scene_code") != "daily_sign_in" {
			continue
		}
		switch text(entry, "sign_in_result") {
		case "success", "already_signed":
			return nil
		case "":
			return nil // 上游没给结论：不谎报成功也不判失败
		default:
			return &Error{Kind: ErrClient, Status: 400,
				Msg: fmt.Sprintf("签到未成功：%s", text(entry, "sign_in_result"))}
		}
	}
	return nil
}

// signInStatus 拉一次签到状态（面板只读展示也走这里）。
func (c *Client) signInStatus(a *auth.Auth) (map[string]any, error) {
	base := c.BaseFor(a.Realm())
	query := url.Values{}
	for _, scene := range []string{"daily_sign_in", "desktop_pet"} {
		query.Add("scene_codes", scene)
	}
	target := base + SignInStatusPath + "?" + query.Encode()
	req, err := c.signedRequest(http.MethodGet, target, "", "/member/usage", a, c.baseHeaders(a, "/member/usage"))
	if err != nil {
		return nil, err
	}
	status, body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	return payloadOf(status, body, "签到状态查询")
}

// postJSON 是「带 cookie 的 JSON POST + 归一到 map」的公用路径。
func (c *Client) postJSON(a *auth.Auth, path, bodyRaw, referer, action string) (map[string]any, error) {
	base := c.BaseFor(a.Realm())
	req, err := c.signedRequest(http.MethodPost, base+path, bodyRaw, referer, a, c.baseHeaders(a, referer))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	status, body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	return payloadOf(status, body, action)
}

// boolOf 把 true / "true" / 1 归一成 bool。
func boolOf(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	case json.Number:
		n, _ := v.Int64()
		return n != 0
	case float64:
		return v != 0
	}
	return false
}

// extractCookie 从「随手复制的一段东西」里挖出 cookie 串。
//
// 用户手里最常有的是这几种，逐个别名匹配比要求他们改格式友好得多：
//
//	curl 'https://web.tabbit.ai/...' -H 'Cookie: token=...; user_id=...'   （DevTools → Copy as cURL）
//	Cookie: token=...; user_id=...                                        （Copy as cURL 的单行）
//	-b 'token=...; user_id=...'
//	token=...; user_id=...                                                （直接粘 cookie）
//
// 找不到就原样返回，让后面的 Parse 去判断（裸 JWT 走那条路）。
func extractCookie(raw string) string {
	text := strings.TrimSpace(raw)
	lower := strings.ToLower(text)
	if strings.Contains(lower, "cookie:") {
		if idx := strings.Index(lower, "cookie:"); idx >= 0 {
			rest := text[idx+len("cookie:"):]
			// cookie 串到引号/换行/回车为止
			if end := strings.IndexAny(rest, "\"'\r\n"); end >= 0 {
				rest = rest[:end]
			}
			if v := strings.TrimSpace(rest); strings.Contains(v, "=") {
				return v
			}
		}
	}
	if idx := strings.Index(lower, " -b "); idx >= 0 || strings.HasPrefix(lower, "-b ") {
		if idx < 0 {
			idx = 0
		}
		rest := strings.TrimSpace(strings.TrimPrefix(text[idx+3:], "-b "))
		// 参数可能带引号：先吃掉开引号，再截到闭引号/换行。
		if rest != "" && (rest[0] == '\'' || rest[0] == '"') {
			quote := rest[0]
			rest = rest[1:]
			if end := strings.IndexByte(rest, quote); end >= 0 {
				rest = rest[:end]
			}
		} else if end := strings.IndexAny(rest, " \t\r\n"); end >= 0 {
			rest = rest[:end]
		}
		if v := strings.TrimSpace(rest); strings.Contains(v, "=") {
			return v
		}
	}
	return text
}

// PrepareAccount 把面板粘贴的内容变成账号：账号 JSON、cookie 串或裸 token。
//
// payload 来自面板的导入端点：{"token":"eyJ…","user_id":"…","realm":"intl"} 或
// {"cookie":"token=…; user_id=…"}。
//
// realm 为空时按账号记录里的 realm/region 字段，再为空就**自动识别**：
// 先按国际站验一次，401/403 说明这个号不属于该站，再换国内站验。
// 两站的 token 不通用（国内 token 打国际站只会 401），所以自动识别是安全的：
// 验不过的那个站不会被误判成「登录态失效」。
func (c *Client) PrepareAccount(_ context.Context, payload map[string]any, realm string) (*auth.Auth, error) {
	raw := text(payload, "token", "cookie", "access_token", "text")
	if raw == "" {
		return nil, &Error{Kind: ErrClient, Status: 400,
			Msg: "无法识别的内容：请粘贴 Tabbit 账号 JSON、cookie 串（token=…; user_id=…）或裸 token"}
	}
	if cookie := text(payload, "cookie"); cookie != "" {
		raw = cookie
	}
	// 允许直接粘 cURL / 请求头块 / 整段 DevTools 输出：从中挖出 cookie。
	raw = extractCookie(raw)
	if realm == "" {
		realm = text(payload, "realm", "region")
	}
	parse := func() (*auth.Auth, error) {
		account, err := auth.Parse([]byte(raw))
		if err != nil {
			return nil, &Error{Kind: ErrClient, Status: 400, Msg: err.Error()}
		}
		return account, nil
	}
	if realm != "" {
		account, err := parse()
		if err != nil {
			return nil, err
		}
		account.Data.Realm = normalizeRealm(realm)
		return c.verifyAccount(account)
	}
	// 自动识别：国际 → 国内。只有「登录态失效」才换站重试，网络/服务错误照实报。
	intlAccount, err := parse()
	if err != nil {
		return nil, err
	}
	intlAccount.Data.Realm = auth.RealmIntl
	verified, err := c.verifyAccount(intlAccount)
	if err == nil {
		return verified, nil
	}
	if !isSessionDead(err) {
		return nil, err
	}
	cnAccount, err2 := parse()
	if err2 != nil {
		return nil, err2
	}
	cnAccount.Data.Realm = auth.RealmCN
	if verifiedCN, errCN := c.verifyAccount(cnAccount); errCN == nil {
		return verifiedCN, nil
	}
	return nil, err // 两个站都不认：回第一个错误（更可能是用户真正要用的那个站）
}

// isSessionDead 判断错误是不是「登录态不属于这个站/已失效」。
func isSessionDead(err error) bool {
	if e, ok := err.(*Error); ok {
		return e.Kind == ErrSessionDead
	}
	return false
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

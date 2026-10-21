// client.go Tabbit 上游：签名、会话、SSE 对话。
//
// 上游是 Tabbit 浏览器的 Web 后端（web.tabbit.ai 国际版 / web.tabbit.com 国内版），
// 认证靠 cookie `token`（HttpOnly JWT），写请求要带一组签名头：
//
//	x-timestamp  Date.now() 毫秒
//	x-signature  随机 UUID v4（名字反直觉，按线上行为为准）
//	x-nonce      HMAC-SHA256(signKey, "{ts}.{uuid}.{sha256hex(body)}") 的 hex
//
// signKey 先取 GET /chat/sign-key，服务端可轮换；请求返回 499 时刷新 key 重试一次。
// 另外两个必带头：x-req-ctx（base64 的客户端版本号）与 unique-uuid（客户端自证标记）。
//
// 上游对话是「一段纯文本进、SSE 文本流出」，没有 messages 数组也没有原生工具调用：
// 本包把 OpenAI 请求摊成一段 prompt（prompt.go），把工具声明编译成文本协议
// （toolcall.go），再把上游事件翻译回 OpenAI 的 chunk（convert.go）。下游三套协议
// （chat / Responses / Messages）因此只认一种线上形状。
package upstream

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/misakano7545/tabbit2api-panel/internal/auth"
)

// 上游地址：两个站点各一套目录/账号，账号归哪个站由 auth 决定。
const (
	BaseIntl = "https://web.tabbit.ai"
	BaseCN   = "https://web.tabbit.com"

	// 基线 config 的四个基址键在 Tabbit 只有一个站的概念，全部指国际版；
	// 国内版走 SetHosts 单独覆盖。
	LLMBase    = BaseIntl
	AuthBase   = BaseIntl
	PointsBase = BaseIntl
	Origin     = BaseIntl

	SignKeyPath = "/chat/sign-key"
	CatalogPath = "/proxy/v1/model_config/models"
	SessionPath = "/panel/session"
	// 对话走 v3 房间协议：先 join 订阅 SSE，再向 runs 投递消息（2026-10 实测，
	// 旧的 POST /api/v1/chat/completion 已不在 Web UI 里再用）。
	JoinPath     = "/api/v3/chat/rooms/%s/join"
	RunsPath     = "/api/v3/chat/rooms/%s/runs"
	ChatPath     = RunsPath // 兼容旧引用
	UserInfoPath = "/api/v0/user/base-info"
	QuotaPath    = "/api/commerce/quota/v1/usage"

	SignInStatusPath = "/api/commerce/activity/v1/sign-in/status"
	SignInPath       = "/api/commerce/activity/v1/sign-in"

	// FallbackSignKey 是前端硬编码的初始 key（国际版）：/chat/sign-key 拿不到时先用它。
	FallbackSignKey = "f8d0e6a73f8d4b1a9c3d2e1f9a4b7c6d"
	// FallbackSignKeyCN 是国内版的初始 key：两站的 key 不同，抓包实测确认
	// （web.tabbit.com/chat/sign-key → 3a9b1c8d…，用它签 intl 请求会被判签名错）。
	FallbackSignKeyCN = "3a9b1c8d5e7f4b209c2d1f4a6e7b8c9f"
	// DefaultReqCtx 是 base64("1.17.16(10117016)")：上游按 {version}({build}) 解析，
	// 格式不对会判 493「版本过低」。版本号跟随桌面端 1.17.16。
	DefaultReqCtx = "MS4xNy4xNigxMDExNzAxNik="
	// ClientID 是上游前端固定的 Chrome 集成 client_id。
	ClientID = "2dd8eb4c1ed9c344d173"

	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
	SecChUA = `"Tabbit Browser";v="149", "Chromium";v="149", "Not)A;Brand";v="24"`

	// DefaultConfigName 是请求没带 model 时的兜底：目录里第一个免费模型。
	DefaultConfigName     = "default"
	DefaultConfigNameIntl = DefaultConfigName

	signKeyTTL = 30 * time.Minute
	sessionTTL = 30 * time.Minute
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind int

const (
	ErrNone          ErrKind = iota // 与账号无关（模型/参数/业务码）
	ErrPlanLimit                    // 额度不足/需升级 Pro（492）→ 计划硬冷却
	ErrSoftRate                     // 限流（429）→ 软冷却
	ErrSessionDead                  // 登录态失效（401/403）→ 连续达阈值才禁用
	ErrNotFound                     // 404 → 短冷却
	ErrServer                       // 5xx → 熔断计数
	ErrClient                       // 其余 4xx → 降权（本仓按「不罚号」处理）
	ErrBusy                         // 会话处理中（409）→ 换号不罚号
	ErrReasoningLoop                // 思考链复读（模型侧问题）→ 中断且不罚号
	ErrGateway                      // 502/504 网关超时 → 不罚号、不换号
)

// String 供日志与面板展示。
func (k ErrKind) String() string {
	switch k {
	case ErrPlanLimit:
		return "plan_limit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrBusy:
		return "busy"
	case ErrReasoningLoop:
		return "reasoning_loop"
	case ErrGateway:
		return "gateway"
	default:
		return "none"
	}
}

// Error 带分类的上游错误。
type Error struct {
	Kind           ErrKind
	Status         int
	Msg            string
	Busy           bool
	RetryAfterSecs int64
	// ServiceUnavailable = 上游明说该模型此刻不可服务：等也没用，立刻回错。
	ServiceUnavailable bool
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return e.Msg
	}
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// BusinessError 是 HTTP 成功但上游业务拒绝（code != 0）。
type BusinessError struct {
	Code    int
	Message string
}

func (e *BusinessError) Error() string { return e.Message }

// ErrEmptyStream 上游一个字都没吐（可换号重试，但不罚号）。
var ErrEmptyStream = fmt.Errorf("上游返回空流")

// ErrCheckinAlready Tabbit 没有签到：保留符号只为基线调用点编译通过。
var ErrCheckinAlready = fmt.Errorf("今日已领取")

// ErrCodeExpired Tabbit 没有授权码流程（保留符号）。
var ErrCodeExpired = fmt.Errorf("授权码已失效，请重新发起登录")

// CodeExpired 是上游「授权码失效」的业务码（保留）。
const CodeExpired = 200035

// IsCheckinRateLimited Tabbit 没有签到限流。
func IsCheckinRateLimited(error) bool { return false }

// Classify 按 HTTP 状态与正文判定错误类别。
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrSessionDead
	case status == http.StatusConflict:
		return ErrBusy
	case status == http.StatusTooManyRequests:
		return ErrSoftRate
	case status == http.StatusBadGateway || status == http.StatusGatewayTimeout:
		return ErrGateway
	case status >= 500:
		return ErrServer
	case status == http.StatusNotFound:
		return ErrNotFound
	}
	if strings.Contains(lower, "session is processing") || strings.Contains(lower, "正在处理") || strings.Contains(lower, "处理中") {
		return ErrBusy
	}
	for _, signal := range []string{"upgrade", "premium", "insufficient", "quota", "额度", "升级", "权益"} {
		if strings.Contains(lower, signal) {
			return ErrPlanLimit
		}
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// Client 访问 Tabbit 上游。两个站的基址可由配置覆盖。
type Client struct {
	// HTTP 用于短 JSON 请求（签名 key/会话/目录/额度），有总超时兜底。
	HTTP *http.Client
	// StreamHTTP 用于 SSE 流式对话：不设总超时，只用 ResponseHeaderTimeout
	// 兜底「上游一直不返首字节」的悬挂。
	StreamHTTP *http.Client

	timeoutMu   sync.RWMutex
	idleTimeout time.Duration

	hostMu sync.RWMutex
	cn     string
	intl   string

	uaMu      sync.RWMutex
	userAgent string

	idMu           sync.RWMutex
	reqCtx         string
	defaultBrowser bool

	signMu sync.Mutex
	signs  map[string]*signKey // host → key

	catMu   sync.RWMutex
	catalog map[string]catalogEntry // realm → 目录

	sessMu   sync.Mutex
	sessions map[string]*sessionEntry // uid|realm → 会话

	// now 供测试夹具替换时间源（真实运行恒为 time.Now）。
	now func() time.Time
}

type signKey struct {
	key string
	at  time.Time
}

type sessionEntry struct {
	id     string
	at     time.Time
	usedAt time.Time
}

// New 生产默认值：连接池共享，短请求 120s 兜底、长流只看首字节超时。
//
// Proxy 必须显式给 ProxyFromEnvironment：自建 Transport 不带 Proxy 字段就等于
// 强制直连，HTTPS_PROXY 会被静默忽略——而 web.tabbit.ai 直连会拿到 CDN 的
// 403「Service Unavailable」（HTML，不是 JSON），面板上的表现就是「登录态失效」，
// 极难排查。抓包/翻墙场景都靠它。
func New() *Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr},
		cn:         BaseCN,
		intl:       BaseIntl,
		reqCtx:     DefaultReqCtx,
		signs:      map[string]*signKey{},
		catalog:    map[string]catalogEntry{},
		sessions:   map[string]*sessionEntry{},
		now:        time.Now,
	}
}

// SetTimeouts 热改超时：short 是短请求总超时，header 是首字节超时，idle 是流内空闲上限。
func (c *Client) SetTimeouts(short, header, idle time.Duration) {
	c.timeoutMu.Lock()
	defer c.timeoutMu.Unlock()
	if short > 0 && c.HTTP != nil {
		c.HTTP.Timeout = short
	}
	if header > 0 {
		for _, hc := range []*http.Client{c.HTTP, c.StreamHTTP} {
			if hc == nil {
				continue
			}
			if tr, ok := hc.Transport.(*http.Transport); ok {
				tr.ResponseHeaderTimeout = header
			}
		}
	}
	c.idleTimeout = idle
}

// SetIdentity 覆盖出站 User-Agent（空 = 用内置 UA）。
func (c *Client) SetIdentity(userAgent, _ string) {
	c.uaMu.Lock()
	c.userAgent = strings.TrimSpace(userAgent)
	c.uaMu.Unlock()
}

// SetHosts 覆盖两个站的基址（config.upstream.base_cn / base_intl）。
func (c *Client) SetHosts(cn, intl string) {
	c.hostMu.Lock()
	c.cn = orDefault(strings.TrimRight(strings.TrimSpace(cn), "/"), BaseCN)
	c.intl = orDefault(strings.TrimRight(strings.TrimSpace(intl), "/"), BaseIntl)
	c.hostMu.Unlock()
}

// SetAgentHosts 是基线签名的兼容入口：llmBase 当国际版，authBase 当国内版。
func (c *Client) SetAgentHosts(llmBase, authBase string) { c.SetHosts(authBase, llmBase) }

// AgentHosts 返回 (国际版, 国内版) 基址（面板展示「生效地址」用）。
func (c *Client) AgentHosts() (llm, authBase string) {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.intl, c.cn
}

// Origins 返回两个站基址（面板展示用）。
func (c *Client) Origins() (points, origin string) {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	return c.intl, c.cn
}

// SetRequestContext 覆盖 x-req-ctx 与 unique-uuid 的默认浏览器标记。
//
// marker=true 只在「本机真的把 Tabbit 设为默认浏览器」时开——那是上游认可的 Pro
// 自证路径；伪造等于冒充付费用户，本仓默认如实填 false。
func (c *Client) SetRequestContext(reqCtx string, defaultBrowser bool) {
	c.idMu.Lock()
	c.reqCtx = orDefault(strings.TrimSpace(reqCtx), DefaultReqCtx)
	c.defaultBrowser = defaultBrowser
	c.idMu.Unlock()
}

// BaseFor 返回某个地区（cn / intl）的站点基址。
func (c *Client) BaseFor(realm string) string {
	c.hostMu.RLock()
	defer c.hostMu.RUnlock()
	if realm == auth.RealmCN {
		return c.cn
	}
	return c.intl
}

func (c *Client) requestContext() (string, bool) {
	c.idMu.RLock()
	defer c.idMu.RUnlock()
	return c.reqCtx, c.defaultBrowser
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) streamClient() *http.Client {
	if c.StreamHTTP != nil {
		return c.StreamHTTP
	}
	return c.httpClient()
}

func (c *Client) currentUA() string {
	c.uaMu.RLock()
	defer c.uaMu.RUnlock()
	if c.userAgent != "" {
		return c.userAgent
	}
	return DefaultUserAgent
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// ---------------------------------------------------------------------------
// 请求构造

// cookies 是上游认的登录态：token 是 JWT，user_id/SAPISID 是同一个用户标识。
func cookies(a *auth.Auth) map[string]string {
	uid := a.Snapshot().UID
	return map[string]string{
		"token":       a.JWT(),
		"user_id":     uid,
		"managed":     "tab_browser",
		"NEXT_LOCALE": "zh",
		"SAPISID":     uid,
	}
}

// baseHeaders 是每个请求都带的浏览器侧身份头。
func (c *Client) baseHeaders(a *auth.Auth, refererPath string) map[string]string {
	base := c.BaseFor(a.Realm())
	uid := a.Snapshot().UID
	return map[string]string{
		"User-Agent":         c.currentUA(),
		"sec-ch-ua":          SecChUA,
		"sec-ch-ua-platform": `"Windows"`,
		"sec-ch-ua-mobile":   "?0",
		"x-chrome-id-consistency-request": fmt.Sprintf(
			"version=1,client_id=%s,device_id=%s,sync_account_id=%s,signin_mode=all_accounts,signout_mode=show_confirmation",
			ClientID, deviceID(uid), uid),
		"referer": base + refererPath,
		"Origin":  base,
	}
}

// deviceID 由 uid 派生稳定 UUID 形态（同一账号每次启动一致，避免上游把设备当新设备）。
func deviceID(seed string) string {
	sum := sha256.Sum256([]byte("tabbit-device:" + seed))
	raw := sum[:16]
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	h := hex.EncodeToString(raw)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// uniqueUUID 生成 `unique-uuid` 头：第 5 位是「默认浏览器」标记位，
// 位置 [2,7,11,14,18,21,25,28] 编码当前时间的 hex（防重放）。
func uniqueUUID(defaultBrowser bool, now time.Time) string {
	const hexDigits = "0123456789abcdef"
	ts := fmt.Sprintf("%08x", now.Unix())
	positions := map[int]byte{}
	for i, pos := range []int{2, 7, 11, 14, 18, 21, 25, 28} {
		positions[pos] = ts[i]
	}
	var raw [32]byte
	_, _ = rand.Read(raw[:])
	out := make([]byte, 32)
	for i := 0; i < 32; i++ {
		switch {
		case i == 5:
			if defaultBrowser {
				out[i] = '1'
			} else {
				// 非默认浏览器：从去掉 '1' 的 hex 集里取，标记位天然不等于 "1"。
				out[i] = []byte("023456789abcdef")[raw[i]%15]
			}
		default:
			if v, ok := positions[i]; ok {
				out[i] = v
			} else {
				out[i] = hexDigits[raw[i]%16]
			}
		}
	}
	s := string(out)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// signatureHeaders 计算一组签名头（x-timestamp / x-signature / x-nonce）。
func signatureHeaders(key, body string, now time.Time) map[string]string {
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	nonce := randomUUID()
	digest := sha256.Sum256([]byte(body))
	message := ts + "." + nonce + "." + hex.EncodeToString(digest[:])
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(message))
	return map[string]string{
		"x-timestamp": ts,
		"x-signature": nonce,
		"x-nonce":     hex.EncodeToString(mac.Sum(nil)),
	}
}

// md5Empty 是上游 entity.key 的固定值（前端对一个空 tab 取的 md5）。
func md5Empty() string {
	sum := md5.Sum(nil)
	return hex.EncodeToString(sum[:])
}

// do 发一次请求并读回响应体（上限 4MiB）。
func (c *Client) do(req *http.Request) (int, []byte, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.currentUA())
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return 0, nil, req.Context().Err()
		}
		return 0, nil, &Error{Kind: ErrServer, Status: 502, Msg: "无法连接 Tabbit，请检查网络或代理设置"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, raw, &Error{Kind: ErrServer, Status: 502, Msg: "读取 Tabbit 响应失败"}
	}
	if os.Getenv("TABBIT_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[upstream] %s %s -> %d %s\n", req.Method, req.URL,
			resp.StatusCode, truncate(string(raw), 200))
	}
	return resp.StatusCode, raw, nil
}

// signKey 返回当前站点的签名 key（过期或缺失时先刷新）。
func (c *Client) signKey(a *auth.Auth) (string, error) {
	base := c.BaseFor(a.Realm())
	c.signMu.Lock()
	entry := c.signs[base]
	fresh := entry != nil && c.now().Sub(entry.at) < signKeyTTL
	c.signMu.Unlock()
	if fresh {
		return entry.key, nil
	}
	if err := c.refreshSignKey(a); err != nil {
		if entry != nil {
			return entry.key, nil // 刷新失败沿用旧 key：可能还能用，别让一次网络抖动打断对话
		}
		return fallbackSignKey(a.Realm()), err
	}
	c.signMu.Lock()
	defer c.signMu.Unlock()
	if latest := c.signs[base]; latest != nil {
		return latest.key, nil
	}
	return fallbackSignKey(a.Realm()), nil
}

// fallbackSignKey 返回该站点的硬编码初始 key（两站不同，不能混用）。
func fallbackSignKey(realm string) string {
	if realm == auth.RealmCN {
		return FallbackSignKeyCN
	}
	return FallbackSignKey
}

// refreshSignKey 拉一次 GET /chat/sign-key。
func (c *Client) refreshSignKey(a *auth.Auth) error {
	base := c.BaseFor(a.Realm())
	req, err := http.NewRequest(http.MethodGet, base+SignKeyPath, nil)
	if err != nil {
		return err
	}
	for key, value := range c.baseHeaders(a, "/") {
		req.Header.Set(key, value)
	}
	req.Header.Set("Cookie", cookieHeader(cookies(a)))
	req.Header.Set("Accept", "text/plain")
	status, body, err := c.do(req)
	if err != nil {
		return err
	}
	key := strings.TrimSpace(string(body))
	if status != http.StatusOK || key == "" || strings.ContainsAny(key, "<> \n") {
		return &Error{Kind: Classify(status, string(body)), Status: status,
			Msg: fmt.Sprintf("获取签名 key 失败（HTTP %d）: %s", status, truncate(string(body), 160))}
	}
	c.signMu.Lock()
	c.signs[base] = &signKey{key: key, at: c.now()}
	c.signMu.Unlock()
	return nil
}

// signedRequest 构造一个带签名与 cookie 的请求；body 为空表示 GET。
func (c *Client) signedRequest(method, url, rawBody, refererPath string, a *auth.Auth, base map[string]string) (*http.Request, error) {
	var reader io.Reader
	if rawBody != "" || method == http.MethodPost {
		reader = bytes.NewReader([]byte(rawBody))
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, err
	}
	for key, value := range base {
		req.Header.Set(key, value)
	}
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("trace-id", randomUUID())
	// cookie 头手工拼：Tabbit 只认这几个固定项，用 cookie jar 反而要管域名。
	// Authorization: Bearer 一并带上——国内站的 commerce（签到）只认 Bearer，
	// 国际站两种都收；同一份 JWT，带上不会改变判定。
	req.Header.Set("Cookie", cookieHeader(cookies(a)))
	if jwt := a.JWT(); jwt != "" {
		req.Header.Set("Authorization", "Bearer "+jwt)
	}
	key, _ := c.signKey(a)
	for name, value := range signatureHeaders(key, rawBody, c.now()) {
		req.Header.Set(name, value)
	}
	return req, nil
}

func cookieHeader(values map[string]string) string {
	parts := make([]string, 0, len(values))
	for _, name := range []string{"token", "user_id", "managed", "NEXT_LOCALE", "SAPISID"} {
		if value := values[name]; value != "" {
			parts = append(parts, name+"="+value)
		}
	}
	return strings.Join(parts, "; ")
}

// ---------------------------------------------------------------------------
// 会话

// sessionID 复用同一账号的对话会话（上游按会话保存上下文与配额）。
func (c *Client) sessionID(ctx context.Context, a *auth.Auth) (string, error) {
	key := a.Snapshot().UID + "|" + a.Realm()
	c.sessMu.Lock()
	entry := c.sessions[key]
	if entry != nil && c.now().Sub(entry.usedAt) < sessionTTL {
		entry.usedAt = c.now()
		id := entry.id
		c.sessMu.Unlock()
		return id, nil
	}
	c.sessMu.Unlock()

	base := c.BaseFor(a.Realm())
	req, err := c.signedRequest(http.MethodPost, base+SessionPath, "", "/newtab", a,
		c.baseHeaders(a, "/newtab"))
	if err != nil {
		return "", err
	}
	req = req.WithContext(ctx)
	status, body, err := c.do(req)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", &Error{Kind: Classify(status, string(body)), Status: status,
			Msg: fmt.Sprintf("创建 Tabbit 会话失败（HTTP %d）: %s", status, truncate(errorText(body), 160))}
	}
	var payload struct {
		ChatSessionID string `json:"chat_session_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.ChatSessionID == "" {
		return "", &Error{Kind: ErrServer, Status: 502,
			Msg: fmt.Sprintf("创建 Tabbit 会话失败：响应缺少 chat_session_id: %s", truncate(string(body), 160))}
	}
	c.sessMu.Lock()
	c.sessions[key] = &sessionEntry{id: payload.ChatSessionID, at: c.now(), usedAt: c.now()}
	c.sessMu.Unlock()
	return payload.ChatSessionID, nil
}

// dropSession 丢弃缓存的会话（上游判会话忙或会话已失效时）。
func (c *Client) dropSession(a *auth.Auth) {
	key := a.Snapshot().UID + "|" + a.Realm()
	c.sessMu.Lock()
	delete(c.sessions, key)
	c.sessMu.Unlock()
}

// SessionCount 返回当前缓存的会话数（面板排障用）。
func (c *Client) SessionCount() int {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return len(c.sessions)
}

// ---------------------------------------------------------------------------
// 对话

// buildRunPayload 组装 v3 runs 请求体；content 是已摊平的 prompt，
// model 是目录里的 display_name（发 slug 会被服务端静默切模型）。
//
// 上游形状（实测）：selected_model 是数组，正文在 input_payload.content，
// 历史靠 parent_message_id 串，references/metadatas 是「当前网页」上下文（网关留空）。
func buildRunPayload(content, model string) (string, error) {
	runID := "tab_" + randomUUID()
	payload := map[string]any{
		"client_run_id":           runID,
		"client_message_id":       runID,
		"agent_plan_mode_enabled": false,
		"input_payload": map[string]any{
			"task_name":         "chat",
			"content":           content,
			"parent_message_id": nil,
			"selected_model":    []any{model},
			"references":        []any{},
			"metadatas":         map[string]any{"html_content": "<p>" + content + "</p>"},
			"page_info_list":    []any{},
			"is_mobile":         false,
			"agent_mode":        false,
			"entity": map[string]any{"key": md5Empty(),
				"extras": map[string]any{"type": "tab", "url": ""}},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// buildJoinPayload 是 join 的 body：只声明订阅者身份，不携带对话内容。
func buildJoinPayload(seed string) (string, error) {
	digest := sha256.Sum256([]byte("tabbit-page:" + seed))
	payload := map[string]any{
		"page_instance_id": randomUUID(),
		"device_id_hash":   hex.EncodeToString(digest[:]),
		"surface":          "panel",
		"last_event_id":    nil,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// ChatStream 把 OpenAI 请求体发给 Tabbit 并返回 OpenAI 形状的 SSE 流。
//
// body 是入站 chat.completions 请求（model 已由 handler 定成规范 id）：
// messages 摊成一段 prompt，tools 编译成文本协议，上游事件翻译回 chunk 帧。
// 非 2xx 时返回分类后的 *Error；HTTP 200 但首帧是 error 事件时也按 *Error 返回。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	return c.chatStream(context.Background(), a, body)
}

func (c *Client) chatStream(ctx context.Context, a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	req, err := parseChatRequest(body)
	if err != nil {
		return nil, 400, nil, &Error{Kind: ErrClient, Status: 400, Msg: err.Error()}
	}
	model, err := c.selectedModel(a, req.Model)
	if err != nil {
		return nil, 400, nil, err
	}
	prompt, err := buildPrompt(req)
	if err != nil {
		return nil, 400, nil, &Error{Kind: ErrClient, Status: 400, Msg: err.Error()}
	}

	sessionID, err := c.sessionID(ctx, a)
	if err != nil {
		return nil, 502, nil, err
	}
	return c.runChat(ctx, a, sessionID, prompt, model, req)
}

// runChat 走 v3 房间协议：先 join 订阅该房间的 SSE，再向 runs 投递消息，
// 然后把 join 的事件流交给转换层（按 run_id 过滤，别的并发 run 的帧一律丢弃）。
//
// 顺序不能反：join 晚于 runs 时会漏掉 run_queued/首批 chunk（实测 join 只在
// 订阅之后的帧上推送，历史要 merge message_history 快照，代价更大）。
func (c *Client) runChat(ctx context.Context, a *auth.Auth, room, prompt, model string, spec chatRequestSpec) (io.ReadCloser, int, []byte, error) {
	joinBody, err := buildJoinPayload(a.Snapshot().UID + "|" + room)
	if err != nil {
		return nil, 500, nil, &Error{Kind: ErrServer, Status: 500, Msg: "构造 Tabbit 订阅请求失败"}
	}
	subscribe, status, raw, err := c.openJoin(ctx, a, room, joinBody)
	if err != nil {
		return nil, status, raw, err
	}

	runRaw, err := buildRunPayload(prompt, model)
	if err != nil {
		_ = subscribe.Body.Close()
		return nil, 500, nil, &Error{Kind: ErrServer, Status: 500, Msg: "构造 Tabbit 请求体失败"}
	}
	runID, status, raw, err := c.postRun(ctx, a, room, runRaw)
	if err != nil {
		_ = subscribe.Body.Close()
		return nil, status, raw, err
	}

	conv := newConvertReader(subscribe.Body, spec)
	conv.runID = runID
	// 先窥一帧：上游可能用 200 + run_failed/error 事件表达权益不足或排队拒绝，
	// 只看状态码会把它记成成功、账号也就没法换。
	first, err := conv.peek()
	if err != nil {
		_ = subscribe.Body.Close()
		return nil, 502, nil, &Error{Kind: ErrServer, Status: 502, Msg: "读取 Tabbit 首帧失败"}
	}
	if first != nil {
		_ = subscribe.Body.Close()
		return nil, first.Status, []byte(first.Msg), &Error{Kind: first.Kind, Status: first.Status,
			Msg: first.Msg, Busy: first.Kind == ErrBusy}
	}
	return conv, 200, nil, nil
}

// openJoin 订阅房间事件流（唯一带签名的对话请求）。
func (c *Client) openJoin(ctx context.Context, a *auth.Auth, room, bodyRaw string) (*http.Response, int, []byte, error) {
	base := c.BaseFor(a.Realm())
	target := base + fmt.Sprintf(JoinPath, url.PathEscape(room))
	for attempt := 0; attempt < 2; attempt++ {
		headers := c.baseHeaders(a, "/panel/"+room)
		headers["Accept"] = "text/event-stream"
		headers["Content-Type"] = "application/json"
		headers["Cache-Control"] = "no-cache"
		reqCtx, defaultBrowser := c.requestContext()
		headers["x-req-ctx"] = reqCtx
		headers["unique-uuid"] = uniqueUUID(defaultBrowser, c.now())
		headers["trace-id"] = randomUUID()
		headers["Cookie"] = cookieHeader(cookies(a))
		key, _ := c.signKey(a)
		for name, value := range signatureHeaders(key, bodyRaw, c.now()) {
			headers[name] = value
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(bodyRaw))
		if err != nil {
			return nil, 500, nil, &Error{Kind: ErrServer, Status: 500, Msg: "构造 Tabbit 上游请求失败"}
		}
		for name, value := range headers {
			httpReq.Header.Set(name, value)
		}
		resp, err := c.streamClient().Do(httpReq)
		if err != nil {
			if ctx.Err() != nil {
				return nil, 0, nil, ctx.Err()
			}
			return nil, 502, nil, &Error{Kind: ErrServer, Status: 502, Msg: "无法连接 Tabbit，请检查网络或代理设置"}
		}
		if resp.StatusCode == 499 && attempt == 0 {
			_ = resp.Body.Close()
			_ = c.refreshSignKey(a)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusConflict {
				c.dropSession(a)
			}
			kind := Classify(resp.StatusCode, string(raw))
			msg := truncate(errorText(raw), 300)
			if msg == "" {
				msg = fmt.Sprintf("Tabbit 订阅房间失败（HTTP %d）", resp.StatusCode)
			}
			return nil, resp.StatusCode, raw, &Error{Kind: kind, Status: resp.StatusCode, Msg: msg,
				Busy: kind == ErrBusy, ServiceUnavailable: queueUnavailable(string(raw))}
		}
		return resp, resp.StatusCode, nil, nil
	}
	return nil, 502, nil, &Error{Kind: ErrServer, Status: 502, Msg: "Tabbit 签名 key 刷新后仍返回 499"}
}

// postRun 投递一次对话（实测不带签名头），返回 run_id 供事件过滤。
func (c *Client) postRun(ctx context.Context, a *auth.Auth, room, bodyRaw string) (string, int, []byte, error) {
	base := c.BaseFor(a.Realm())
	target := base + fmt.Sprintf(RunsPath, url.PathEscape(room))
	headers := c.baseHeaders(a, "/panel/"+room)
	headers["Accept"] = "application/json"
	headers["Content-Type"] = "application/json"
	reqCtx, defaultBrowser := c.requestContext()
	headers["x-req-ctx"] = reqCtx
	headers["unique-uuid"] = uniqueUUID(defaultBrowser, c.now())
	headers["trace-id"] = randomUUID()
	headers["Cookie"] = cookieHeader(cookies(a))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(bodyRaw))
	if err != nil {
		return "", 500, nil, &Error{Kind: ErrServer, Status: 500, Msg: "构造 Tabbit 上游请求失败"}
	}
	for name, value := range headers {
		httpReq.Header.Set(name, value)
	}
	status, raw, err := c.do(httpReq)
	if err != nil {
		return "", status, raw, err
	}
	if status < 200 || status >= 300 {
		if status == http.StatusConflict {
			c.dropSession(a) // 房间被占用：下次换新房间
		}
		kind := Classify(status, string(raw))
		msg := truncate(errorText(raw), 300)
		if msg == "" {
			msg = fmt.Sprintf("Tabbit 投递消息失败（HTTP %d）", status)
		}
		return "", status, raw, &Error{Kind: kind, Status: status, Msg: msg,
			Busy: kind == ErrBusy, ServiceUnavailable: queueUnavailable(string(raw))}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", 502, raw, &Error{Kind: ErrServer, Status: 502, Msg: "Tabbit 未返回有效 run 信息"}
	}
	return text(payload, "run_id"), status, raw, nil
}

// PrepareBody 是入站体的轻量规整钩子：Tabbit 的上游请求由本包自己组装，入站体原样保留。
func PrepareBody(src []byte) []byte { return src }

// RefreshToken Tabbit 没有续期接口：这里做一次登录态探活，token 失效时返回
// ErrSessionDead，让 pool 记账并提示重新导 cookie。
func (c *Client) RefreshToken(a *auth.Auth) error {
	_, _, _, err := c.GetUserInfo(a)
	return err
}

// RefreshTokenIfNeeded 仅在 skew 内将过期时探活一次；返回是否真的动了账号。
func (c *Client) RefreshTokenIfNeeded(a *auth.Auth, skew time.Duration) (bool, error) {
	if !a.NeedsRefresh(skew) {
		return false, nil
	}
	return false, c.RefreshToken(a)
}

// GetUserInfo 用 /api/v0/user/base-info 验证登录态并取昵称。
func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	base := c.BaseFor(a.Realm())
	req, err := c.signedRequest(http.MethodGet, base+UserInfoPath, "", "/", a, c.baseHeaders(a, "/"))
	if err != nil {
		return "", "", "", err
	}
	status, body, err := c.do(req)
	if err != nil {
		return "", "", "", err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "", "", "", &Error{Kind: ErrSessionDead, Status: status, Msg: "Tabbit 登录态已失效，请重新导出 cookie"}
	}
	if status < 200 || status >= 300 {
		return "", "", "", &Error{Kind: Classify(status, string(body)), Status: status,
			Msg: fmt.Sprintf("Tabbit 用户信息失败（HTTP %d）: %s", status, truncate(errorText(body), 160))}
	}
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	info, _ := payload["user_info"].(map[string]any)
	snap := a.Snapshot()
	uid = orDefault(text(info, "user_id", "uid", "id"), snap.UID)
	// 昵称口径（实测）：上游字段叫 display_name；用户没设过时它是空串，
	// 此时 JWT 的 name 里还有一个上游生成的默认名（user_xxxx）。
	// 旧代码只认 nickname/name/user_name，于是永远拿不到 → 面板显示 uid。
	nickname = text(info, "display_name", "nickname", "name", "user_name")
	if nickname == "" {
		nickname = auth.JWTName(a.JWT())
	}
	// 拿到真名就写回账号（调用方随后 SaveAtomic 时一并落盘）；没有就保留原值，
	// 不用 uid 顶替用户可能手工设过的名字。
	if nickname != "" && nickname != snap.UID {
		a.SetNickname(nickname)
	}
	if nickname == "" {
		nickname = snap.Nickname
	}
	return uid, nickname, text(info, "enterprise_id"), nil
}

// ---------------------------------------------------------------------------
// 小工具

var (
	retryAfterRE   = regexp.MustCompile(`"retryAfterSeconds"\s*:\s*(\d+)`)
	waitTimeRE     = regexp.MustCompile(`"waitTime"\s*:\s*(\d+)`)
	serviceAvailRE = regexp.MustCompile(`"serviceAvailable"\s*:\s*false`)
)

// queueUnavailable 看排队体里的 serviceAvailable:false——上游自己说此刻不可服务。
func queueUnavailable(raw string) bool {
	flat := strings.ReplaceAll(raw, `\`, "")
	return serviceAvailRE.MatchString(flat)
}

// retryAfterOf 抓上游建议的重试间隔（秒）。
func retryAfterOf(raw string) int64 {
	flat := strings.ReplaceAll(raw, `\`, "")
	for _, re := range []*regexp.Regexp{retryAfterRE, waitTimeRE} {
		if m := re.FindStringSubmatch(flat); len(m) == 2 {
			if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

// truncate 截断到 n 个 rune（不切坏多字节字符）。
func truncate(value string, n int) string {
	runes := []rune(value)
	if len(runes) <= n {
		return value
	}
	return string(runes[:n])
}

// text 取第一个非空的字符串/数字字段。
func text(payload map[string]any, keys ...string) string {
	if payload == nil {
		return ""
	}
	for _, key := range keys {
		switch value := payload[key].(type) {
		case string:
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		case json.Number:
			return value.String()
		case float64:
			return strconv.FormatFloat(value, 'f', -1, 64)
		}
	}
	return ""
}

// intOf 把 JSON 数字/数字字符串归一成 int。
func intOf(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case json.Number:
		if n, err := typed.Int64(); err == nil {
			return int(n), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return n, true
		}
	}
	return 0, false
}

// errorText 从错误体里取人类可读文案。
func errorText(body []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if decoder.Decode(&payload) != nil {
		return strings.TrimSpace(string(body))
	}
	if msg := text(payload, "message", "msg", "detail", "error_message", "displayTips"); msg != "" {
		return msg
	}
	if inner, ok := payload["error"].(map[string]any); ok {
		return text(inner, "message")
	}
	if inner, ok := payload["detail"].(map[string]any); ok {
		return text(inner, "message")
	}
	return strings.TrimSpace(string(body))
}

// payloadOf 校验 2xx 并取出 JSON 对象（错误体走 errorText）。
func payloadOf(status int, body []byte, action string) (map[string]any, error) {
	if status < 200 || status >= 300 {
		hint := ""
		switch status {
		case 401, 403:
			hint = "，登录态可能已失效"
		case 429:
			hint = "，请稍后重试"
		}
		return nil, &Error{Kind: Classify(status, string(body)), Status: status,
			Msg: fmt.Sprintf("Tabbit %s失败（HTTP %d）%s: %s", action, status, hint, truncate(errorText(body), 200))}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: fmt.Sprintf("Tabbit %s未返回有效 JSON 对象", action)}
	}
	return payload, nil
}

// login.go 上游登录：手机号 + 短信验证码，全程在面板里完成（不需要 Tabbit 浏览器）。
//
// 实测流程（web.tabbit.ai 与 web.tabbit.com 完全同构）：
//
//	POST /proxy/v0/oauth/send-verification-code
//	     {"uuid":"<64 位随机串>","platform":"1","version":"","app":"1000","mobile":"<手机号>"}
//	     → {"code":0,"error":null,"data":null}
//	POST /proxy/v0/oauth/login
//	     同上前缀 + {"smsCode":"<验证码>","device_info":{…}}
//	     → {"success":true,"code":"<授权码>","expires_in":604800,"user_id":"<uid>","access_token":"<JWT>"}
//	     响应头的 Set-Cookie 里就是浏览器那份登录态（token 等），约 3.4KB。
//
// 关键点：登录态在 **Set-Cookie** 里，body 的 access_token 只是同一份 JWT 的副本。
// 所以这里必须保留响应头（c.do 会丢掉它们），单独走一条带 header 的请求路径。
package upstream

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/misakano7545/tabbit2api-panel/internal/auth"
)

const (
	SendCodePath = "/proxy/v0/oauth/send-verification-code"
	SmsLoginPath = "/proxy/v0/oauth/login"
	// loginAppID / platform 是前端固定值（抓包实测），改动会让上游判参数非法。
	loginAppID = "1000"
	loginPlat  = "1"
)

// LoginCodeError 表示上游明确拒绝了这次操作（验证码错、号码格式错、发得太频繁…）。
// 与网络错误分开：这类错误要原样告诉用户，不要当成"稍后重试"。
type LoginCodeError struct{ Msg string }

func (e *LoginCodeError) Error() string { return e.Msg }

// NewLoginUUID 生成一次登录会话的 uuid（实测 64 位 [A-Za-z0-9]）。
func NewLoginUUID() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, 64)
	raw := make([]byte, 64)
	if _, err := rand.Read(raw); err != nil {
		// 极端情况下退化成时间派生，仍然可用（上游只把它当会话标识）。
		sum := sha256.Sum256([]byte(time.Now().String()))
		copy(raw, sum[:])
	}
	for i := range buf {
		buf[i] = alphabet[int(raw[i])%len(alphabet)]
	}
	return string(buf)
}

// loginDeviceInfo 是登录时上报的设备信息（上游按它记账与风控）。
// device_id_hash 必须稳定：变来变去会被当成新设备。
func loginDeviceInfo() map[string]any {
	host, _ := os.Hostname()
	seed := "tabbit-login-device:" + host
	sum := sha256.Sum256([]byte(seed))
	osVersion := runtime.GOOS
	if runtime.GOOS == "windows" {
		osVersion = "10.0"
	}
	return map[string]any{
		"device_id_hash":  hex.EncodeToString(sum[:]),
		"device_name":     host,
		"platform":        loginPlatformName(),
		"os_version":      osVersion,
		"browser_version": "1.17.16.0",
		"tabbit_version":  "1.17.16(10117016)",
	}
}

// loginPlatformName 上游用的平台名（win / mac / linux）。
func loginPlatformName() string {
	switch runtime.GOOS {
	case "windows":
		return "win"
	case "darwin":
		return "mac"
	default:
		return "linux"
	}
}

// sendJSONWithHeader 发一次 JSON 请求并返回状态码、响应体与响应头。
//
// 单独一份而不是复用 c.do：登录必须读 Set-Cookie，而 c.do 只回 body。
func (c *Client) sendJSONWithHeader(ctx context.Context, realm, path, bodyRaw string) (int, []byte, http.Header, error) {
	base := c.BaseFor(realm)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, strings.NewReader(bodyRaw))
	if err != nil {
		return 0, nil, nil, &Error{Kind: ErrServer, Status: 500, Msg: "构造登录请求失败"}
	}
	req.Header.Set("User-Agent", c.currentUA())
	req.Header.Set("sec-ch-ua", SecChUA)
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/login?callback=close")
	req.Header.Set("x-req-ctx", DefaultReqCtx)
	req.Header.Set("unique-uuid", uniqueUUID(false, c.now()))
	req.Header.Set("trace-id", randomUUID())
	// 登录前没有 token：只带 locale，让上游把这次的登录态种到同一个浏览器身份上。
	req.Header.Set("Cookie", "NEXT_LOCALE=zh")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, nil, nil, ctx.Err()
		}
		return 0, nil, nil, &Error{Kind: ErrServer, Status: 502, Msg: "无法连接 Tabbit 登录服务，请检查网络或代理设置"}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, resp.Header, &Error{Kind: ErrServer, Status: 502, Msg: "读取登录响应失败"}
	}
	return resp.StatusCode, raw, resp.Header, nil
}

// loginErrorText 从登录接口的错误响应里取人话。
//
// 这个接口的错误形状有三种：{"code":非0,"error":"…"}、FastAPI 的
// {"detail":[{…, "msg":…}]}、以及纯文本。逐个试，实在没有就回原文。
func loginErrorText(status int, raw []byte) string {
	var payload map[string]any
	if json.Unmarshal(raw, &payload) == nil {
		if msg := text(payload, "error", "message", "msg", "detail_text"); msg != "" {
			return tidyUpstreamMsg(msg)
		}
		if detail, ok := payload["detail"].([]any); ok && len(detail) > 0 {
			first, _ := detail[0].(map[string]any)
			if msg := text(first, "msg", "message"); msg != "" {
				return tidyUpstreamMsg(msg)
			}
		}
		if msg := text(payload, "detail"); msg != "" {
			return tidyUpstreamMsg(msg)
		}
	}
	if body := strings.TrimSpace(string(raw)); body != "" && len(body) < 300 {
		return body
	}
	return fmt.Sprintf("登录接口返回 HTTP %d", status)
}

// tidyUpstreamMsg 去掉上游校验器加的英文前缀，只留人话。
//
// FastAPI/pydantic 的文案是 "Value error, 手机号格式错误" 这种混排；面板是中文界面，
// 前缀对用户没有信息量，留着只会让人以为出错了。
func tidyUpstreamMsg(msg string) string {
	msg = strings.TrimSpace(msg)
	for _, prefix := range []string{"Value error, ", "value error, ", "Validation error, "} {
		if strings.HasPrefix(msg, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(msg, prefix))
		}
	}
	return msg
}

// SendLoginCode 发一条短信验证码。uuid 由调用方持有，登录时要用同一个。
func (c *Client) SendLoginCode(ctx context.Context, realm, mobile, uuid string) error {
	mobile = strings.TrimSpace(mobile)
	if mobile == "" {
		return &LoginCodeError{Msg: "请填写手机号"}
	}
	if uuid == "" {
		return &LoginCodeError{Msg: "登录会话无效，请重新打开登录窗口"}
	}
	body, _ := json.Marshal(map[string]any{
		"uuid": uuid, "platform": loginPlat, "version": "", "app": loginAppID, "mobile": mobile,
	})
	status, raw, _, err := c.sendJSONWithHeader(ctx, realm, SendCodePath, string(body))
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return &LoginCodeError{Msg: loginErrorText(status, raw)}
	}
	// {"code":0} 才算成功；code 非 0 时 error 字段是原因（如"发送过于频繁"）。
	var payload map[string]any
	if json.Unmarshal(raw, &payload) == nil {
		if code, ok := intOf(payload["code"]); ok && code != 0 {
			msg := text(payload, "error", "message")
			if msg == "" {
				msg = fmt.Sprintf("发送验证码失败（错误码 %d）", code)
			}
			return &LoginCodeError{Msg: msg}
		}
	}
	return nil
}

// SmsLoginResult 是登录成功后从上游拿到的凭据。
type SmsLoginResult struct {
	Cookie   string // 面板导入用的 cookie 串（token 等）
	UID      string
	Nickname string
	Realm    string
	// AccessToken 是 body 里的 access_token（与 cookie 里的 token 同源，兜底用）。
	AccessToken string
}

// SmsLogin 用验证码换登录态。登录态在 Set-Cookie 里，所以这里解析响应头。
func (c *Client) SmsLogin(ctx context.Context, realm, mobile, code, uuid string) (*SmsLoginResult, error) {
	mobile, code = strings.TrimSpace(mobile), strings.TrimSpace(code)
	if mobile == "" || code == "" {
		return nil, &LoginCodeError{Msg: "请填写手机号和验证码"}
	}
	body, _ := json.Marshal(map[string]any{
		"uuid": uuid, "platform": loginPlat, "version": "", "app": loginAppID,
		"mobile": mobile, "smsCode": code, "device_info": loginDeviceInfo(),
	})
	status, raw, header, err := c.sendJSONWithHeader(ctx, realm, SmsLoginPath, string(body))
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &LoginCodeError{Msg: loginErrorText(status, raw)}
	}
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	if ok, exists := payload["success"]; exists {
		if v, isBool := ok.(bool); isBool && !v {
			msg := text(payload, "message", "error", "msg")
			if msg == "" {
				msg = "登录失败"
			}
			return nil, &LoginCodeError{Msg: msg}
		}
	}

	cookie := cookieFromSetCookie(header)
	accessToken := text(payload, "access_token", "accessToken")
	uid := text(payload, "user_id", "userId", "id")
	nickname := text(payload, "display_name", "nickname", "name")

	// Set-Cookie 里没有 token（个别网关会省略）时，用 body 里的 access_token 拼一份：
	// 实测 access_token 就是 token cookie 的值，够后续所有接口用。
	if !strings.Contains(cookie, "token=") && accessToken != "" {
		parts := []string{"token=" + accessToken}
		if uid != "" {
			parts = append(parts, "user_id="+uid, "SAPISID="+uid)
		}
		parts = append(parts, "managed=tab_browser", "NEXT_LOCALE=zh")
		cookie = strings.Join(parts, "; ")
	}
	if cookie == "" || !strings.Contains(cookie, "token=") {
		return nil, &LoginCodeError{Msg: "登录成功但没拿到登录态 cookie，请改用「手动粘贴」"}
	}
	if uid == "" {
		uid = auth.JWTUserID(accessToken)
	}
	if nickname == "" {
		nickname = auth.JWTName(accessToken)
	}
	return &SmsLoginResult{Cookie: cookie, UID: uid, Nickname: nickname, Realm: realm, AccessToken: accessToken}, nil
}

// cookieFromSetCookie 把多个 Set-Cookie 里我们关心的键拼成一条 Cookie 请求头。
//
// 只留 token/user_id/SAPISID/managed/NEXT_LOCALE：其余是统计与登录流程用的
// 一次性 cookie（_gcl_au、g_state…），带着只会增加被判异常的概率。
func cookieFromSetCookie(header http.Header) string {
	if header == nil {
		return ""
	}
	wanted := []string{"token", "user_id", "SAPISID", "managed", "NEXT_LOCALE"}
	values := map[string]string{}
	for _, line := range header.Values("Set-Cookie") {
		name, value, ok := parseSetCookie(line)
		if !ok {
			continue
		}
		for _, key := range wanted {
			if strings.EqualFold(name, key) && value != "" {
				values[key] = value
			}
		}
	}
	parts := make([]string, 0, len(wanted))
	for _, key := range wanted {
		if v := values[key]; v != "" {
			parts = append(parts, key+"="+v)
		}
	}
	return strings.Join(parts, "; ")
}

// parseSetCookie 取出 "name=value" 里的 name 与 value（value 可能含 '='，不能简单 SplitN 后截断）。
func parseSetCookie(line string) (string, string, bool) {
	first, _, _ := strings.Cut(line, ";")
	name, value, ok := strings.Cut(first, "=")
	if !ok {
		return "", "", false
	}
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if name == "" || value == "" {
		return "", "", false
	}
	return name, value, true
}

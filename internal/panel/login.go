package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/misakano7545/tabbit2api-panel/internal/auth"
	"github.com/misakano7545/tabbit2api-panel/internal/upstream"
)

// 登录：手机号 + 短信验证码，全程在面板里完成。
//
// 为什么不再让用户去浏览器抄 cookie：Tabbit 的登录接口（send-verification-code +
// oauth/login）本来就吃纯 HTTP，登录态通过 **Set-Cookie** 下发。网关自己发这两个请求
// 就能拿到和浏览器一模一样的那份 cookie，用户只需填手机号和收到的验证码——不需要
// Tabbit 浏览器，也不需要开 F12 抄 2.4KB 字符串。
const (
	loginTTL = 10 * time.Minute
	// codeCooldown 是「重发验证码」的最小间隔：上游本身有频率限制，本地先挡一下，
	// 免得用户连点把号码打成风控。
	codeCooldown = 60 * time.Second
)

var errLoginSession = errors.New("登录会话已过期，请重新获取验证码")

// 登录方式与地区的绑定关系（面板与文档共用同一句话，避免两边说得不一样）。
const (
	// 手机号登录只服务国内站（见 loginStart 的检查）。
	errSMSOnlyCN = "手机号登录只用于国内版（web.tabbit.com）：请把地区切成「国内版」；国际版请用「凭证导入」（复制整串 cookie）"
)

// loginSession 是一次短信登录。
type loginSession struct {
	Realm    string
	Mobile   string
	UUID     string
	At       time.Time
	Verified bool
}

// randHex 生成 n 字节的随机 hex 串（登录会话 id 等）。
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// loginStart 第一步：发验证码。入参 {realm, mobile}，返回 {id, resend_in}。
//
// 上游要的 uuid（64 位随机串）留在服务端：前端拿不到也不需要知道，
// 少一个可被误用的字段。
func (p *Panel) loginStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Realm  string `json:"realm"`
		Mobile string `json:"mobile"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req)
	realm := normalizeLoginRealm(req.Realm)
	// 登录方式和地区是一一对应的，不是偏好：上游对国际站的手机号登录返回
	// 403 auth.noBetaPermission，国内站又只支持手机号。所以这里直接挡住越界组合，
	// 免得用户拿到一句上游的灰度错误提示去猜。
	if realm != auth.RealmCN {
		writeErr(w, http.StatusBadRequest, errSMSOnlyCN)
		return
	}
	mobile := strings.TrimSpace(req.Mobile)
	if mobile == "" {
		writeErr(w, http.StatusBadRequest, "请填写手机号")
		return
	}
	uuid := upstream.NewLoginUUID()
	// 45s：走代理的首个请求常在 20s 上下（线路冷启动），30s 会偶发误报超时。
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := p.cfg.Upstream.SendLoginCode(ctx, realm, mobile, uuid); err != nil {
		writeErr(w, loginErrStatus(err), publicErr(err))
		return
	}

	id, _ := randHex(8)
	p.loginMu.Lock()
	if p.logins == nil {
		p.logins = map[string]loginSession{}
	}
	for oldID, sess := range p.logins { // 清过期会话，避免面板长开时 map 一直涨
		if time.Since(sess.At) > loginTTL {
			delete(p.logins, oldID)
		}
	}
	p.logins[id] = loginSession{Realm: realm, Mobile: mobile, UUID: uuid, At: time.Now()}
	p.loginMu.Unlock()

	log.Printf("panel: 已发送登录验证码 realm=%s mobile=%s", realm, maskMobile(mobile))
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "realm": realm, "resend_in": int(codeCooldown.Seconds()),
	})
}

// loginPoll 查一次会话状态：前端用它判断「会话还在不在 / 是否已完成」。
// 401/410 = 会话没了（过期或被清理），前端应回到填手机号那一步。
func (p *Panel) loginPoll(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	p.loginMu.Lock()
	sess, ok := p.logins[id]
	p.loginMu.Unlock()
	if !ok || time.Since(sess.At) > loginTTL {
		p.dropLogin(id)
		writeErr(w, http.StatusGone, errLoginSession.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"done": sess.Verified, "realm": sess.Realm})
}

// loginVerify 第二步：用验证码换登录态，落盘入池。入参 {id, code}。
func (p *Panel) loginVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req)
	code := strings.TrimSpace(req.Code)
	if code == "" {
		writeErr(w, http.StatusBadRequest, "请填写验证码")
		return
	}
	p.loginMu.Lock()
	sess, ok := p.logins[req.ID]
	p.loginMu.Unlock()
	if !ok || time.Since(sess.At) > loginTTL {
		p.dropLogin(req.ID)
		writeErr(w, http.StatusGone, errLoginSession.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	result, err := p.cfg.Upstream.SmsLogin(ctx, sess.Realm, sess.Mobile, code, sess.UUID)
	if err != nil {
		writeErr(w, loginErrStatus(err), publicErr(err))
		return
	}
	// 拿到的 cookie 交给同一条导入管线（校验登录态 → 补昵称 → 落盘 → 入池）。
	// realm 用本次登录的站点：短信换来的登录态只在该站有效。
	creds, err := p.cfg.Upstream.PrepareAccount(ctx,
		map[string]any{"cookie": result.Cookie, "realm": result.Realm}, result.Realm)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "登录成功但校验失败："+publicErr(err))
		return
	}
	if creds.Snapshot().Nickname == "" && result.Nickname != "" {
		creds.SetNickname(result.Nickname)
	}
	uid, nick, err := p.saveAccount(creds)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.loginMu.Lock()
	if s, still := p.logins[req.ID]; still {
		s.Verified = true
		p.logins[req.ID] = s
	}
	p.loginMu.Unlock()
	log.Printf("panel: 短信登录成功 uid=%s realm=%s mobile=%s", uid, result.Realm, maskMobile(sess.Mobile))
	writeJSON(w, http.StatusOK, map[string]any{
		"done": true, "ok": true, "uid": uid, "nickname": nick, "realm": result.Realm,
	})
}

// dropLogin 删掉一个会话。
func (p *Panel) dropLogin(id string) {
	p.loginMu.Lock()
	delete(p.logins, id)
	p.loginMu.Unlock()
}

// loginErrStatus 把登录错误映射成 HTTP 状态码。
//
// 验证码错/号码格式错是「用户能自己改」的 400；太频繁是 429；网络与上游 5xx 是 502。
func loginErrStatus(err error) int {
	var codeErr *upstream.LoginCodeError
	if errors.As(err, &codeErr) {
		msg := codeErr.Msg
		if strings.Contains(msg, "频繁") || strings.Contains(msg, "过多") {
			return http.StatusTooManyRequests
		}
		return http.StatusBadRequest
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline exceeded") {
		return http.StatusGatewayTimeout // 上游/代理太慢，不是用户填错了
	}
	var upErr *upstream.Error
	if errors.As(err, &upErr) {
		if upErr.Status >= 500 || upErr.Status == 0 {
			return http.StatusBadGateway
		}
	}
	return http.StatusBadRequest
}

// normalizeLoginRealm 把面板传来的地区归一：只认 cn / intl，其余按国际。
func normalizeLoginRealm(realm string) string {
	if strings.EqualFold(strings.TrimSpace(realm), auth.RealmCN) {
		return auth.RealmCN
	}
	return auth.RealmIntl
}

// maskMobile 只留前 3 后 2，日志里别写完整号码。
func maskMobile(mobile string) string {
	if len(mobile) <= 5 {
		return "***"
	}
	return mobile[:3] + "****" + mobile[len(mobile)-2:]
}

// saveAccount 把一个已确认的凭证落盘并入池（同一 uid 直接覆盖旧文件）。
func (p *Panel) saveAccount(creds *auth.Auth) (uid, nickname string, err error) {
	if creds.UID == "" {
		return "", "", errors.New("上游没有返回用户标识，无法保存账号")
	}
	creds.FilePath = p.cfg.AuthDir + "/" + auth.FileNameFor(creds.UID)
	if err := creds.SaveAtomic(); err != nil {
		return "", "", err
	}
	p.cfg.Pool.Add(creds)
	log.Printf("panel: 账号已入池 uid=%s realm=%s", creds.UID, creds.Realm())
	return creds.UID, creds.Nickname, nil
}

// loginRefresh 用手上已有的 token 直登（Tabbit 没有 refresh token，token 就是唯一凭据）。
// 面板的「手动粘贴」走这里：一次性把 token 换成本地账号记录。
func (p *Panel) loginRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
		Realm string `json:"realm"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	rt := strings.TrimSpace(req.Token)
	if rt == "" {
		writeErr(w, http.StatusBadRequest, "请填写 token（浏览器 cookie 里的 token 值）")
		return
	}
	creds := &auth.Auth{Data: auth.Data{Token: rt, Realm: normalizeLoginRealm(req.Realm)}}
	if err := p.completeByRefresh(r.Context(), creds); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if creds.UID == "" {
		writeErr(w, http.StatusBadGateway, "上游没有返回用户标识，无法保存账号")
		return
	}
	if err := creds.CompleteIdentity(""); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	uid, nick, err := p.saveAccount(creds)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "nickname": nick})
}

// completeByRefresh 探活一次并补齐 uid/昵称（Tabbit 没有续期，这里只做校验）。
func (p *Panel) completeByRefresh(ctx context.Context, creds *auth.Auth) error {
	if err := p.cfg.Upstream.RefreshToken(creds); err != nil {
		return err
	}
	uid, nick, _, err := p.cfg.Upstream.GetUserInfo(creds)
	if err != nil {
		if creds.UID == "" {
			return err
		}
		return nil
	}
	if uid != "" {
		if creds.UID != "" && creds.UID != uid {
			return errors.New("续期返回了不同账号，旧凭证未被覆盖")
		}
		creds.UID = uid
	}
	if nick != "" {
		creds.SetNickname(nick)
	}
	return nil
}

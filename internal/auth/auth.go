// Package auth 解析 Tabbit 账号凭证：磁盘形态、原子写回与目录扫描。
//
// 一个账号一个 JSON（auths/<uid>.json，0600）：
//
//	{"token":"eyJ…","user_id":"…","nickname":"Tabbit-1","realm":"intl","expires_at":1792369369000}
//
// `token` 是 web.tabbit.ai / web.tabbit.com 域下的 cookie（HttpOnly JWT），
// 上游没有 refresh 流程：过期只能重新导出 cookie，所以 token 落盘即全部。
package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// tmpSeq 让同一个账号文件的临时名在进程内唯一（见 SaveAtomic）。
var tmpSeq atomic.Uint64

// 地区：国内外两个站，账号与模型目录都按站分开。
const (
	RealmCN     = "cn"
	RealmIntl   = "intl"
	RealmGlobal = RealmIntl
)

// MaxTokenLength 是 token 字段长度上限（防粘贴整页内容）。
const MaxTokenLength = 8192

// Auth 归一化后的账号凭证。
//
// 并发模型：mu 保护可变字段（Token/ExpiresAt/QuotaPercent），写路径持写锁，
// 读路径持读锁取快照。
type Auth struct {
	Data
	mu       sync.RWMutex
	FilePath string `json:"-"`
}

// Data 是可序列化字段（不含锁与路径，避免拷锁）。
type Data struct {
	Token     string `json:"token"`
	UID       string `json:"user_id"`
	Nickname  string `json:"nickname,omitempty"`
	Email     string `json:"email,omitempty"`
	Realm     string `json:"realm"`
	ExpiresAt int64  `json:"expires_at,omitempty"` // Unix 毫秒；0 = 未知
	// QuotaPercent 是上游 /api/commerce/quota/v1/usage 的 usage_percentage 快照（面板展示用）。
	QuotaPercent float64 `json:"quota_percent,omitempty"`
	QuotaAt      string  `json:"quota_at,omitempty"`
}

// Lock / Unlock / RLock / RUnlock 供其他包在改写字段期间加锁。
func (a *Auth) Lock()    { a.mu.Lock() }
func (a *Auth) Unlock()  { a.mu.Unlock() }
func (a *Auth) RLock()   { a.mu.RLock() }
func (a *Auth) RUnlock() { a.mu.RUnlock() }

// JWT 返回当前 cookie token 的读锁快照（字段名沿用基线：它就是上游的 Bearer 凭据）。
func (a *Auth) JWT() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.Token
}

// ExpiresAtValue 读 token 到期时刻（Unix 毫秒）。
func (a *Auth) ExpiresAtValue() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ExpiresAt
}

// Realm 返回账号所属站点；空值按国际版（web.tabbit.ai）。
func (a *Auth) Realm() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.Data.Realm == "" {
		return RealmIntl
	}
	return a.Data.Realm
}

// NeedsRefresh 报告 token 是否将在 within 内过期。Tabbit 没有续期接口：
// 到期只能重新导 cookie，调用方据此提前报警（不会静默失败）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.NeedsRefreshLocked(within)
}

// NeedsRefreshLocked 是 NeedsRefresh 的持锁内部版本。
func (a *Auth) NeedsRefreshLocked(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return false
	}
	return time.Now().Add(within).UnixMilli() >= a.ExpiresAt
}

// SetToken 写入一次新 token（调用方随后负责 SaveAtomic）。
func (a *Auth) SetToken(token string, expiresAtMs int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if token != "" {
		a.Token = stripBearer(token)
	}
	a.ExpiresAt = expiresAtMs
	if a.ExpiresAt == 0 {
		a.ExpiresAt = JWTExpiry(a.Token)
	}
}

// SetQuota 记录额度快照。
func (a *Auth) SetQuota(percent float64) {
	a.mu.Lock()
	a.QuotaPercent = percent
	a.QuotaAt = time.Now().Format(time.RFC3339)
	a.mu.Unlock()
}

// SetNickname 写入展示名（上游 base-info 的 display_name 或 JWT 的 name）。
//
// 只在拿到**非空且不等于 uid** 的值时写入：上游没设昵称时不要用 uid 覆盖掉
// 用户手工起的名字（面板允许自行命名）。
func (a *Auth) SetNickname(nickname string) bool {
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if nickname == a.UID || nickname == a.Nickname {
		return false
	}
	a.Nickname = nickname
	return true
}

// FileNameFor 给出账号文件的规范名。
func FileNameFor(uid string) string {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return "unknown.json"
	}
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, uid)
	return safe + ".json"
}

// SaveAtomic 原子写回（临时文件 + rename，0600）。FilePath 为空时退到 FileNameFor，
// 调用方负责给绝对路径。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	path := a.FilePath
	if path == "" && a.UID != "" {
		path = FileNameFor(a.UID)
	}
	snap := a.snapshotLocked()
	a.mu.Unlock()
	if path == "" {
		return errors.New("账号没有落盘路径")
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := fmt.Sprintf("%s.%d-%d.tmp", path, os.Getpid(), tmpSeq.Add(1))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// snapshotLocked 返回可序列化副本（调用方持锁）。
func (a *Auth) snapshotLocked() Data { return a.Data }

// Snapshot 返回读锁副本。
func (a *Auth) Snapshot() Data {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.snapshotLocked()
}

// UIDValue 返回账号标识的读锁快照（池的 Release 之类按 uid 记账）。
func (a *Auth) UIDValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.UID
}

// Parse 解析一份账号 JSON（面板粘贴 / 磁盘文件同形）。
// 也接受裸 token 串与浏览器 DevTools 复制的 `token=…; user_id=…` cookie 串。
func Parse(raw []byte) (*Auth, error) {
	text := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(text, "{") {
		return parseFreeform(text)
	}
	var in struct {
		Token     string  `json:"token"`
		Access    string  `json:"access_token"`
		UserID    string  `json:"user_id"`
		UID       string  `json:"uid"`
		Nickname  string  `json:"nickname"`
		Name      string  `json:"name"`
		Email     string  `json:"email"`
		Realm     string  `json:"realm"`
		BaseURL   string  `json:"base_url"`
		ExpiresAt any     `json:"expires_at"`
		Quota     float64 `json:"quota_percent"`
	}
	if err := json.Unmarshal([]byte(text), &in); err != nil {
		return nil, fmt.Errorf("解析账号 JSON: %w", err)
	}
	token := stripBearer(firstNonEmpty(in.Token, in.Access))
	if token == "" {
		return nil, errors.New("缺少 token")
	}
	if len(token) > MaxTokenLength {
		return nil, errors.New("token 过长")
	}
	a := &Auth{Data: Data{
		Token:        token,
		UID:          firstNonEmpty(in.UserID, in.UID),
		Nickname:     firstNonEmpty(in.Nickname, in.Name),
		Email:        in.Email,
		Realm:        realmOf(in.Realm, in.BaseURL),
		ExpiresAt:    millisOf(in.ExpiresAt),
		QuotaPercent: in.Quota,
	}}
	if a.ExpiresAt == 0 {
		a.ExpiresAt = JWTExpiry(token)
	}
	if a.UID == "" {
		a.UID = JWTUserID(token)
	}
	if a.UID == "" {
		return nil, errors.New("无法从 token 解析用户标识，请粘贴含 user_id 的账号记录或 cookie 串")
	}
	a.nicknameFallback()
	return a, nil
}

// nicknameFallback 在没拿到昵称时按「JWT name → uid」补一个。
//
// Tabbit 的 JWT payload 里有 `name`（上游生成的 user_xxxx），它比 uid 更像人名；
// 老版本把昵称直接写成 uid 落了盘，这里把「昵称 == uid」也当成没设置，等于顺手迁移。
func (a *Auth) nicknameFallback() {
	if a.Nickname != "" && a.Nickname != a.UID {
		return
	}
	if name := JWTName(a.Token); name != "" && name != a.UID {
		a.Nickname = name
		return
	}
	if a.Nickname == "" {
		a.Nickname = a.UID
	}
}

// parseFreeform 接受裸 JWT 或 `token=…; user_id=…` cookie 串。
func parseFreeform(text string) (*Auth, error) {
	if strings.Contains(text, "=") {
		fields := map[string]string{}
		for _, part := range strings.Split(text, ";") {
			key, value, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			fields[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
		}
		if token := fields["token"]; token != "" {
			body, _ := json.Marshal(map[string]string{
				"token": token, "user_id": firstNonEmpty(fields["user_id"], fields["sapisid"]),
			})
			return Parse(body)
		}
	}
	if strings.Count(text, ".") == 2 && !strings.ContainsAny(text, " \t\r\n") {
		body, _ := json.Marshal(map[string]string{"token": text})
		return Parse(body)
	}
	return nil, errors.New("无法识别的内容：请粘贴账号 JSON、cookie 串（token=…; user_id=…）或裸 token")
}

// LoadDir 读目录下所有 *.json 账号（单个坏文件只跳过，不阻塞启动）。
func LoadDir(dir string) ([]*Auth, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*Auth, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = path
		out = append(out, a)
	}
	return out, nil
}

// CompleteIdentity 补齐 uid/昵称（导入后 token 与 user_id 已就位）。
func (a *Auth) CompleteIdentity(_ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.UID == "" {
		a.UID = JWTUserID(a.Token)
	}
	if a.UID == "" {
		return errors.New("无法从 token 解析用户标识，请粘贴含 user_id 的完整账号记录")
	}
	a.nicknameFallback()
	return nil
}

// realmOf 由显式 realm 或登录域名推断站点；都没有按国际版。
func realmOf(realm, baseURL string) string {
	switch strings.ToLower(strings.TrimSpace(realm)) {
	case RealmCN, "china", "domestic", "国内", "国内版":
		return RealmCN
	case RealmIntl, "global", "overseas", "international", "国际", "国际版":
		return RealmIntl
	}
	host := ""
	if parsed, err := url.Parse(strings.TrimSpace(baseURL)); err == nil {
		host = "." + strings.ToLower(parsed.Hostname()) + "."
	}
	switch {
	case strings.Contains(host, ".tabbit.com."), strings.Contains(host, ".tabbit-ai.com."):
		return RealmCN
	}
	return RealmIntl
}

func stripBearer(v string) string {
	v = strings.TrimSpace(v)
	return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// JWTExpiry 读 JWT payload 的 exp（毫秒）；解不出返回 0。不验签。
func JWTExpiry(token string) int64 {
	claims := jwtClaims(token)
	if claims == nil {
		return 0
	}
	raw, ok := claims["exp"]
	if !ok {
		return 0
	}
	var sec float64
	if json.Unmarshal(raw, &sec) != nil || sec <= 0 {
		return 0
	}
	return int64(sec * 1000)
}

// JWTUserID 读 JWT payload 里的用户 id（Tabbit 用 `id`）；解不出返回空串。
func JWTUserID(token string) string {
	claims := jwtClaims(token)
	if claims == nil {
		return ""
	}
	for _, key := range []string{"id", "user_id", "userId", "uid", "sub"} {
		raw, ok := claims[key]
		if !ok {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) == nil && text != "" {
			return text
		}
		var num json.Number
		if json.Unmarshal(raw, &num) == nil && num.String() != "" && num.String() != "0" {
			return num.String()
		}
	}
	return ""
}

// JWTName 读 JWT payload 里上游给的默认展示名（Tabbit 字段名是 `name`，
// 形如 user_4b24e854fd25）；解不出返回空串。不验签。
func JWTName(token string) string {
	claims := jwtClaims(token)
	if claims == nil {
		return ""
	}
	for _, key := range []string{"name", "nickname", "display_name"} {
		raw, ok := claims[key]
		if !ok {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) == nil && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

// jwtClaims 解出 JWT payload 的字段表（base64url，不验签）。
func jwtClaims(token string) map[string]json.RawMessage {
	parts := strings.Split(stripBearer(token), ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return nil
		}
	}
	var claims map[string]json.RawMessage
	if json.Unmarshal(payload, &claims) != nil {
		return nil
	}
	return claims
}

// millisOf 把数字/数字字符串/RFC3339 归一成毫秒时间戳（0 = 没有）。
func millisOf(value any) int64 {
	switch v := value.(type) {
	case float64:
		return normalizeMillis(int64(v))
	case int64:
		return normalizeMillis(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return normalizeMillis(n)
		}
	case string:
		trimmed := strings.TrimSpace(v)
		if n, err := json.Number(trimmed).Int64(); err == nil {
			return normalizeMillis(n)
		}
		if t, err := time.Parse(time.RFC3339, trimmed); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}

func normalizeMillis(v int64) int64 {
	if v <= 0 {
		return 0
	}
	if v < 100_000_000_000 {
		return v * 1000
	}
	return v
}

// MaskToken 只回末 4 位（面板展示用）。
func MaskToken(token string) string {
	r := []rune(stripBearer(token))
	if len(r) <= 8 {
		return "«" + fmt.Sprint(len(r)) + " 字符»"
	}
	return "…" + string(r[len(r)-4:])
}

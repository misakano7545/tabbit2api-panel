// models.go 模型目录：Tabbit GET /proxy/v1/model_config/models 的解析、缓存与解析口径。
//
// 上游形态（`{"models":[{...}]}`，部分站点包在 data 里）：
//
//	display_name  "GPT-5.5"          上游 selected_model 只认这个（发 slug 会被静默切模型）
//	model_access_type  "free_unlimited" | 付费档
//	supports_images / supports_tools / support_thinking  能力位
//	display_multiplier  额度倍率（1.0 / 2.5 …）
//
// 对外 id 用 display_name 的 slug（gpt-5-5），`realm:` 前缀由 server 层加。
package upstream

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/misakano7545/tabbit2api-panel/internal/auth"
)

// ModelInfo 是归一化后的目录条目（面板与 /v1/models 共用的形状）。
type ModelInfo struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Realm         string   `json:"realm"`
	ContextWindow int64    `json:"context_length"`
	MaxOutput     int64    `json:"max_output_tokens"`
	Reasoning     bool     `json:"reasoning"`
	Efforts       []string `json:"efforts,omitempty"`
	VL            bool     `json:"vision"`
	Credits       string   `json:"credits,omitempty"`
	Source        string   `json:"function,omitempty"`
	UpstreamKey   string   `json:"upstream_key,omitempty"`
	Visible       bool     `json:"visible"`
	AccessType    string   `json:"access_type,omitempty"`
}

type catalogEntry struct {
	models []ModelInfo
	at     time.Time
}

// staticModels 是目录拉不到时的兜底：只给一条「让上游选默认模型」的入口，
// 不编造具体模型名——目录里没有的名字发上去只会被上游静默换掉。
func staticModels(realm string) []ModelInfo {
	return []ModelInfo{{
		ID: DefaultConfigName, Name: "Default（Tabbit 默认模型）", Realm: realm,
		Reasoning: true, Efforts: []string{}, Source: "tabbit", Visible: true,
	}}
}

// Catalog 返回两个站点的目录缓存。
//
// 没拉到目录的站**不**塞兜底条目：兜底只在一个站都没目录时给一条 `default`。
// 否则「国内站没账号、目录拉不到」会凭空多出一条 cn:default，"default" 这类
// 裸名就会被判成国内模型，选号直接落空（国际号接不了国内模型）。
func (c *Client) Catalog() []ModelInfo {
	c.catMu.RLock()
	defer c.catMu.RUnlock()
	out := make([]ModelInfo, 0, 32)
	for _, realm := range []string{auth.RealmIntl, auth.RealmCN} {
		if entry, ok := c.catalog[realm]; ok {
			out = append(out, entry.models...)
		}
	}
	if len(out) == 0 {
		return staticModels(auth.RealmIntl)
	}
	return out
}

// ResolveModel 按 id / 展示名 / 上游名查目录（大小写与分隔符不敏感）。
// realm 非空时优先在该站里找。
func (c *Client) ResolveModel(model, realm string) (ModelInfo, bool) {
	want := normalizeModelKey(model)
	if want == "" {
		return ModelInfo{}, false
	}
	catalog := c.Catalog()
	order := []string{realm, auth.RealmIntl, auth.RealmCN}
	seen := map[string]bool{}
	for _, target := range order {
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		for _, mi := range catalog {
			if mi.Realm != target {
				continue
			}
			if normalizeModelKey(mi.ID) == want || normalizeModelKey(mi.Name) == want ||
				normalizeModelKey(mi.UpstreamKey) == want {
				return mi, true
			}
		}
	}
	return ModelInfo{}, false
}

// RefreshModels 拉一次某个站点的目录；force=false 时 10 分钟内复用缓存。
func (c *Client) RefreshModels(a *auth.Auth, force bool) ([]ModelInfo, error) {
	if a == nil {
		return c.Catalog(), &Error{Kind: ErrClient, Status: 400, Msg: "没有可用账号，无法拉取模型目录"}
	}
	realm := a.Realm()
	c.catMu.RLock()
	entry, ok := c.catalog[realm]
	fresh := ok && len(entry.models) > 0 && time.Since(entry.at) < 10*time.Minute
	c.catMu.RUnlock()
	if fresh && !force {
		return c.Catalog(), nil
	}

	base := c.BaseFor(realm)
	url := base + CatalogPath + "?a=0&scene=chat"
	req, err := c.signedRequest("GET", url, "", "/newtab", a, c.baseHeaders(a, "/newtab"))
	if err != nil {
		return c.Catalog(), err
	}
	req = req.WithContext(context.Background())
	status, body, err := c.do(req)
	if err != nil {
		return c.Catalog(), err
	}
	if status != 200 {
		return c.Catalog(), &Error{Kind: Classify(status, string(body)), Status: status,
			Msg: fmt.Sprintf("拉取模型目录失败（HTTP %d）: %s", status, truncate(errorText(body), 200))}
	}
	models, err := parseCatalog(body, realm)
	if err != nil {
		return c.Catalog(), err
	}
	c.catMu.Lock()
	c.catalog[realm] = catalogEntry{models: models, at: time.Now()}
	c.catMu.Unlock()
	return c.Catalog(), nil
}

// parseCatalog 平铺 models 列表；同名首次出现者胜；空 display_name 丢弃。
func parseCatalog(body []byte, realm string) ([]ModelInfo, error) {
	payload, err := payloadOf(200, body, "模型目录")
	if err != nil {
		return nil, err
	}
	rawList, _ := payload["models"].([]any)
	if len(rawList) == 0 {
		// 部分站点包一层 data。
		if data, ok := payload["data"].(map[string]any); ok {
			rawList, _ = data["models"].([]any)
		}
	}
	seen := map[string]bool{}
	out := make([]ModelInfo, 0, len(rawList))
	for _, raw := range rawList {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			continue
		}
		mi, ok := normalizeEntry(entry, realm)
		if !ok || seen[mi.ID] {
			continue
		}
		seen[mi.ID] = true
		out = append(out, mi)
	}
	if len(out) == 0 {
		return nil, &Error{Kind: ErrServer, Status: 502, Msg: "模型目录为空"}
	}
	return out, nil
}

// normalizeEntry 把一条上游目录条目归一化。
func normalizeEntry(entry map[string]any, realm string) (ModelInfo, bool) {
	name := text(entry, "display_name", "displayName", "model_name", "name")
	if name == "" {
		return ModelInfo{}, false
	}
	params, _ := entry["params"].(map[string]any)
	pick := func(keys ...string) int64 {
		for _, key := range keys {
			if n, ok := int64Of(entry[key]); ok && n > 0 {
				return n
			}
			if params != nil {
				if n, ok := int64Of(params[key]); ok && n > 0 {
					return n
				}
			}
		}
		return 0
	}
	credits := ""
	if raw, ok := entry["display_multiplier"]; ok {
		if f, ok := floatOf(raw); ok && f > 0 {
			credits = "x" + strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%.2f", f), "0"), ".")
		}
	}
	flag := func(keys ...string) bool {
		for _, key := range keys {
			if v, ok := entry[key].(bool); ok && v {
				return true
			}
		}
		return false
	}
	return ModelInfo{
		ID:            slug(name),
		Name:          name,
		Realm:         realm,
		ContextWindow: pick("context_window", "contextWindow", "max_context_tokens"),
		MaxOutput:     pick("max_tokens", "maxTokens", "max_output_tokens"),
		Reasoning:     flag("support_thinking", "supports_thinking", "support_reasoning"),
		Efforts:       []string{"low", "medium", "high"},
		VL:            flag("supports_images", "support_images", "supports_vision"),
		Credits:       credits,
		Source:        "tabbit",
		UpstreamKey:   name,
		Visible:       true,
		AccessType:    text(entry, "model_access_type", "access_type"),
	}, true
}

var (
	nonSlugRe  = regexp.MustCompile(`[^a-z0-9\-.]+`)
	dashRunRe  = regexp.MustCompile(`-{2,}`)
	modelKeyRe = regexp.MustCompile(`[\s_\-]+`)
)

// slug 把展示名转成对外 id：GPT-5.5 → gpt-5-5，Claude-Opus-4.7 → claude-opus-4-7。
// 空格与点都折成 '-'：客户端 URL 里带点会被某些 SDK 当扩展名截断。
func slug(displayName string) string {
	d := strings.ToLower(strings.TrimSpace(displayName))
	d = strings.ReplaceAll(d, " ", "-")
	d = strings.ReplaceAll(d, ".", "-")
	d = nonSlugRe.ReplaceAllString(d, "-")
	d = dashRunRe.ReplaceAllString(d, "-")
	d = strings.Trim(d, "-.")
	if d == "" {
		return DefaultConfigName
	}
	return d
}

// normalizeModelKey 是查找用的宽松口径：去掉空格/下划线/连字符并小写。
func normalizeModelKey(value string) string {
	return strings.ToLower(modelKeyRe.ReplaceAllString(strings.TrimSpace(value), ""))
}

// FreeModel 返回目录里第一个免费模型（没有则第一个），用于「默认模型」与兜底。
func freeModel(models []ModelInfo) (ModelInfo, bool) {
	if len(models) == 0 {
		return ModelInfo{}, false
	}
	for _, mi := range models {
		if strings.EqualFold(mi.AccessType, "free_unlimited") {
			return mi, true
		}
	}
	return models[0], true
}

// selectedModel 把客户端 model 翻成上游 selected_model（display_name）。
//
// 目录里查得到就用它的上游名；查不到（或请求没带 model）时用该站第一个免费模型。
func (c *Client) selectedModel(a *auth.Auth, requested string) (string, error) {
	realm := a.Realm()
	if mi, ok := c.ResolveModel(requested, realm); ok && mi.UpstreamKey != "" {
		return mi.UpstreamKey, nil
	}
	if requested != "" && !strings.EqualFold(requested, DefaultConfigName) &&
		!strings.EqualFold(requested, "auto") {
		return "", &Error{Kind: ErrClient, Status: 400,
			Msg: fmt.Sprintf("未知模型 %q：可调用 GET /v1/models 查看可用模型", requested)}
	}
	// 兜底：拉一次目录再挑第一个免费模型。
	if _, err := c.RefreshModels(a, false); err != nil {
		return "", err
	}
	for _, mi := range c.Catalog() {
		if mi.Realm != realm {
			continue
		}
		if mi, ok := freeModel([]ModelInfo{mi}); ok && mi.UpstreamKey != "" {
			return mi.UpstreamKey, nil
		}
	}
	return "", &Error{Kind: ErrClient, Status: 503, Msg: "上游模型目录为空，无法确定默认模型"}
}

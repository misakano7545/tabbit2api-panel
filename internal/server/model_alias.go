// model_alias.go 客户端自带的模型名 → 本项目真能调的模型。
//
// 做法照社区现成实现（ZedeX/trae-local-api 的 model-config.json：客户端名字 → config_name 表；
// linqiu919/trae2api 的 convertModelName 同理）：Claude Code / Cursor / Cline 这类工具把
// claude-*、gpt-4o 写死在配置里，别名层让它们零改造接进来，不用手改每个客户端的模型名。
//
// ponytail: 只登记「目标确实存在且实测出正文」的映射；这里不做智能路由、不做 tier 回退
// （参考实现有 tiers/fallback 那套，等真需要多家竞速再加）。
package server

import (
	"strings"

	"github.com/misakano7545/tabbit2api-panel/internal/auth"
	"github.com/misakano7545/tabbit2api-panel/internal/upstream"
)

// strongModel 各地区的旗舰模型：别名没有更贴切的目标时就落到它。
func strongModel(realm string) string {
	if realm == auth.RealmIntl {
		return upstream.DefaultConfigNameIntl
	}
	return upstream.DefaultConfigName
}

// deprecatedClientModels 是客户端配置里写死的旧模型名。
//
// Tabbit 的目录按天变（GPT-5.5 → GPT-5.6 …），逐个登记目标只会很快过时，
// 所以统一落到 `default`：上游会挑该站第一个免费模型。想指定具体模型就写 /v1/models 里的 id。
var deprecatedClientModels = map[string]bool{
	"gpt-4o": true, "gpt-4o-mini": true, "gpt-4.1": true, "gpt-4": true,
	"gpt-3.5-turbo": true, "gpt-5": true, "o1": true, "o3": true,
	"claude-3-5-sonnet": true, "claude-3-7-sonnet": true,
	"claude-sonnet-4-20250514": true, "claude-opus-4-20250514": true,
}

// otherRealm 另一个地区（别名兜底用）。
// aliasModel 查别名。claude-* 走前缀（Claude 客户端会带日期后缀，逐个登记不划算）。
// 入参可能是 normalizeModelName 出来的「Claude-sonnet-4-5」（首字母被大写），统一转小写再比。
// 本地区表里没有就查另一地区：网页端显示名（Gemini-3.1-Pro-Preview）不带地区信息，
// 命中的目标名会由调用方回查 modelRealm 决定真正发哪个地区的号。
// aliasModel 只在「目录里查不到这个名字」时才会被问到（见 mapModel）：
// 到这一步说明客户端发的是它自己写死的名字，落到该站默认模型比 400 有用。
func aliasModel(realm, model string) (string, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if deprecatedClientModels[model] || strings.HasPrefix(model, "claude") || strings.HasPrefix(model, "gpt-") {
		return strongModel(realm), true
	}
	return "", false
}

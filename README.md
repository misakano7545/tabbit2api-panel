<p align="center">
  <h1>Tabbit2API Panel</h1>
</p>

<p align="center">
  <b>把 Tabbit 浏览器 Web 对话包成 OpenAI / Anthropic 兼容 API 的多账号网关 · 附 Web 管理面板</b><br>
  三协议入站 · v3 房间协议 · 多账号轮转 · 请求台账 · 单二进制 · 内嵌面板
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI%20%7C%20Anthropic-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Single_Binary%20%7C%20Docker-2496ED?style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/License-MIT-green?style=flat-square">
</p>

---

> 本项目与 Tabbit 官方无任何关系，**非官方、未获授权**，仅个人自用；仓库不含任何账号、token 或密钥。

> [!IMPORTANT]
> **本仓是 [workbuddy2api-panel](https://github.com/misakano7545/workbuddy2api-panel) 的严格派生**（经 `xiaohuanxiong2api-panel` 传递，同一作者）；Tabbit 协议事实**取自公开实现，并结合作者对本人账号的抓包分析印证**；基线仓仍在维护，但**不保证能力随时同步**。
>
> **维护方式披露**：本仓的 issue / PR 处置、代码审查、测试与修复实现由 **Ekko Studio + Hermes Agent**（AI 编码 agent）辅助完成，合并与发布决策由维护者本人作出。作者是学生，因家庭经济困难，周末需兼职，业余还有摄影、公交等自己的兴趣——**本项目兴趣驱动，更新随缘，不代表作者有全部精力投入维护**，也可能出现较长时间没有提交，请勿当作有 SLA 的商业项目。
>
> 风险自评：**上游随时可能收紧导致通道不可用**；因使用非官方客户端导致的账号风控 / 封禁，由使用者自行承担。

> [!WARNING]
> **郑重提醒**——这段话针对两类人：**现实中与作者同校的使用者**，以及**拿本项目搭建对外中转站、做批量小号 / 售卖额度等恶意滥用的人**。
>
> 作者无法追踪、也不会去追踪任何人的行为，不对任何人行使监控。但必须把话说清楚：上述用法**严重违背上游项目作者与维护者的初衷**，也是消耗本仓作者维护动力的最大来源——本项目免费、开源、由一名学生用课余时间维护，不是给任何人拿去转卖的资源。
>
> 如果哪一天这个项目**确确实实让作者心力憔悴、不再有动力维护，本仓也会停止维护**，且不另行通知。上游的今天，可能就是本仓的明天，请自重。
>
> 同样的底线也适用于**同校使用者**：若有人出于一己私利，或以作者无法理解、也不知情的行为，把本项目的情况反映到**学校信息中心**、作者所在的**二级学院**，乃至**学院院长与校方其他上级领导**，藉此换取奖学金等个人好处，**本仓也会同时停止维护**。这不是赌气——接受核查、说明与后续处理所占用的时间与心力，已经超出本仓能承受的维护成本。

## 项目简介

把 Tabbit 浏览器（`web.tabbit.ai` / `web.tabbit.com`）的 Web 对话能力包成**本地 OpenAI 兼容网关**，
同时提供 **Anthropic Messages** 与 **OpenAI Responses** 两个入口，让 Claude Code / Codex 这类
编码代理直接把它当模型服务用。单二进制、内嵌管理面板、多账号轮转。

本仓是 [workbuddy2api-panel](https://github.com/misakano7545/workbuddy2api-panel) 的**严格派生**
（经 `xiaohuanxiong2api-panel` 同族传递）：目录结构、账号池、调度器、请求台账、
三协议适配层、面板（`internal/panel/index.html` + `app.js`）全部沿用基线，
只把「上游适配层」换成 Tabbit：`internal/upstream`（签名/会话/SSE/目录/额度）与
`internal/auth`（cookie 凭证）。基线里 Tabbit 没有的能力（签到、换设备指纹、refresh token 续期）
**已按上游事实删除**，不留点了没反应的按钮。

- **OpenAI SDK / curl** → `POST /v1/chat/completions`
- **OpenAI Codex / 新版 Agent** → `POST /v1/responses`
- **Claude Code / Anthropic SDK** → `POST /v1/messages`（同时挂 `/messages`、`/v1/messages/count_tokens`）


## Tabbit 上游是什么形状

| 能力 | 上游 | 说明 |
|---|---|---|
| 对话 | `POST /api/v3/chat/rooms/{room}/join` + `/runs` | **v3 房间协议**：先 join 订阅 SSE，再向 runs 投递消息；正文仍是纯文本，没有原生 tool_calls（旧 `/api/v1/chat/completion` 已不在 Web UI 里使用） |
| 会话 | `POST /panel/session` | 取 `chat_session_id`（= 房间 id），同一账号复用（30 分钟 TTL） |
| 目录 | `GET /proxy/v1/model_config/models` | `selected_model` 只认目录里的 **display_name**，发 slug 会被静默换模型；`selected_model` 是**数组** |
| 额度 | `GET /api/commerce/quota/v1/usage?user_id=…&timezone=…` | **百分比制**：只有 `usage_percentage`（已用 %）与 `member_level`、`remaining_reset_minutes`、`unused_reset_coupon_count`；没有积分包/金额。只按 user_id 查，不带 cookie 也能读 |
| 签到 | `POST /api/commerce/activity/v1/sign-in`（+`/status`） | 真实运营功能：`{request_no, scene_codes:["daily_sign_in"], usage_percentage}` → `sign_in_result` |
| 签名 | `GET /chat/sign-key` | 动态 key；`x-nonce = HMAC-SHA256(key, "{ts}.{uuid}.{sha256(body)}")`，`x-signature` 是那个 uuid（**只有 join 带签名**，runs 不签）。两站 key 不同：intl `f8d0e6a7…`、cn `3a9b1c8d…` |
| 登录 | 浏览器 cookie | `token`（RS256 JWT，7 天）+ `user_id`；**没有续期接口**，过期只能重新导出 |

网关做五件事：选号、把请求摊成上游能吃的一段 prompt、把工具声明编译成文本协议并在回包里还原、
把 SSE 翻译成 OpenAI chunk、把「请求本身被拒」与「账号故障」分开。下游三套协议因此只认一种线上形状。

### 两个必带头

- `x-req-ctx`：base64 的客户端版本号（`MS4xNy4xNigxMDExNzAxNik=` = `1.17.16(10117016)`）。格式不对上游判 493「版本过低」。
- `unique-uuid`：客户端自证标记。第 5 位是「是否默认浏览器」，位置 `[2,7,11,14,18,21,25,28]` 编码当前时间。
  本仓默认如实填 **非默认浏览器**——标记为真是上游认可的 Pro 自证路径，伪造等于冒充付费用户。

## 快速开始

```bash
git clone https://github.com/misakano7545/tabbit2api-panel
cd tabbit2api-panel
cp config.example.json config.json     # 按需改 listen / api_key
go build -o tabbit2api ./cmd/server
./tabbit2api -config ./config.json
```

```bash
curl -s http://127.0.0.1:7867/healthz
curl -s -H 'Authorization: Bearer ***' http://127.0.0.1:7867/v1/models
curl -s -H 'Authorization: Bearer ***' -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5-5","messages":[{"role":"user","content":"你好"}]}' \
  http://127.0.0.1:7867/v1/chat/completions
```

面板：`http://127.0.0.1:7867/panel/`，输入 `api_key` 进入（与 `/v1` 同一把钥匙）。

Docker：`docker compose up -d --build`（`auths/`、`data/`、`config.json` 都挂出来）。

## 加账号

面板「添加账号」有两条路，都在网关进程里完成，不需要网关机器上有浏览器：

| 路径 | 适用 | 网关做什么 |
|---|---|---|
| **手机号登录** | 国内站（web.tabbit.com） | 自己发 `send-verification-code` / `oauth/login`，读响应 Set-Cookie 里的 `token` 等五项，校验后落盘入池 |
| **凭证导入** | 两站通用（国际站唯一入口） | 粘 DevTools 的 **Copy as cURL** 整段、`token=…; user_id=…`、账号 JSON 或裸 JWT，自动挑出 Cookie |

地区与登录方式是绑定的，界面会按地区只显示可用那条：国内版 → 手机号登录；国际版 → 凭证导入。
越界组合在服务端也会被拒（错误文案直接告诉你该用哪个）。

### 为什么国际站没有「自动登录」

Google 只把授权结果投递给 OAuth client 登记的 `redirect_uri`，而 Tabbit 那个
`client_id` 对所有网关能接到的地址都回 `redirect_uri_mismatch`（loopback / localhost /
`postmessage` / 站点自身路径全试过，见 `docs/tabbit-live-capture.md` §13）；
国际站的手机号登录又处在上游灰度里（`403 auth.noBetaPermission`）。
所以国际号只能把**已有的**登录态搬进来——`Copy as cURL` 整段粘贴是最省事的一种。

> 只贴 `token` 不够：实测单独 `Cookie: token=…` 会被判 401「无权限」，必须带整串
> （`user_id` / `SAPISID` / `managed` / `NEXT_LOCALE`）。面板的导入会自己挑全。

## 配置

`config.json` 的每个键都能用 `Q2A_<大写>` 环境变量覆盖（环境变量优先）。

| 键 | 默认 | 说明 |
|---|---|---|
| `listen` | `127.0.0.1:7867` | 监听地址 |
| `api_key` | `""` | `/v1/*` 与面板的密钥；空 = 不鉴权（仅本机） |
| `auth_dir` / `state_file` | `./auths` / `./data/state.json` | 账号目录与池状态 |
| `default_model` | `default` | 请求没带 model 时用（`default` = 该站第一个免费模型） |
| `upstream.base_intl` / `base_cn` | `https://web.tabbit.ai` / `https://web.tabbit.com` | 两个站基址（热生效） |
| `upstream.timeout_seconds` / `header_timeout_seconds` / `idle_timeout_seconds` | `120/120/300` | 短请求总超时 / 首字节超时 / 流内空闲 |
| `cooldown.plan_credit` / `soft_rate` / `soft_rate_max` | `12h/60s/2h` | 权益（492）、限流冷却与退避封顶 |
| `pool.*` | 见示例 | 在途上限、熔断/降权阈值、闲置补偿 |
| `schedule.keepalive_hours` | `[3]` | 每日探活时点 |
| `schedule.checkin_hours` / `checkin_enabled` | `[9]` / `true` | 每日签到时点（上游 sign-in 接口；活动没开时上游回 `activity_open=false`，这一轮跳过） |

## 接入客户端

**Claude Code**

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:7867
export ANTHROPIC_AUTH_TOKEN=<api_key>
```

**Codex**

```toml
model_provider = "tabbit"
model = "gpt-5-5"

[model_providers.tabbit]
name = "Tabbit"
base_url = "http://127.0.0.1:7867/v1"
env_key = "TABBIT_API_KEY"
wire_api = "responses"
```

**任何 OpenAI SDK**：`base_url=http://127.0.0.1:7867/v1`。

## 工具调用怎么来的

上游没有结构化 tool_calls，所以只能走文本协议：

```
<tool_call>{"name":"exec_command","arguments":{"cmd":"ls -la"}}</tool_call>
```

- 请求声明了 `tools` 时，网关在 prompt 末尾追加协议说明与工具清单；
- 回包由 `internal/upstream/toolcall.go` **增量**扫描（跨 chunk 也能拼回完整标记），
  还原成 `delta.tool_calls` —— 非增量的话工具调用要等整段回复结束才出现，客户端会先超时；
- 判定从严：标签必须完整闭合、JSON 必须有 `name`、工具名要在请求声明的名单里。
  任何一条不满足就**原文透出**，修复层绝不吞字节。

## 安全与合规

- **非官方、未获授权**：本文与上游服务商无任何关系，仅用于**本人授权账号**在本机 / 私有环境下的协议互操作。
- **凭证**：`auths/*.json` 以 0600 明文保存，目录不得进入备份、同步盘或共享范围；仓库不含任何账号、token 或密钥。
- **鉴权**：`api_key`（或 `api_keys`）为空时 `/v1/*` 与 `/panel/api/*` 不鉴权，仅适合 `127.0.0.1` 或可信内网；公网部署必须设密钥并前置 HTTPS 反代与访问限流。
- **合规边界**：不得用于搭建对外中转站、共享他人账号、批量注册小号或售卖额度；作者不承担使用后果。

## 行为与边界

- **模型名**：对外 id 是目录 `display_name` 的 slug（`GPT-5.5` → `gpt-5-5`），带 `intl:` / `cn:` 前缀区分两个站；
  裸名按「有账号的那个站」解析。目录拉不到时用内置兜底表，不会编造模型名。
- **单次上限**：上游对一次请求的字符数有硬限制（实测 2 万字符必失败，且错误文案是营销语、极具误导性）。
  网关按 16000 字符预算裁剪：系统指令优先，其余从最近往回填，并明确告知模型「历史已截断」。
- **图片**：上游只接受文本。带图的请求会把图片标成「未被读取」的文字占位，**不假装看得见**。
- **usage**：上游**一个 token 数都不报**（实抓：`run_completed` 只带 `{run_id, status}`，
  整条流没有 usage 帧）。网关按 `usage.EstimateTokens`（中文 0.62 token/字、英文 4 字符/token）
  估算提示词与产出，随响应下发并标注 `"estimated": true`，请求记录里显示成 `~1.2k`；
  估算只进请求记录，**不进用量台账、不算积分**——不把估算当账单。
- **额度是百分比制**：上游只报「已用 %」，面板按 `剩余 = 100 - 已用` 显示（如 `91.93%`），
  并在下面给出与站点一致的倒计时（`6天21小时后重置`）。倒计时按**绝对重置时刻**算
  （由上游 `remaining_reset_minutes` 换算并持久化），所以两次刷新之间也不会停住或漂移。
- **错误口径**：401 探活失败计 session 失效（连续达阈值才禁用），429 软冷却，5xx 熔断换号；
  请求本身被上游拒（其余 4xx）不换号，照实回上游状态码与原话。流里 `{"error":…}` 不算成功（会触发换号）。
- **没有的能力**：设备指纹轮换、refresh token 续期——上游没有，面板也不提供入口。
  （签到**有**：`/api/commerce/activity/v1/sign-in`，面板行内「签到」按钮 + 每日排程，活动未开时如实跳过。）
- **代理**：HTTP(S)_PROXY / NO_PROXY 生效（`New()` 里的 Transport 显式设了 `ProxyFromEnvironment`）。
  直连 web.tabbit.ai 会拿到 CDN 的 HTML 403（表现为「登录态失效」），所以走代理是常见前提。

## 开发

```bash
go build ./...
go test ./...          # internal/server 用 httptest 假 Tabbit 覆盖三协议 × 流/非流 × 工具调用 × 492
go test -race ./...

# 手工冒烟：假上游 + 真进程
python3 scripts/fake_tabbit.py 18123 &
./tabbit2api -config ./config.json
```

## 血统与致谢

- 本仓是 [`workbuddy2api-panel`](https://github.com/misakano7545/workbuddy2api-panel) 的严格派生（经 `xiaohuanxiong2api-panel` 传递，同一作者），面板形态、账号池、调度器与三协议适配层沿用基线，只替换上游适配层。
- 基线仓仍在维护；能力同步不保证，问题在本仓处理，不向上游基线仓报。
- 感谢基线作者的把同族骨架开源。

## License

MIT，见 [LICENSE](LICENSE)。

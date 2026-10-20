# tabbit2api-panel

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

> 本项目与 Tabbit 官方无任何关系，非官方、未获授权。请遵守上游服务条款，仅个人自用。
> 仓库不含任何账号、token 或密钥。

## Tabbit 上游是什么形状

| 能力 | 上游 | 说明 |
|---|---|---|
| 对话 | `POST /api/v1/chat/completion` | **一段纯文本进、SSE 文本流出**：没有 messages 数组，也没有原生 tool_calls |
| 会话 | `POST /panel/session` | 取 `chat_session_id`，同一账号复用（30 分钟 TTL） |
| 目录 | `GET /proxy/v1/model_config/models` | `selected_model` 只认目录里的 **display_name**，发 slug 会被静默换模型 |
| 额度 | `GET /api/commerce/quota/v1/usage` | 只有 `usage_percentage`（已用百分比）与 `member_level`，没有金额 |
| 签名 | `GET /chat/sign-key` | 动态 key；`x-nonce = HMAC-SHA256(key, "{ts}.{uuid}.{sha256(body)}")`，`x-signature` 是那个 uuid（名字反直觉，按线上行为为准） |
| 登录 | 浏览器 cookie | `token`（HttpOnly JWT）+ `user_id`；**没有续期接口**，过期只能重新导出 |

网关做五件事：选号、把请求摊成上游能吃的一段 prompt、把工具声明编译成文本协议并在回包里还原、
把 SSE 翻译成 OpenAI chunk、把「请求本身被拒」与「账号故障」分开。下游三套协议因此只认一种线上形状。

### 两个必带头

- `x-req-ctx`：base64 的客户端版本号（`MS42LjIwKDEwMTA2MDIwKQ==` = `1.6.20(10106020)`）。格式不对上游判 493「版本过低」。
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

Tabbit 没有设备码授权、没有 refresh token，唯一入口是**浏览器 cookie**：

1. 在 Tabbit 浏览器里登录；
2. DevTools → Application → Cookies → `https://web.tabbit.ai`；
3. 复制 `token` 与 `user_id`，拼成 `token=…; user_id=…`，粘到面板「导入凭证」；
   也可以直接粘账号 JSON：`{"token":"eyJ…","user_id":"…","nickname":"Tabbit-1","realm":"intl"}`。

导入时网关会用 `/api/v0/user/base-info` 验一次登录态，验不过不会落盘。账号文件在
`auths/<uid>.json`（0600，原子写）。`realm` 是 `intl`（web.tabbit.ai）或 `cn`（web.tabbit.com），
决定这个号走哪个站——国内外模型重名，混着用会拿到「没有该模型权限」。

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
| `schedule.checkin_*` | — | **不生效**：Tabbit 没有签到接口，保留键只为旧配置不报错 |

## 接到客户端

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

## 行为与边界

- **模型名**：对外 id 是目录 `display_name` 的 slug（`GPT-5.5` → `gpt-5-5`），带 `intl:` / `cn:` 前缀区分两个站；
  裸名按「有账号的那个站」解析。目录拉不到时用内置兜底表，不会编造模型名。
- **单次上限**：上游对一次请求的字符数有硬限制（实测 2 万字符必失败，且错误文案是营销语、极具误导性）。
  网关按 16000 字符预算裁剪：系统指令优先，其余从最近往回填，并明确告知模型「历史已截断」。
- **图片**：上游只接受文本。带图的请求会把图片标成「未被读取」的文字占位，**不假装看得见**。
- **usage**：上游实测很少给 token 数，网关不编造——拿到才透出，拿不到就是 0。
- **错误口径**：401 探活失败计 session 失效（连续达阈值才禁用），429 软冷却，5xx 熔断换号；
  请求本身被上游拒（其余 4xx）不换号，照实回上游状态码与原话。流里 `{"error":…}` 不算成功（会触发换号）。
- **没有的能力**：签到、积分/签到收益、设备指纹轮换、refresh token 续期——上游没有，面板也不再提供入口。

## 开发

```bash
go build ./...
go test ./...          # internal/server 用 httptest 假 Tabbit 覆盖三协议 × 流/非流 × 工具调用 × 492
go test -race ./...

# 手工冒烟：假上游 + 真进程
python3 scripts/fake_tabbit.py 18123 &
./tabbit2api -config ./config.json
```

## License

MIT，见 [LICENSE](LICENSE)。基线来自 workbuddy2api-panel（同一作者）。

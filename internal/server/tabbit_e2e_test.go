package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/misakano7545/tabbit2api-panel/internal/auth"
	"github.com/misakano7545/tabbit2api-panel/internal/pool"
	"github.com/misakano7545/tabbit2api-panel/internal/reqlog"
	"github.com/misakano7545/tabbit2api-panel/internal/upstream"
)

// fakeTabbit 是 Tabbit 上游的假实现：签名 key、会话、SSE 对话、目录、额度各回一份真形状。
// 对话按 reply 逐片吐 message_chunk —— 工具调用标记天然被切成几段，正好验证增量扫描。
type fakeTabbit struct {
	reply    []string
	thinking string
	failCode int // 非 0 时首帧回 error 事件（模拟 492 权益不足）
	// usage 非 nil 时随 run_completed 的 payload 带 token_usage（模拟真上游的用量帧）。
	usage    map[string]any
	lastBody map[string]any
	lastReq  *http.Request
	// joined 记录 join 是否先于 runs 发生（协议顺序断言）。
	joined  chan struct{}
	runSent chan struct{}
}

func (f *fakeTabbit) handler() http.Handler {
	if f.joined == nil {
		f.joined = make(chan struct{}, 1)
	}
	if f.runSent == nil {
		f.runSent = make(chan struct{}, 1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/sign-key", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "test-sign-key")
	})
	mux.HandleFunc("/panel/session", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"chat_session_id":"sess-1"}`)
	})
	// v3 join：先发订阅快照，再从 runs 到达后推送事件（真上游也是这个顺序）。
	mux.HandleFunc("/api/v3/chat/rooms/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/join"):
			if got := r.Header.Get("x-nonce"); got == "" {
				http.Error(w, `{"message":"missing signature"}`, http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flush := func() {
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
			}
			select {
			case f.joined <- struct{}{}:
			default:
			}
			flush()
			select {
			case <-f.runSent:
			case <-time.After(3 * time.Second):
				http.Error(w, "no run", http.StatusGatewayTimeout)
				return
			}
			f.streamRun(w, flush)
			return
		case strings.HasSuffix(r.URL.Path, "/runs"):
			f.lastReq = r
			body, _ := io.ReadAll(r.Body)
			f.lastBody = map[string]any{}
			_ = json.Unmarshal(body, &f.lastBody)
			select {
			case f.runSent <- struct{}{}:
			default:
			}
			_, _ = io.WriteString(w, `{"run_id":"run-1","room_id":"sess-1","status":"QUEUED"}`)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/proxy/v1/model_config/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"models":[
			{"display_name":"GPT-5.5","model_access_type":"free_unlimited",
			 "supports_images":true,"supports_tools":true,"support_thinking":true,
			 "context_window":200000,"max_tokens":32000,"display_multiplier":1}]}`)
	})
	mux.HandleFunc("/api/v0/user/base-info", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"user_info":{"user_id":"u-1","nickname":"测试号"}}`)
	})
	mux.HandleFunc("/api/commerce/quota/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"usage_percentage":12.5,"member_level":"free"}`)
	})
	mux.HandleFunc("/api/commerce/activity/v1/sign-in/status", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"scene_code":"daily_sign_in","activity_open":true,"signed_today":false}]}`)
	})
	mux.HandleFunc("/api/commerce/activity/v1/sign-in", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"scene_code":"daily_sign_in","sign_in_result":"success","usage_reward_result":"skipped"}]}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	return mux
}

// streamRun 按真上游的 v3 事件序列推一次对话。
//
// 真上游每帧是一行紧凑 JSON：data: 行必须单行，否则截断成多条 data 行后
// 下游只能拼回半截 JSON（这里用 marshal 而不是手写多行字符串）。
func (f *fakeTabbit) streamRun(w http.ResponseWriter, flush func()) {
	emit := func(event string, payload map[string]any) {
		env := map[string]any{
			"event_id":   "e-" + event,
			"room_id":    "sess-1",
			"event_type": event,
			"payload":    payload,
			"created_at": "2026-10-21T00:00:00Z",
		}
		raw, _ := json.Marshal(env)
		_, _ = io.WriteString(w, "id: 1\nevent: "+event+"\ndata: "+string(raw)+"\n\n")
		flush()
	}
	base := func(extra map[string]any) map[string]any {
		out := map[string]any{"run_id": "run-1"}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	emit("snapshot_begin", map[string]any{})
	emit("active_runs_snapshot", map[string]any{"runs": []any{}})
	emit("snapshot_end", map[string]any{})
	emit("run_queued", base(map[string]any{"status": "QUEUED"}))
	emit("run_started", base(map[string]any{"status": "RUNNING"}))
	emit("assistant_message_started", base(map[string]any{"message_id": "m-1"}))
	if f.failCode != 0 {
		emit("run_failed", base(map[string]any{"status": "FAILED", "message": "欢迎使用 Tabbit 浏览器"}))
		return
	}
	if f.thinking != "" {
		emit("event_message_chunk", base(map[string]any{
			"chunk_type":    "thinking",
			"chunk_payload": map[string]any{"reasoning_content": f.thinking, "message_id": "m-1"},
		}))
		emit("event_message_chunk", base(map[string]any{
			"chunk_type": "thinking_finished", "chunk_payload": map[string]any{"message_id": "m-1"},
		}))
	}
	for _, piece := range f.reply {
		emit("assistant_message_delta", base(map[string]any{
			"chunk_type":    "message_chunk",
			"delta":         piece,
			"chunk_payload": map[string]any{"content": piece, "message_id": "m-1"},
			"message_id":    "m-1",
		}))
	}
	completed := base(map[string]any{"status": "COMPLETED"})
	if f.usage != nil {
		completed["token_usage"] = f.usage
	}
	emit("assistant_message_finished", base(map[string]any{"message_id": "m-1"}))
	emit("run_completed", completed)
	emit("room_reset_required", base(map[string]any{"status": "COMPLETED"}))
}

func quote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// newTestHandler 建一个指向假上游的 handler（单账号、不鉴权）。
func newTestHandler(t *testing.T, upURL string) *Handler {
	t.Helper()
	client := upstream.New()
	client.SetHosts(upURL, upURL)
	p := pool.New("")
	p.Add(&auth.Auth{Data: auth.Data{
		Token: "cookie-token", UID: "u-1", Nickname: "测试号", Realm: auth.RealmIntl,
		ExpiresAt: time.Now().Add(48 * time.Hour).UnixMilli(),
	}})
	return NewHandler(Config{
		Pool: p, Upstream: client, MaxRotate: 1,
		RefreshSkew: 5 * time.Minute, DefaultModel: upstream.DefaultConfigName,
		RequestLog: reqlog.New(reqlog.Config{}),
	})
}

func newFakeHandler(t *testing.T) (*Handler, *fakeTabbit) {
	t.Helper()
	fake := &fakeTabbit{reply: []string{"你好"}}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)
	return newTestHandler(t, srv.URL), fake
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec
}

// 非流式 chat：正文照原样回来，model 必须是客户端请求的名字（压掉上游内部名）。
func TestChatNonStream(t *testing.T) {
	h, fake := newFakeHandler(t)
	fake.thinking = "想一下"
	rec := post(t, h, "/v1/chat/completions",
		`{"model":"gpt-5-5","messages":[{"role":"user","content":"在吗"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解响应失败: %v (%s)", err, rec.Body.String())
	}
	if out.Model != "gpt-5-5" {
		t.Errorf("model = %q，应为客户端请求的名字", out.Model)
	}
	if got := out.Choices[0].Message.Content; got != "你好" {
		t.Errorf("content = %q", got)
	}
	if got := out.Choices[0].Message.ReasoningContent; got != "想一下" {
		t.Errorf("reasoning_content = %q", got)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", out.Choices[0].FinishReason)
	}
	// 上游只认 display_name，发 slug 会被静默换模型（v3：selected_model 是数组）。
	input, _ := fake.lastBody["input_payload"].(map[string]any)
	models, _ := input["selected_model"].([]any)
	if len(models) != 1 || models[0] != "GPT-5.5" {
		t.Errorf("selected_model = %v，应为 [\"GPT-5.5\"]", input["selected_model"])
	}
	if prompt, _ := input["content"].(string); prompt == "" {
		t.Errorf("input_payload.content 为空")
	}
}

// 流式 chat：帧要带 data: 前缀、以 [DONE] 收尾。
func TestChatStream(t *testing.T) {
	h, fake := newFakeHandler(t)
	fake.reply = []string{"你", "好", "呀"}
	rec := post(t, h, "/v1/chat/completions",
		`{"model":"gpt-5-5","stream":true,"messages":[{"role":"user","content":"在吗"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: ") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("不是 OpenAI SSE 形状: %s", body)
	}
	if !strings.Contains(body, `"content":"你"`) {
		t.Errorf("缺正文帧: %s", body)
	}
}

// 工具调用：上游只吐纯文本，网关要把它还原成结构化 tool_calls（流式与非流式都要）。
func TestToolCallRepair(t *testing.T) {
	h, fake := newFakeHandler(t)
	// 故意把标记切成三段，验证跨帧扫描。
	fake.reply = []string{"稍等。", "<tool_", `call>{"name":"lookup","arguments":{"q":1}}</tool_`, "call>"}

	rec := post(t, h, "/v1/chat/completions",
		`{"model":"gpt-5-5","messages":[{"role":"user","content":"查一下"}],
		  "tools":[{"type":"function","function":{"name":"lookup","description":"查",
		            "parameters":{"type":"object","properties":{"q":{"type":"integer"}}}}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解响应失败: %v (%s)", err, rec.Body.String())
	}
	msg := out.Choices[0].Message
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d，应为 1（正文 %q）", len(msg.ToolCalls), msg.Content)
	}
	if msg.ToolCalls[0].Function.Name != "lookup" {
		t.Errorf("name = %q", msg.ToolCalls[0].Function.Name)
	}
	if !strings.Contains(msg.ToolCalls[0].Function.Arguments, `"q":1`) {
		t.Errorf("arguments = %q", msg.ToolCalls[0].Function.Arguments)
	}
	if strings.Contains(msg.Content, "<tool_call>") {
		t.Errorf("标记没被剥离: %q", msg.Content)
	}
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", out.Choices[0].FinishReason)
	}
	// 协议说明必须进了上游 prompt，否则模型不知道该怎么调。
	input, _ := fake.lastBody["input_payload"].(map[string]any)
	if prompt, _ := input["content"].(string); !strings.Contains(prompt, "<tool_call>") {
		t.Errorf("prompt 里没有工具协议: %q", prompt)
	}
}

// Claude Messages：非流式要 Anthropic 形状（content 数组 + stop_reason + usage）。
func TestMessagesNonStream(t *testing.T) {
	h, _ := newFakeHandler(t)
	rec := post(t, h, "/v1/messages",
		`{"model":"gpt-5-5","max_tokens":64,"messages":[{"role":"user","content":"在吗"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解响应失败: %v", err)
	}
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Errorf("不是 Anthropic message: %v", out)
	}
	blocks, _ := out["content"].([]any)
	if len(blocks) == 0 {
		t.Fatalf("content 为空: %v", out)
	}
	first, _ := blocks[0].(map[string]any)
	if first["text"] != "你好" {
		t.Errorf("text = %v", first["text"])
	}
	if _, ok := out["usage"].(map[string]any); !ok {
		t.Errorf("缺 usage: %v", out)
	}
}

// Codex Responses：形状要对（object=response + output 数组）。
func TestResponsesNonStream(t *testing.T) {
	h, _ := newFakeHandler(t)
	rec := post(t, h, "/v1/responses",
		`{"model":"gpt-5-5","input":"在吗"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解响应失败: %v", err)
	}
	if out["object"] != "response" {
		t.Errorf("object = %v", out["object"])
	}
	if _, ok := out["output"].([]any); !ok {
		t.Errorf("缺 output 数组: %v", out)
	}
}

// 上游 200 里的 error 事件（492 权益）必须当失败：不能记成成功、也不能回空 200。
func TestStreamErrorEvent(t *testing.T) {
	h, fake := newFakeHandler(t)
	fake.failCode = 492
	rec := post(t, h, "/v1/chat/completions",
		`{"model":"gpt-5-5","messages":[{"role":"user","content":"在吗"}]}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("492 被当成成功了: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Errorf("错误体不带 error 字段: %s", rec.Body.String())
	}
}

// 客户端写死的旧模型名与 "default" 都不能 400：前者落到默认模型，后者交给上游挑。
// 这两个分支都曾因为「裸名地区兜底」被解析到没有账号的站（503 / 400）。
func TestModelAliases(t *testing.T) {
	h, _ := newFakeHandler(t)
	for _, model := range []string{"claude-3-5-sonnet-20241022", "gpt-4o", "default"} {
		rec := post(t, h, "/v1/chat/completions",
			`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusOK {
			t.Errorf("%s → %d: %s", model, rec.Code, rec.Body.String())
		}
	}
	// 不带 model 字段：走 DefaultModel，同样要能出结果。
	rec := post(t, h, "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Errorf("缺 model → %d: %s", rec.Code, rec.Body.String())
	}
}

// 请求记录要能看到每次请求的 token 用量：上游把 token_usage 放在 run_completed 的
// payload 里，此前被当成忽略帧丢掉，面板请求表整列都是「—」。这里断言三条通路：
// 非流式 usage 字段、流式末帧 usage 字段、以及 reqlog 事件里的 token 数。
func TestUsageReachesRequestLog(t *testing.T) {
	usage := map[string]any{"input_tokens": 120, "output_tokens": 45}

	// 非流式：响应体要带 usage（OpenAI 形状）。
	h, fake := newFakeHandler(t)
	fake.usage = usage
	rec := post(t, h, "/v1/chat/completions",
		`{"model":"gpt-5-5","messages":[{"role":"user","content":"在吗"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解响应失败: %v (%s)", err, rec.Body.String())
	}
	if out.Usage.PromptTokens != 120 || out.Usage.CompletionTokens != 45 || out.Usage.TotalTokens != 165 {
		t.Errorf("非流式 usage = %+v", out.Usage)
	}

	// 流式：末帧（finish_reason 帧）要带 usage。
	h2, fake2 := newFakeHandler(t)
	fake2.usage = usage
	rec = post(t, h2, "/v1/chat/completions",
		`{"model":"gpt-5-5","stream":true,"messages":[{"role":"user","content":"在吗"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"prompt_tokens":120`) {
		t.Errorf("流式末帧缺 usage: %s", rec.Body.String())
	}

	// 请求记录（reqlog）：两条请求的事件里都要有 token 数。
	check := func(h *Handler, want int64) {
		t.Helper()
		ev := h.cfg.RequestLog.Snapshot().Recent
		if len(ev) == 0 {
			t.Fatal("请求记录为空")
		}
		e := ev[len(ev)-1]
		if e.TotalTokens != want {
			t.Errorf("请求记录 total_tokens = %d，应为 %d（prompt=%d completion=%d）",
				e.TotalTokens, want, e.PromptTokens, e.CompletionTokens)
		}
	}
	check(h, 165)
	check(h2, 165)
}

// 上游不报 token（实测就是这样）：请求记录仍要有量级，但必须标成估算——
// 面板加 ~ 前缀，用量台账不许进估算值。
func TestRequestLogEstimatesTokensWhenUpstreamSilent(t *testing.T) {
	h, _ := newFakeHandler(t) // fake.usage 为 nil：上游一帧 token 都不报
	rec := post(t, h, "/v1/chat/completions",
		`{"model":"gpt-5-5","messages":[{"role":"user","content":"在吗"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	ev := h.cfg.RequestLog.Snapshot().Recent
	if len(ev) == 0 {
		t.Fatal("请求记录为空")
	}
	e := ev[len(ev)-1]
	if !e.TokensEstimated {
		t.Errorf("上游没报 token，记录却没标估算: %+v", e)
	}
	if e.TotalTokens <= 0 || e.CompletionTokens <= 0 {
		t.Errorf("估算没落进请求记录: total=%d completion=%d", e.TotalTokens, e.CompletionTokens)
	}
	if e.HasCredit {
		t.Errorf("估算的 token 不该算积分: credit=%v", e.Credit)
	}
}

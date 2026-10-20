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
	lastBody map[string]any
	lastReq  *http.Request
}

func (f *fakeTabbit) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/sign-key", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "test-sign-key")
	})
	mux.HandleFunc("/panel/session", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"chat_session_id":"sess-1"}`)
	})
	mux.HandleFunc("/api/v1/chat/completion", func(w http.ResponseWriter, r *http.Request) {
		f.lastReq = r
		body, _ := io.ReadAll(r.Body)
		f.lastBody = map[string]any{}
		_ = json.Unmarshal(body, &f.lastBody)
		w.Header().Set("Content-Type", "text/event-stream")
		flush := func() {
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
		frame := func(event, data string) {
			_, _ = io.WriteString(w, "event: "+event+"\ndata: "+data+"\n\n")
			flush()
		}
		frame("ready", `{}`)
		if f.failCode != 0 {
			frame("error", `{"code":492,"message":"欢迎使用 Tabbit 浏览器"}`)
			return
		}
		if f.thinking != "" {
			frame("thinking", `{"reasoning_content":`+quote(f.thinking)+`}`)
		}
		for _, piece := range f.reply {
			frame("message_chunk", `{"content":`+quote(piece)+`}`)
		}
		frame("message_finish", `{}`)
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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	return mux
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
	// 上游只认 display_name，发 slug 会被静默换模型。
	if got := fake.lastBody["selected_model"]; got != "GPT-5.5" {
		t.Errorf("selected_model = %v，应为目录里的 display_name", got)
	}
	if got := fake.lastBody["chat_session_id"]; got != "sess-1" {
		t.Errorf("chat_session_id = %v", got)
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
	if prompt, _ := fake.lastBody["content"].(string); !strings.Contains(prompt, "<tool_call>") {
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

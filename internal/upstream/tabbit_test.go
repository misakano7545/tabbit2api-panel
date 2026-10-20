package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/misakano7545/tabbit2api-panel/internal/auth"
)

// 目录：上游只认 display_name；对外 id 是它的 slug。
func TestRefreshModels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/sign-key", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "test-sign-key")
	})
	mux.HandleFunc("/proxy/v1/model_config/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"models":[{"display_name":"GPT-5.5","model_access_type":"free_unlimited",
			"supports_images":true,"support_thinking":true,"context_window":200000,"max_tokens":32000,
			"display_multiplier":1}]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := New()
	client.SetHosts(srv.URL, srv.URL)
	account := &auth.Auth{Data: auth.Data{Token: "cookie-token", UID: "u-1", Realm: auth.RealmIntl}}
	models, err := client.RefreshModels(account, true)
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	var got *ModelInfo
	for i := range models {
		if models[i].ID == "gpt-5-5" {
			got = &models[i]
		}
	}
	if got == nil || got.Name != "GPT-5.5" || got.ContextWindow != 200000 || got.MaxOutput != 32000 {
		t.Fatalf("目录 = %+v", models)
	}
	if mi, ok := client.ResolveModel("gpt-5-5", ""); !ok || mi.UpstreamKey != "GPT-5.5" {
		t.Errorf("ResolveModel 命中失败: %+v ok=%v", mi, ok)
	}
	// 裸名要能跨站命中（客户端不知道自己在哪个站）。
	if mi, ok := client.ResolveModel("GPT-5.5", auth.RealmCN); !ok || mi.Realm != auth.RealmIntl {
		t.Errorf("跨站兜底失败: %+v ok=%v", mi, ok)
	}
}

// 工具协议：声明了 tools 才追加说明；标记要被还原成调用，正文不能吞。
func TestToolScanner(t *testing.T) {
	scan := newToolScanner(map[string]bool{"lookup": true})
	var text string
	var calls []ToolCall
	for _, piece := range []string{"前缀", "<tool_", `call>{"name":"lookup","arguments":{"q":1}}`, "</tool_", "call>", "后缀"} {
		plain, got := scan.Push(piece)
		text += plain
		calls = append(calls, got...)
	}
	plain, got := scan.Flush()
	text += plain
	calls = append(calls, got...)
	if text != "前缀后缀" {
		t.Errorf("正文 = %q", text)
	}
	if len(calls) != 1 || calls[0].Name != "lookup" || calls[0].Arguments != `{"q":1}` {
		t.Errorf("调用 = %+v", calls)
	}
}

// 名单里没有的工具名当文本，不冒充工具调用（模型可能只是在讨论这个语法）。
func TestToolScannerRejectsUnknown(t *testing.T) {
	scan := newToolScanner(map[string]bool{"lookup": true})
	plain, calls := scan.Push(`<tool_call>{"name":"exec","arguments":{}}</tool_call>`)
	if len(calls) != 0 {
		t.Fatalf("不该还原: %+v", calls)
	}
	if plain != `<tool_call>{"name":"exec","arguments":{}}</tool_call>` {
		t.Errorf("原文没透出: %q", plain)
	}
}

// 签名头：x-nonce 必须是 HMAC(key, "{ts}.{uuid}.{sha256(body)}")，x-signature 是那个 uuid。
func TestSignatureHeaders(t *testing.T) {
	headers := signatureHeaders("k", `{"a":1}`, fixedTime())
	if headers["x-timestamp"] == "" || headers["x-signature"] == "" || headers["x-nonce"] == "" {
		t.Fatalf("缺签名头: %v", headers)
	}
	again := signatureHeaders("k", `{"a":1}`, fixedTime())
	if again["x-nonce"] == headers["x-nonce"] {
		t.Error("同一 body 两次签名不该相同（nonce 随机）")
	}
	if signatureHeaders("k2", `{"a":1}`, fixedTime())["x-timestamp"] != headers["x-timestamp"] {
		t.Error("时间戳应来自时间源")
	}
}

func fixedTime() time.Time { return time.Unix(1792500000, 0) }

package upstream

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 上游模型（DeepSeek 系）会把特殊 token 混进协议：实测吐过
//
//	<tool_call>{"name":"read_file",...}</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜tool_call>
//
// 只认 </tool_call> 时整段被当正文透出，客户端拿不到 tool_calls —— Agent 循环卡死。
// 这段是线上抓到的原文，别再改回去。
func TestToolScannerAcceptsDSMLCloseTag(t *testing.T) {
	scanner := newToolScanner(map[string]bool{"read_file": true})
	raw := "继续读取日志。<tool_call>{\"name\":\"read_file\",\"arguments\":{\"path\":\"/var/log/elevator.log\"}}" +
		"</｜DSML｜parameter>\n</｜DSML｜invoke>\n</｜DSML｜tool_call>"
	plain, calls := scanner.Push(raw)
	rest, tailCalls := scanner.Flush()
	if n := len(calls) + len(tailCalls); n != 1 {
		t.Fatalf("认出的调用数 = %d，正文残留 %q", n, plain+rest)
	}
	call := append(calls, tailCalls...)[0]
	if call.Name != "read_file" || !strings.Contains(call.Arguments, "/var/log/elevator.log") {
		t.Fatalf("解析结果不对: %+v", call)
	}
	if strings.Contains(plain+rest, "DSML") {
		t.Errorf("特殊 token 漏进了正文: %q", plain+rest)
	}
}

// 标签被网络切在两帧之间时，半截前缀要留住，不能当正文吐出去。
func TestToolScannerAcrossChunks(t *testing.T) {
	scanner := newToolScanner(nil)
	var text string
	var calls []ToolCall
	for _, chunk := range []string{"查一下<tool_", "call>{\"name\":\"get_time\",\"argu", "ments\":{}}</tool_", "call>好了"} {
		plain, got := scanner.Push(chunk)
		text += plain
		calls = append(calls, got...)
	}
	plain, tail := scanner.Flush()
	text += plain
	calls = append(calls, tail...)
	if len(calls) != 1 || calls[0].Name != "get_time" {
		t.Fatalf("调用没还原出来: %+v（正文 %q）", calls, text)
	}
	if text != "查一下好了" {
		t.Errorf("正文被改写: %q", text)
	}
}

// tool 消息按 OpenAI 规范只带 tool_call_id，模型要看到工具名才知道是哪次调用的结果。
func TestBuildPromptNamesToolResult(t *testing.T) {
	spec := chatRequestSpec{Messages: []chatMessage{
		{Role: "user", Content: []byte(`"查一下"`)},
		{Role: "assistant", ToolCalls: []chatToolCall{{
			ID: "call_1", Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: "lookup_elevator", Arguments: `{"code":"DL-7749"}`},
		}}},
		{Role: "tool", ToolCallID: "call_1", Content: []byte(`"结论：合格"`)},
	}}
	out, err := buildPrompt(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[工具结果 lookup_elevator]") {
		t.Errorf("工具结果没标上工具名:\n%s", out)
	}
}

// tool_choice 是客户端的硬要求，模型看不到它，只能靠协议说明里那句话表达。
func TestToolProtocolCarriesToolChoice(t *testing.T) {
	spec := chatRequestSpec{
		Tools: []toolSpec{{Type: "function", Function: &struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}{Name: "get_time"}}},
		ToolChoice: "required",
	}
	if out := toolProtocol(spec); !strings.Contains(out, "本次必须先调用工具") {
		t.Errorf("required 没传下去:\n%s", out)
	}
	spec.ToolChoice = "none"
	if out := toolProtocol(spec); !strings.Contains(out, "本次不要调用工具") {
		t.Errorf("none 没传下去:\n%s", out)
	}
	spec.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": "get_time"}}
	if out := toolProtocol(spec); !strings.Contains(out, "必须调用工具 get_time") {
		t.Errorf("点名工具没传下去:\n%s", out)
	}
}

// 只有思考、没有正文也没有工具调用的回合，必须当空流让调用方换号重试，
// 否则 Agent 客户端收到 content 为空的 200，循环当场停住。
func TestAggregateEmptyAnswerIsEmptyStream(t *testing.T) {
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"想想\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	_, err := Aggregate(strings.NewReader(stream), "cn:default")
	if !errors.Is(err, ErrEmptyStream) {
		t.Fatalf("err = %v，应为 ErrEmptyStream", err)
	}
}

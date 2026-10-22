// prompt.go 把 OpenAI 请求摊成一段上游能吃的 prompt。
//
// Tabbit 的 completion 只收一个 content 字符串：没有 messages 数组、没有 role、
// 没有原生工具调用。所以这里做三件事：
//
//  1. 按角色加标记（[系统指令] / [助手] / [工具结果]），让模型看得出对话结构；
//  2. 按字符预算裁剪（上游实测 ≥2 万字符必失败，且错误文案是营销语，极具误导性），
//     系统指令优先、其余从最近往回填；
//  3. 把 tools 声明编译成文本协议（见 toolcall.go 的文件头），没有工具就一个字不加。
package upstream

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// 上游单次请求的字符上限：实测 20000 字符成功、25000 必失败，预算取 16000 留余量。
const (
	promptBudget = 16000
	promptSingle = 9000
	promptHead   = 6000
	promptTail   = 2000
	systemBudget = 5000
)

const (
	truncNote   = "\n…[内容过长，中间部分已省略]…\n"
	historyNote = "[注意：更早的对话历史因长度限制已省略]"
)

// chatRequestSpec 是入站 chat.completions 请求里本包真正要用的部分。
type chatRequestSpec struct {
	Model      string        `json:"model"`
	Stream     bool          `json:"stream"`
	Messages   []chatMessage `json:"messages"`
	Tools      []toolSpec    `json:"tools"`
	Function   []toolSpec    `json:"functions"`
	ToolChoice any           `json:"tool_choice"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Name       string          `json:"name"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toolSpec 同时覆盖 OpenAI tools[].function 与 Responses 风格裸 name。
type toolSpec struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Function *struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// name 返回工具名（两种声明形状都认）。
func (t toolSpec) name() string {
	if t.Function != nil && t.Function.Name != "" {
		return t.Function.Name
	}
	return t.Name
}

// description 返回工具描述。
func (t toolSpec) description() string {
	if t.Function != nil && t.Function.Description != "" {
		return t.Function.Description
	}
	return t.Description
}

// schema 返回参数 schema 原文（缺省是空对象）。
func (t toolSpec) schema() string {
	for _, raw := range []json.RawMessage{t.InputSchema, t.Parameters} {
		if len(raw) > 0 && string(raw) != "null" {
			return string(raw)
		}
	}
	if t.Function != nil && len(t.Function.Parameters) > 0 && string(t.Function.Parameters) != "null" {
		return string(t.Function.Parameters)
	}
	return "{}"
}

// parseChatRequest 解析入站请求体；只要求 messages 非空。
func parseChatRequest(body []byte) (chatRequestSpec, error) {
	var spec chatRequestSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		return spec, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	if len(spec.Messages) == 0 {
		return spec, fmt.Errorf("messages 不能为空")
	}
	// 旧版 functions[] 与新版 tools[] 等价，合成一张表（toolcall 协议只认名字）。
	if len(spec.Tools) == 0 && len(spec.Function) > 0 {
		spec.Tools = spec.Function
	}
	return spec, nil
}

// toolsOf 返回请求声明的工具名集合（解析失败按空处理，不编造名单）。
func (s chatRequestSpec) toolsOf() map[string]bool {
	names := map[string]bool{}
	for _, t := range s.Tools {
		if name := t.name(); name != "" {
			names[name] = true
		}
	}
	return names
}

// buildPrompt 摊平消息并追加工具协议，返回送上游的 content。
func buildPrompt(spec chatRequestSpec) (string, error) {
	var systems, others []string
	// 客户端回的 tool 消息只带 tool_call_id、不带工具名（OpenAI 规范如此），
	// 但模型要靠名字对上是哪次调用的结果，所以从前面 assistant 的调用里反查。
	callNames := map[string]string{}
	for _, message := range spec.Messages {
		for _, call := range message.ToolCalls {
			if call.ID != "" && call.Function.Name != "" {
				callNames[call.ID] = call.Function.Name
			}
		}
		text := renderMessage(message, callNames)
		if text == "" {
			continue
		}
		switch message.Role {
		case "system", "developer":
			systems = append(systems, text)
		default:
			others = append(others, text)
		}
	}

	out := joinWithinBudget(systems, others)
	if protocol := toolProtocol(spec); protocol != "" {
		if out != "" {
			out += "\n\n"
		}
		out += protocol
	}
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("messages 里没有可发送的文本内容")
	}
	return out, nil
}

// renderMessage 把一条消息渲染成带角色标记的文本块。
func renderMessage(message chatMessage, callNames map[string]string) string {
	text := messageText(message.Content)
	switch message.Role {
	case "system", "developer":
		if text == "" {
			return ""
		}
		return "[系统指令]\n" + text
	case "tool", "function":
		label := "工具结果"
		name := message.Name
		if name == "" {
			name = callNames[message.ToolCallID]
		}
		if name != "" {
			label += " " + name
		}
		return "[" + label + "]\n" + text
	case "assistant":
		parts := []string{}
		if text != "" {
			parts = append(parts, text)
		}
		for _, call := range message.ToolCalls {
			parts = append(parts, renderToolCall(call))
		}
		if len(parts) == 0 {
			return ""
		}
		return "[助手]\n" + strings.Join(parts, "\n")
	default:
		if text == "" {
			return ""
		}
		return text
	}
}

// renderToolCall 把历史里的工具调用写成协议形态：模型看到自己上次是怎么调的。
func renderToolCall(call chatToolCall) string {
	args := strings.TrimSpace(call.Function.Arguments)
	if args == "" {
		args = "{}"
	}
	body, err := json.Marshal(map[string]any{"name": call.Function.Name, "arguments": json.RawMessage(args)})
	if err != nil {
		body = []byte(fmt.Sprintf(`{"name":%q,"arguments":{}}`, call.Function.Name))
	}
	return toolCallOpen + string(body) + toolCallClose
}

// messageText 把 content（字符串或分段数组）摊成纯文本。
// 图片一律如实标注「不支持」，不假装看得见。
func messageText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return strings.TrimSpace(plain)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	buf := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text", "output_text":
			if strings.TrimSpace(part.Text) != "" {
				buf = append(buf, part.Text)
			}
		case "image", "image_url", "input_image":
			buf = append(buf, "[图片：当前网关上上游只接受文本，图片未被读取]")
		}
	}
	return strings.TrimSpace(strings.Join(buf, "\n"))
}

// joinWithinBudget 先放系统指令（各自限 systemBudget），再按顺序放其余消息；
// 超预算时从最早的开始丢，并明确告知模型历史被截断。
func joinWithinBudget(systems, others []string) string {
	// 单位必须是**字符**：预算（promptBudget/promptSingle/systemBudget）都是按字符定的，
	// 用 len() 是字节数，中文一个字三字节，8k 汉字的单条消息会被判超预算整段丢掉——
	// 实测表现为模型回一句「之前的对话内容没有传过来」。
	sysText := strings.Join(systems, "\n\n")
	if utf8.RuneCountInString(sysText) > systemBudget {
		sysText = clipText(sysText, systemBudget)
	}
	used := utf8.RuneCountInString(sysText)
	room := promptBudget - used
	if room < 400 {
		room = 400
	}

	kept := make([]string, 0, len(others))
	bodyUsed := 0
	dropped := 0
	for _, block := range others {
		block = clipText(block, promptSingle)
		n := utf8.RuneCountInString(block)
		if bodyUsed+n+2 > room {
			dropped++
			continue
		}
		kept = append(kept, block)
		bodyUsed += n + 2
	}
	if dropped > 0 {
		kept = append([]string{historyNote}, kept...)
	}
	parts := []string{}
	if sysText != "" {
		parts = append(parts, sysText)
	}
	parts = append(parts, kept...)
	return strings.Join(parts, "\n\n")
}

// clipText 把单条文本裁到 limit 以内，保留首尾（中间省略）。
func clipText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	head := promptHead
	if limit-200 < head {
		head = max(1, limit/2)
	}
	tail := promptTail
	if limit-head-60 < tail {
		tail = max(0, limit-head-60)
	}
	if tail < 0 {
		tail = 0
	}
	return string(runes[:head]) + truncNote + string(runes[len(runes)-tail:])
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

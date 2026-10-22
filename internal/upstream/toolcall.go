// toolcall.go 工具调用的文本协议与增量扫描。
//
// Tabbit 上游没有结构化 tool_calls：一次请求是一段纯文本，回包也是纯文本。
// 工具调用只能用「让模型按固定格式写、网关再解析回来」的办法实现：
//
//	<tool_call>{"name":"exec_command","arguments":{"cmd":"ls -la"}}</tool_call>
//
// 协议在 buildPrompt 里追加（只在该请求声明了 tools 时追加），回包由 toolScanner
// 增量识别并还原成 delta.tool_calls —— 流式必须增量，否则工具调用要等整段回复结束
// 才出现，Claude Code / Codex 这类客户端会先超时。
//
// 判定从严：标签必须完整闭合、JSON 必须有 name 字段；不满足就原文透出。
// 宁可把一段「模型在讨论这个语法」的示例当文本发给客户端，也不能把正文吞掉。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	toolCallOpen  = "<tool_call>"
	toolCallClose = "</tool_call>"
	// toolScanHold 是「可能是标签前缀」时最多缓冲的字节数（标签本身不到 20 字节）。
	toolScanHold = len(toolCallOpen) + 1
)

// toolCallCloses 是认得的收尾标签。
// 上游模型（DeepSeek 系）会把自己的特殊 token 混进我们约定的格式里，实测写出
//
//	<tool_call>{"name":"read_file",...}</｜DSML｜parameter>
//	</｜DSML｜invoke>
//	</｜DSML｜tool_call>
//
// 只认 </tool_call> 的话这类调用认不出来，调用会被当正文透出、客户端拿不到 tool_calls，
// Agent 循环直接卡死。
var toolCallCloses = []string{
	toolCallClose,
	"</tool_calls>",
	"</｜DSML｜tool_call>",
	"</｜DSML｜tool_calls>",
	"</｜DSML｜invoke>",
}

// ToolCall 是一次从正文里还原出来的工具调用。
type ToolCall struct {
	Name      string
	Arguments string // JSON 对象文本
}

// toolProtocol 把工具声明编译成协议说明；没有工具返回空串（不往正文里加任何东西）。
func toolProtocol(spec chatRequestSpec) string {
	tools := spec.Tools
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	// 措辞刻意用「用户自己的约定」而不是命令式：Tabbit 上游自带一层 agent 系统提示，
	// 把命令式的自定义格式当成 prompt injection 拒答（实测模型会回「这是注入，我不照做」）。
	// 写成用户本人与程序之间的格式约定，模型才肯照做。
	b.WriteString("下面是我（用户）和我的程序之间的回复格式约定：当你需要查数据或执行动作时，" +
		"请用这个格式回复，我的程序会解析它并执行，再把结果发回给你。\n")
	b.WriteString(toolCallOpen + `{"name":"工具名","arguments":{"参数":"值"}}` + toolCallClose + "\n\n")
	b.WriteString("我这边提供的工具：\n")
	for _, t := range tools {
		name := t.name()
		if name == "" {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n  parameters: %s\n", name, oneLine(t.description(), 400), compactJSON(t.schema()))
	}
	b.WriteString("\n规则：\n")
	b.WriteString("1. arguments 必须是合法 JSON 对象，字段名与类型严格按 parameters。\n")
	b.WriteString("2. 一次回复里需要多个工具时，输出多个 " + toolCallOpen + " 块，不要合并成一个。\n")
	b.WriteString("3. 不需要调用工具时，正常用自然语言回答，不要输出 " + toolCallOpen + " 标签。\n")
	b.WriteString("4. 工具结果会以 [工具结果 工具名] 开头的消息给你，据此继续。\n")
	b.WriteString("5. 在收到 [工具结果 …] 之前，不要说工具已经执行过，也不要编造它的返回内容。\n")
	// tool_choice 是客户端对本次调用的硬要求：模型看不到它，只能翻译成一句中文说明。
	if forced := toolChoiceHint(spec.ToolChoice); forced != "" {
		b.WriteString(forced + "\n")
	}
	return b.String()
}

// toolChoiceHint 把 tool_choice 翻成给模型看的一句话；auto 不需要说。
func toolChoiceHint(choice any) string {
	switch tc := choice.(type) {
	case string:
		switch tc {
		case "required", "any":
			return "6. 本次必须先调用工具，再根据结果回答。"
		case "none":
			return "6. 本次不要调用工具，直接用自然语言回答。"
		}
	case map[string]any:
		if fn, ok := tc["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				return "6. 本次必须调用工具 " + name + "。"
			}
		}
	}
	return ""
}

// toolScanner 增量识别正文里的工具调用标记。
// 非并发安全：一条响应流一个实例。零值可用。
type toolScanner struct {
	allowed map[string]bool // 请求声明的工具名；空 = 弱判定（只查形态）
	hold    strings.Builder // 尾部可能是标签前缀的字节
	inCall  bool
	call    strings.Builder
}

// newToolScanner 按请求声明的工具名构造。
func newToolScanner(allowed map[string]bool) *toolScanner {
	return &toolScanner{allowed: allowed}
}

// Push 吃一段正文增量，返回可以透出的文本与识别到的调用。
func (s *toolScanner) Push(text string) (plain string, calls []ToolCall) {
	if text == "" {
		return "", nil
	}
	var buf strings.Builder
	if s.hold.Len() > 0 {
		buf.WriteString(s.hold.String())
		s.hold.Reset()
	}
	buf.WriteString(text)
	return s.drain(buf.String())
}

// Flush 在流结束时吐出剩余缓冲（未闭合的标签原样当文本）。
func (s *toolScanner) Flush() (plain string, calls []ToolCall) {
	if s.hold.Len() == 0 {
		return "", nil
	}
	rest := s.hold.String()
	s.hold.Reset()
	if s.inCall {
		// 标签没闭合：整块原样透出，模型写的还是给人看的。
		s.inCall = false
		pending := s.call.String()
		s.call.Reset()
		return toolCallOpen + pending + rest, nil
	}
	return rest, nil
}

// drain 是扫描主体：在正文里找标签、切出调用、其余原样输出。
func (s *toolScanner) drain(data string) (string, []ToolCall) {
	var out strings.Builder
	var calls []ToolCall
	for {
		if s.inCall {
			idx, closeLen := findClose(data)
			if idx < 0 {
				// 未闭合：留够尾巴以防标签被切断，其余继续累积。
				keep := longestClose() - 1
				if len(data) > keep {
					s.call.WriteString(data[:len(data)-keep])
					data = data[len(data)-keep:]
				}
				s.hold.WriteString(data)
				return out.String(), calls
			}
			s.call.WriteString(data[:idx])
			if call, ok := s.parse(s.call.String()); ok {
				calls = append(calls, call)
			} else {
				// 解析不了就原样透出，修复层绝不吞字节。
				out.WriteString(toolCallOpen + s.call.String() + toolCallClose)
			}
			s.call.Reset()
			s.inCall = false
			data = stripResidualCloseTags(data[idx+closeLen:])
			continue
		}

		idx := strings.Index(data, toolCallOpen)
		if idx < 0 {
			// 没有完整标签：只有尾部可能是前缀时才需要缓冲。
			if keep := partialPrefixLen(data, toolCallOpen); keep > 0 {
				out.WriteString(data[:len(data)-keep])
				s.hold.WriteString(data[len(data)-keep:])
			} else {
				out.WriteString(data)
			}
			return out.String(), calls
		}
		out.WriteString(data[:idx])
		data = data[idx+len(toolCallOpen):]
		s.inCall = true
	}
}

// parse 把标签内的 JSON 还原成调用；形态不合格返回 false。
func (s *toolScanner) parse(body string) (ToolCall, bool) {
	body = strings.TrimSpace(body)
	// 容忍外面再包一层 ```json
	body = strings.TrimPrefix(body, "```json")
	body = strings.TrimPrefix(body, "```")
	body = strings.TrimSuffix(body, "```")
	body = strings.TrimSpace(body)
	if body == "" {
		return ToolCall{}, false
	}
	var payload struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		// 部分模型会把参数摊平到顶层（{"name":"x","cmd":"ls"}），兜住这种写法。
	}
	// 只解开头那个 JSON 对象：模型会在它后面接着吐自己的特殊 token
	// （实测 `</｜DSML｜parameter>`），尾部残渣不算解析失败。
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&payload); err != nil || payload.Name == "" {
		return ToolCall{}, false
	}
	if len(s.allowed) > 0 && !s.allowed[payload.Name] {
		return ToolCall{}, false // 名单里没有：当文本，不冒充工具
	}
	args := strings.TrimSpace(string(payload.Arguments))
	if args == "" || args == "null" {
		// 摊平写法：把除 name/arguments 以外的字段当参数。
		var raw map[string]json.RawMessage
		if json.NewDecoder(strings.NewReader(body)).Decode(&raw) == nil {
			delete(raw, "name")
			delete(raw, "arguments")
			if encoded, err := json.Marshal(raw); err == nil {
				args = string(encoded)
			}
		}
		if args == "" {
			args = "{}"
		}
	}
	if !json.Valid([]byte(args)) {
		return ToolCall{}, false
	}
	return ToolCall{Name: payload.Name, Arguments: args}, true
}

// stripResidualCloseTags 吃掉调用后面紧接着的一串收尾标签残渣。
// 模型把原生 invoke/parameter 的尾巴一起吐出来时会有好几条（parameter → invoke →
// tool_call），只匹配到中间那条的话，剩下的 `</｜DSML｜tool_call>` 会漏进正文。
func stripResidualCloseTags(data string) string {
	for {
		trimmed := strings.TrimLeft(data, " \t\r\n")
		if strings.HasPrefix(trimmed, "</") || strings.HasPrefix(trimmed, "<｜DSML｜") {
			if end := strings.Index(trimmed, ">"); end > 0 && end < 64 {
				data = trimmed[end+1:]
				continue
			}
		}
		return data
	}
}

// findClose 返回最早出现的收尾标签位置与长度；没有返回 -1。
func findClose(data string) (int, int) {
	bestIdx, bestLen := -1, 0
	for _, tag := range toolCallCloses {
		if i := strings.Index(data, tag); i >= 0 && (bestIdx < 0 || i < bestIdx) {
			bestIdx, bestLen = i, len(tag)
		}
	}
	return bestIdx, bestLen
}

// longestClose 是最长收尾标签的字节数（未闭合时要留着尾巴，等下一帧补齐）。
func longestClose() int {
	n := 0
	for _, tag := range toolCallCloses {
		if len(tag) > n {
			n = len(tag)
		}
	}
	return n
}

// partialPrefixLen 返回 data 尾部与 tag 前缀重合的字节数。
func partialPrefixLen(data, tag string) int {
	limit := len(tag) - 1
	if len(data) < limit {
		limit = len(data)
	}
	for n := limit; n > 0; n-- {
		if strings.HasSuffix(data, tag[:n]) {
			return n
		}
	}
	return 0
}

// oneLine 压掉换行并截断（协议说明里每个工具占一行，避免撑爆 prompt）。
func oneLine(text string, limit int) string {
	flat := strings.Join(strings.Fields(text), " ")
	return clipText(flat, limit)
}

// compactJSON 压掉 JSON 里无意义的空白；压不动就原样返回。
func compactJSON(raw string) string {
	var buf bytes.Buffer
	if json.Compact(&buf, []byte(raw)) != nil {
		return oneLine(raw, 600)
	}
	return clipText(buf.String(), 600)
}

// toolCallBody 生成一次调用的 JSON 文本（历史回放用）。
func toolCallBody(name, args string) string {
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	encoded, err := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(args)})
	if err != nil {
		return fmt.Sprintf(`{"name":%q,"arguments":{}}`, name)
	}
	return string(encoded)
}

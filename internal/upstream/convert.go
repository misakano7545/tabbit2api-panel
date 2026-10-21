// convert.go 把 Tabbit 的 SSE 事件翻译成 OpenAI chunk 的 SSE 字节流。
//
// 这样做的原因：基线的流式/非流式管道（sse.go 的 StreamWithError / Aggregate、
// server 层的三协议适配）全部只认「OpenAI chunk 帧」。让本文件实现一个
// io.ReadCloser 做翻译，那 600 多行管道一行不用改，三套入口自然跟着通。
//
// 上游是 v3 房间协议（POST /api/v3/chat/rooms/{id}/join 的事件流），
// 每帧形如 `event: <type>` + `data: {"event_type":…,"payload":{…},"created_at":…}`：
//
//	snapshot_begin / snapshot_end / active_runs_snapshot / message_history  订阅快照，忽略
//	run_queued / run_started / user_message_created                          控制帧
//	assistant_message_started                                                控制帧
//	event_message_chunk  payload.chunk_type=thinking → chunk_payload.reasoning_content
//	assistant_message_delta  payload.delta（或 chunk_payload.content）       正文增量
//	assistant_message_finished / run_completed                               结束
//	run_failed / error                                                       流内错误
//	room_reset_required / session_title_updated                              忽略
//
// 正文里的工具调用标记由 toolScanner 增量剥离，转成 delta.tool_calls。
package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/misakano7545/tabbit2api-panel/internal/usage"
)

// convertReader 是交给 server 层的「上游 OpenAI 流」。
type convertReader struct {
	src     io.ReadCloser
	br      *bufio.Reader
	spec    chatRequestSpec
	model   string
	id      string
	created int64
	scan    *toolScanner

	out      bytes.Buffer // 已翻译、待交给调用方的字节
	done     bool         // 已写出 [DONE]
	started  bool         // 已发出首个角色帧
	toolIdx  int          // 已还原的调用序号
	finished bool         // 收到结束事件
	err      *Error       // peek 阶段发现的上游错误（尚未交给调用方）
	// pendingUsage 是 usage 事件先于结束帧到达时的暂存。
	pendingUsage map[string]any
	// promptTokens 是本请求送上游的 prompt 估算（runChat 填）。上游一帧 token 都不报，
	// 收尾时靠它与 gen 的字数补一个带 estimated 标记的 usage。
	promptTokens int64
	gen          strings.Builder // 已产出的正文/思考，用于估算 completion
	// runID 是本次投递的 run：join 流里别的并发 run 的帧一律丢弃。
	runID string
}

func newConvertReader(src io.ReadCloser, spec chatRequestSpec) *convertReader {
	return &convertReader{
		src:     src,
		br:      bufio.NewReaderSize(src, 64*1024),
		spec:    spec,
		model:   spec.Model,
		id:      "chatcmpl-" + strings.ReplaceAll(randomUUID(), "-", "")[:24],
		created: time.Now().Unix(),
		scan:    newToolScanner(spec.toolsOf()),
	}
}

// peek 预读一帧：上游常用 HTTP 200 + error 事件表达 492/409，
// 只看状态码会把它记成成功、账号也没法换。返回非 nil 表示这次请求失败了。
func (c *convertReader) peek() (*Error, error) {
	if c.model == "" {
		c.model = DefaultConfigName
	}
	if err := c.pump(); err != nil {
		return nil, err
	}
	if c.err != nil {
		return c.err, nil
	}
	return nil, nil
}

// Read 实现 io.Reader：先吐已翻译的字节，空了就翻译下一帧。
func (c *convertReader) Read(p []byte) (int, error) {
	for c.out.Len() == 0 {
		if c.done {
			return 0, io.EOF
		}
		if err := c.pump(); err != nil {
			if c.err != nil {
				return 0, c.err
			}
			return 0, err
		}
	}
	return c.out.Read(p)
}

// Close 关闭上游。
func (c *convertReader) Close() error { return c.src.Close() }

// pump 读一帧并翻译；读到流尾时补结束帧与 [DONE]。
func (c *convertReader) pump() error {
	name, data, err := c.nextEvent()
	if err != nil {
		if err != io.EOF {
			return err
		}
		c.finish("")
		return nil
	}
	return c.translate(name, data)
}

// nextEvent 读一个完整 SSE 帧（event: + data:），返回事件名与 data 文本。
func (c *convertReader) nextEvent() (string, string, error) {
	var event string
	var data []string
	for {
		line, err := c.br.ReadString('\n')
		if err != nil && line == "" {
			if len(data) > 0 || event != "" {
				return event, strings.Join(data, "\n"), nil
			}
			return "", "", err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if len(data) > 0 || event != "" {
				return event, strings.Join(data, "\n"), nil
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line, "data:"))
		}
		if err != nil {
			if len(data) > 0 || event != "" {
				return event, strings.Join(data, "\n"), nil
			}
			return "", "", err
		}
	}
}

// translate 把一帧上游事件翻成零到多个 OpenAI chunk。
func (c *convertReader) translate(event, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[DONE]" {
		return nil
	}
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil {
		return nil // 上游偶尔发心跳/空帧：忽略，不报错
	}
	// 少数站点把事件名放在 data.type 里。
	if event == "" {
		event = text(payload, "type", "event", "event_type")
	}
	// 用量帧可能在任何事件里（v3 的 usage/run_completed 都带 payload 外壳）：
	// 先吸出 token 数再分派，否则它会掉进下面的忽略表，请求记录里就全是「—」。
	inner, hasInner := payload["payload"].(map[string]any)
	c.absorbUsage(payload, inner, chunkPayloadOf(inner))
	// v3 房间事件：正文/思考/错误都在 payload 里，且要按 run_id 过滤。
	if hasInner {
		if run := text(inner, "run_id"); run != "" && c.runID != "" && run != c.runID {
			return nil // 同一房间里的其他 run，不串话
		}
		switch event {
		case "event_message_chunk":
			kind := text(inner, "chunk_type")
			chunk, _ := inner["chunk_payload"].(map[string]any)
			switch kind {
			case "thinking":
				if delta := text(chunk, "reasoning_content", "content"); delta != "" {
					c.gen.WriteString(delta)
					c.ensureRole()
					c.writeChunk(map[string]any{"reasoning_content": delta}, nil)
				}
			case "thinking_finished":
				// 无正文
			case "error":
				msg := text(chunk, "message", "error_message", "detail")
				c.err = streamErrorOf(0, msg, raw)
				c.finish("")
				return nil
			default:
				// message_chunk 变体：正文也可能塞在这里
				if delta := text(chunk, "content"); delta != "" {
					c.pushContent(delta)
				}
			}
			return nil
		case "assistant_message_delta":
			delta := text(inner, "delta")
			if delta == "" {
				if chunk, ok := inner["chunk_payload"].(map[string]any); ok {
					delta = text(chunk, "content")
				}
			}
			if delta != "" {
				c.pushContent(delta)
			}
			return nil
		case "assistant_message_finished":
			// 不在这里收尾：v3 的 token_usage 随后面的 run_completed 才到，先 finish 会把
			// 结束帧写早、usage 落空（请求记录整列「—」）。收尾交给 run_completed /
			// run_failed / error / 流 EOF（finish 幂等，缺结束事件也有 EOF 兜底）。
			return nil
		case "run_completed":
			c.finish("")
			return nil
		case "run_failed", "run_cancelled":
			msg := text(inner, "message", "error_message", "reason", "detail")
			if msg == "" {
				msg = text(inner, "status")
			}
			c.err = streamErrorOf(0, msg, raw)
			c.finish("")
			return nil
		case "snapshot_begin", "snapshot_end", "message_history", "active_runs_snapshot",
			"run_queued", "run_started", "user_message_created", "assistant_message_started",
			"session_title_updated", "room_reset_required":
			return nil
		}
	}
	switch event {
	case "thinking":
		if delta := text(payload, "reasoning_content", "content", "text"); delta != "" {
			c.gen.WriteString(delta)
			c.ensureRole()
			c.writeChunk(map[string]any{"reasoning_content": delta}, nil)
		}
	case "message_chunk", "message", "delta", "":
		content := text(payload, "content", "text", "delta")
		if content == "" {
			// 有的帧把正文放在 delta.content 里。
			if inner, ok := payload["delta"].(map[string]any); ok {
				content = text(inner, "content", "text")
			}
		}
		if content != "" {
			c.ensureRole()
			plain, calls := c.scan.Push(content)
			if plain != "" {
				c.writeChunk(map[string]any{"content": plain}, nil)
			}
			for _, call := range calls {
				c.writeToolCall(call)
			}
		}
	case "usage":
		// 上游 usage 与 OpenAI 字段名不同，由 absorbUsage 统一翻译（见该函数）。
		c.absorbUsage(payload)
	case "error":
		msg := text(payload, "message", "msg", "error_message", "displayTips")
		if msg == "" {
			if inner, ok := payload["error"].(map[string]any); ok {
				msg = text(inner, "message")
			}
		}
		code, _ := intOf(payload["code"])
		c.err = streamErrorOf(code, msg, raw)
		c.finish("")
		return nil
	case "message_finish", "finish", "close":
		reason := text(payload, "finish_reason", "stop_reason")
		c.finish(reason)
	case "cancel":
		// 用户侧取消：当正常结束，不再补正文。
		c.finish("stop")
	case "ready", "ttft", "title", "switch_model", "message_start", "thinking_finished", "risky":
		// 控制帧，无正文。
	}
	return nil
}

// pendingUsage 是 usage 事件先于结束帧到达时的暂存。

// absorbUsage 从一帧的几个可能载体里吸出用量。上游把 token 数放在哪一层不稳定
// （顶层的 usage 帧、v3 信封里的 payload、chunk_payload，甚至跟着 run_completed 一起来），
// 所以按载体顺序找一遍，找到就以最后一次为准。
func (c *convertReader) absorbUsage(maps ...map[string]any) {
	for _, m := range maps {
		if u := usageOf(m); u != nil {
			c.pendingUsage = u
		}
	}
}

// usageOf 把上游的 token 字段翻成 OpenAI 键名：新接口用 input/output_tokens，
// 旧接口用 prompt/completion_tokens；两者也可能整体嵌在 token_usage 子对象里
// （v3 实测：run_completed.payload.token_usage）。一个都没报就是「没观测到」，
// 返回 nil 不编 0。
func usageOf(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	if nested, ok := m["token_usage"].(map[string]any); ok {
		if u := usageOf(nested); u != nil {
			return u
		}
	}
	usage := map[string]any{}
	if n, ok := asInt64(m["input_tokens"]); ok {
		usage["prompt_tokens"] = n
	}
	if n, ok := asInt64(m["prompt_tokens"]); ok {
		usage["prompt_tokens"] = n
	}
	if n, ok := asInt64(m["output_tokens"]); ok {
		usage["completion_tokens"] = n
	}
	if n, ok := asInt64(m["completion_tokens"]); ok {
		usage["completion_tokens"] = n
	}
	if len(usage) == 0 {
		return nil
	}
	prompt, _ := asInt64(usage["prompt_tokens"])
	completion, _ := asInt64(usage["completion_tokens"])
	usage["total_tokens"] = prompt + completion
	return usage
}

// chunkPayloadOf 取 v3 事件里的 chunk_payload（正文增量与用量都可能塞在这一层）。
func chunkPayloadOf(inner map[string]any) map[string]any {
	if inner == nil {
		return nil
	}
	chunk, _ := inner["chunk_payload"].(map[string]any)
	return chunk
}

// streamErrorOf 把上游 error 事件分类成 *Error。
func streamErrorOf(code int, msg, raw string) *Error {
	kind := classifyErrorText(raw)
	if msg == "" {
		msg = messageOf(kind)
	}
	status := 502
	switch kind {
	case ErrPlanLimit:
		status = 429 // 权益/额度：让客户端别立刻重试
	case ErrSoftRate:
		status = 429
	case ErrSessionDead:
		status = 401
	case ErrBusy:
		status = 409
	}
	if code == 492 {
		kind, status = ErrPlanLimit, 429
		if msg == "" || strings.Contains(msg, "欢迎使用") {
			msg = "当前账号没有该模型的权限（上游 492）：Tabbit 需要 Pro 或该模型今日额度已用完"
		}
	}
	return &Error{Kind: kind, Status: status, Msg: msg, Busy: kind == ErrBusy}
}

// pushContent 处理一段正文增量：先过工具标记扫描，再写 chunk。
func (c *convertReader) pushContent(content string) {
	if content == "" {
		return
	}
	c.gen.WriteString(content)
	c.ensureRole()
	plain, calls := c.scan.Push(content)
	if plain != "" {
		c.writeChunk(map[string]any{"content": plain}, nil)
	}
	for _, call := range calls {
		c.writeToolCall(call)
	}
}

// ensureRole 在首个正文帧前发一次角色帧（OpenAI 客户端靠它判定流已开始）。
func (c *convertReader) ensureRole() {
	if c.started {
		return
	}
	c.started = true
	c.writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)
}

// writeChunk 拼一帧 OpenAI chunk 写进待输出缓冲。
func (c *convertReader) writeChunk(delta map[string]any, finish *string) {
	frame := map[string]any{
		"id":      c.id,
		"object":  "chat.completion.chunk",
		"created": c.created,
		"model":   c.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	if c.pendingUsage != nil && finish != nil {
		frame["usage"] = c.pendingUsage
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		return
	}
	c.out.WriteString("data: ")
	c.out.Write(raw)
	c.out.WriteString("\n\n")
}

// writeToolCall 把一次还原出来的调用写成 delta.tool_calls。
// index 写成 float64：下游累积器按 float64 读，写 int 会静默丢字段。
func (c *convertReader) writeToolCall(call ToolCall) {
	c.writeChunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": float64(c.toolIdx),
		"id":    fmt.Sprintf("call_%d_%d", c.created, c.toolIdx),
		"type":  "function",
		"function": map[string]any{
			"name":      call.Name,
			"arguments": call.Arguments,
		},
	}}}, nil)
	c.toolIdx++
}

// finish 收尾：吐出扫描器剩余缓冲、结束帧与 [DONE]；幂等。
func (c *convertReader) finish(reason string) {
	if c.done {
		return
	}
	c.done = true
	if plain, calls := c.scan.Flush(); plain != "" || len(calls) > 0 {
		c.ensureRole()
		if plain != "" {
			// 不再计入 c.gen：这段正文早已随 pushContent 计入，重复加会虚高 completion。
			c.writeChunk(map[string]any{"content": plain}, nil)
		}
		for _, call := range calls {
			c.writeToolCall(call)
		}
	}
	if reason == "" {
		reason = "stop"
	}
	if c.toolIdx > 0 {
		reason = "tool_calls"
	}
	// 上游报多少用多少；一个数都没报才估算，并打 estimated 标记（见 usage.EstimateTokens）。
	if c.pendingUsage == nil {
		ct := int64(usage.EstimateTokens([]rune(c.gen.String())))
		c.pendingUsage = map[string]any{
			"prompt_tokens":     c.promptTokens,
			"completion_tokens": ct,
			"total_tokens":      c.promptTokens + ct,
			"estimated":         true,
		}
	}
	c.writeChunk(map[string]any{}, &reason)
	c.out.WriteString("data: [DONE]\n\n")
}

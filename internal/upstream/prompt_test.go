package upstream

import (
	"strings"
	"testing"
)

// 中文长消息必须按**字符**算预算：早先用 len()（字节）比较，8k 汉字的单条消息被判
// 超预算整段丢掉，模型只收到一行「历史已省略」的提示，回一句「内容没有传过来」。
func TestBuildPromptKeepsLongChineseMessage(t *testing.T) {
	for _, n := range []int{1000, 4000, 8000} {
		body := strings.Repeat("电梯", n/2) + "【编号 DL-7749】问：编号是什么？"
		spec := chatRequestSpec{Messages: []chatMessage{{Role: "user", Content: []byte(`"` + body + `"`)}}}
		out, err := buildPrompt(spec)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !strings.Contains(out, "DL-7749") {
			t.Errorf("n=%d：正文被丢了，prompt 只剩 %d 字", n, len([]rune(out)))
		}
		if got := len([]rune(out)); got < n {
			t.Errorf("n=%d：prompt 只有 %d 字，正文没被完整送上", n, got)
		}
	}
}

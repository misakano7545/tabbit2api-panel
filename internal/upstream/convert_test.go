package upstream

import (
	"io"
	"strings"
	"testing"
)

// 上游一个 token 都不报（实抓的 v3 帧形状）时，收尾必须给一份带 estimated 标记的
// 估算 usage——否则请求记录整列「—」，客户端也拿不到任何 token 数。
func TestFinishEstimatesUsageWhenUpstreamSilent(t *testing.T) {
	src := strings.NewReader(
		`event: assistant_message_started` + "\n" + `data: {"event_type":"assistant_message_started","payload":{"run_id":"r1"}}` + "\n\n" +
			`event: assistant_message_delta` + "\n" + `data: {"event_type":"assistant_message_delta","payload":{"run_id":"r1","delta":"收到"}}` + "\n\n" +
			`event: run_completed` + "\n" + `data: {"event_type":"run_completed","payload":{"run_id":"r1","status":"COMPLETED"}}` + "\n\n")
	c := newConvertReader(io.NopCloser(src), chatRequestSpec{})
	c.runID = "r1"
	c.promptTokens = 6
	out, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	if !strings.Contains(body, `"estimated":true`) {
		t.Fatalf("收尾帧没有估算 usage: %s", body)
	}
	if !strings.Contains(body, `"prompt_tokens":6`) {
		t.Errorf("prompt 估算没带上: %s", body)
	}
	if !strings.Contains(body, `"completion_tokens":`) {
		t.Errorf("completion 估算没带上: %s", body)
	}
}

// 上游真给了 token 时原样透传，不许盖成估算（estimated 只属于「上游没报」这一种情况）。
func TestUpstreamUsageWins(t *testing.T) {
	src := strings.NewReader(
		`event: assistant_message_delta` + "\n" + `data: {"event_type":"assistant_message_delta","payload":{"run_id":"r1","delta":"收到"}}` + "\n\n" +
			`event: run_completed` + "\n" + `data: {"event_type":"run_completed","payload":{"run_id":"r1","status":"COMPLETED","token_usage":{"input_tokens":120,"output_tokens":45}}}` + "\n\n")
	c := newConvertReader(io.NopCloser(src), chatRequestSpec{})
	c.runID = "r1"
	c.promptTokens = 6
	out, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	body := string(out)
	if strings.Contains(body, `"estimated"`) {
		t.Errorf("上游报了 token 却标了估算: %s", body)
	}
	if !strings.Contains(body, `"prompt_tokens":120`) || !strings.Contains(body, `"total_tokens":165`) {
		t.Errorf("上游 usage 没透传: %s", body)
	}
}

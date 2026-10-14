package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"
)

// WriteSSE 把 ChatResponse 合成为 OpenAI SSE 流写入。
// 上游以非流式调用（structured），此处在完整解析后再合成 SSE，
// 因此响应头在拿到上游结果后才提交：首字节前的上游失败已在内部重试/换号。
func WriteSSE(w http.ResponseWriter, resp *ChatResponse, includeUsage bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	if len(resp.UnsupportedTools) > 0 {
		w.Header().Set("X-OrcaTerm-Unsupported-Tools", joinComma(resp.UnsupportedTools))
	}
	if resp.UpstreamAction != "" {
		w.Header().Set("X-OrcaTerm-Action", resp.UpstreamAction)
	}
	flusher, _ := w.(http.Flusher)

	id := newID("chatcmpl-")
	created := time.Now().Unix()
	send := func(chunk *StreamChunk) {
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	// role 首块
	send(&StreamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: resp.Model,
		Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{Role: "assistant"}}}})

	// 文本分块
	for _, piece := range splitText(resp.Text, 60) {
		send(&StreamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: resp.Model,
			Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{Content: piece}}}})
	}

	// 工具调用（单块发出）
	if len(resp.ToolCalls) > 0 {
		tc := resp.ToolCalls[0]
		tcOut := ToolCall{ID: tc.ID, Type: "function"}
		tcOut.Function.Name = tc.Function.Name
		tcOut.Function.Arguments = tc.Function.Arguments
		send(&StreamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: resp.Model,
			Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{ToolCalls: []ToolCall{tcOut}}}}})
	}

	// 结束块
	fr := resp.FinishReason
	send(&StreamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: resp.Model,
		Choices: []StreamChoice{{Index: 0, Delta: StreamDelta{}, FinishReason: &fr}}})

	// usage 块（可选）
	if includeUsage {
		u := resp.Usage
		send(&StreamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: resp.Model,
			Choices: []StreamChoice{}, Usage: &u})
	}

	fmt.Fprintf(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// splitText 按 rune 切分文本。
func splitText(s string, n int) []string {
	if s == "" {
		return nil
	}
	var out []string
	for len(s) > 0 {
		i := 0
		count := 0
		for i < len(s) && count < n {
			_, size := utf8.DecodeRuneInString(s[i:])
			i += size
			count++
		}
		out = append(out, s[:i])
		s = s[i:]
	}
	return out
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

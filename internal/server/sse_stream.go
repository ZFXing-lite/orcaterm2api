package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/ZFXing-lite/orcaterm2api/internal/relay"
)

// typewriterSize 每小片的字符数；上游单块可能很大（整段一坨），
// 下游按此粒度重切并加微延迟产生打字机观感。
const typewriterSize = 16

// typewriterGap 小片间延迟；末片不延迟以减少总时长损耗。
const typewriterGap = 15 * time.Millisecond

// splitForTypewriter 按 rune 安全切块（不劈多字节字符）。
func splitForTypewriter(s string) []string {
	runes := []rune(s)
	if len(runes) <= typewriterSize {
		return []string{s}
	}
	var out []string
	for i := 0; i < len(runes); i += typewriterSize {
		end := i + typewriterSize
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}

// emitTypewriter 把一整块可见文本按打字机节奏逐片下发。
func emitTypewriter(emit func(string), piece string) {
	parts := splitForTypewriter(piece)
	for i, part := range parts {
		emit(part)
		if i < len(parts)-1 {
			time.Sleep(typewriterGap)
		}
	}
}

// sseHead 统一 SSE 响应头（首个下发动作时才提交，保留换号重试窗口）。
func sseHead(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

// openAIStreamSink OpenAI 兼容流式 sink：块序列与 WriteSSE 完全一致，
// 区别是文本逐段实时下发（真打字机）。
func openAIStreamSink(w http.ResponseWriter, model string, includeUsage bool) relay.StreamEvents {
	flusher, _ := w.(http.Flusher)
	started := false
	id := newResponsesID("chatcmpl-")
	created := time.Now().Unix()
	send := func(chunk *relay.StreamChunk) {
		b, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	ensureStart := func() {
		if started {
			return
		}
		sseHead(w)
		started = true
		send(&relay.StreamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []relay.StreamChoice{{Index: 0, Delta: relay.StreamDelta{Role: "assistant"}}}})
	}
	return relay.StreamEvents{
		OnText: func(piece string) error {
			ensureStart()
			emitTypewriter(func(part string) {
				send(&relay.StreamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []relay.StreamChoice{{Index: 0, Delta: relay.StreamDelta{Content: part}}}})
			}, piece)
			return nil
		},
		OnFinish: func(resp *relay.ChatResponse) error {
			ensureStart()
			if len(resp.ToolCalls) > 0 {
				tc := resp.ToolCalls[0]
				tcOut := relay.ToolCall{ID: tc.ID, Type: "function"}
				tcOut.Function.Name = tc.Function.Name
				tcOut.Function.Arguments = tc.Function.Arguments
				send(&relay.StreamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []relay.StreamChoice{{Index: 0, Delta: relay.StreamDelta{ToolCalls: []relay.ToolCall{tcOut}}}}})
			}
			fr := resp.FinishReason
			send(&relay.StreamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []relay.StreamChoice{{Index: 0, Delta: relay.StreamDelta{}, FinishReason: &fr}}})
			if includeUsage {
				send(&relay.StreamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: resp.Model, Usage: &resp.Usage})
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		},
		OnError: func(he *relay.HTTPError) {
			if !started {
				writeOpenAIError(w, he)
				return
			}
			// 已下发过字节：以流内错误块体面收尾
			b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": he.Message, "type": he.Code, "code": he.Code}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		},
	}
}

// anthropicStreamSink Anthropic Messages 流式 sink：事件序列与
// writeAnthropicSSE 一致；usage 在 message_delta 里补齐（input+output）。
func anthropicStreamSink(w http.ResponseWriter, model string) relay.StreamEvents {
	flusher, _ := w.(http.Flusher)
	started := false
	blockOpen := false
	idx := 0
	msgID := newMsgID()
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	ensureStart := func() {
		if started {
			return
		}
		sseHead(w)
		started = true
		send("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": msgID, "type": "message", "role": "assistant", "model": model,
				"content": []any{}, "stop_reason": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})
	}
	closeBlock := func() {
		if blockOpen {
			send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
			idx++
			blockOpen = false
		}
	}
	return relay.StreamEvents{
		OnText: func(piece string) error {
			ensureStart()
			if !blockOpen {
				send("content_block_start", map[string]any{
					"type": "content_block_start", "index": idx,
					"content_block": map[string]any{"type": "text", "text": ""},
				})
				blockOpen = true
			}
			emitTypewriter(func(part string) {
				send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": idx,
					"delta": map[string]any{"type": "text_delta", "text": part},
				})
			}, piece)
			return nil
		},
		OnFinish: func(resp *relay.ChatResponse) error {
			ensureStart()
			closeBlock()
			for _, tc := range resp.ToolCalls {
				send("content_block_start", map[string]any{
					"type": "content_block_start", "index": idx,
					"content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{}},
				})
				args := tc.Function.Arguments
				if args == "" {
					args = "{}"
				}
				send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": idx,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
				})
				send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
				idx++
			}
			stopReason := "end_turn"
			if len(resp.ToolCalls) > 0 {
				stopReason = "tool_use"
			}
			send("message_delta", map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": stopReason},
				"usage": map[string]any{"input_tokens": resp.Usage.PromptTokens, "output_tokens": resp.Usage.CompletionTokens},
			})
			send("message_stop", map[string]any{"type": "message_stop"})
			return nil
		},
		OnError: func(he *relay.HTTPError) {
			if !started {
				writeAnthropicError(w, he.Status, he.Code, he.Message)
				return
			}
			send("error", map[string]any{"type": "error", "error": map[string]any{"type": he.Code, "message": he.Message}})
			send("message_stop", map[string]any{"type": "message_stop"})
		},
	}
}

// responsesStreamSink OpenAI Responses API 流式 sink：事件序列与
// writeResponsesSSE 一致。
func responsesStreamSink(w http.ResponseWriter, model string) relay.StreamEvents {
	flusher, _ := w.(http.Flusher)
	started := false
	respID := newResponsesID("resp_")
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	ensureStart := func() {
		if started {
			return
		}
		sseHead(w)
		started = true
		send("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": respID, "object": "response", "model": model}})
	}
	return relay.StreamEvents{
		OnText: func(piece string) error {
			ensureStart()
			emitTypewriter(func(part string) {
				send("response.output_text.delta", map[string]any{
					"type": "response.output_text.delta", "item_id": newResponsesID("msg_"),
					"output_index": 0, "content_index": 0, "delta": part,
				})
			}, piece)
			return nil
		},
		OnFinish: func(resp *relay.ChatResponse) error {
			ensureStart()
			send("response.completed", map[string]any{"type": "response.completed", "response": chatToResponses(resp)})
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return nil
		},
		OnError: func(he *relay.HTTPError) {
			if !started {
				writeOpenAIError(w, he)
				return
			}
			send("response.failed", map[string]any{"type": "response.failed", "response": map[string]any{"id": respID, "error": map[string]any{"message": he.Message, "type": he.Code, "code": he.Code}}})
			fmt.Fprint(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		},
	}
}

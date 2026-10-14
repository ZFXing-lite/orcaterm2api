package relay

// OpenAI Chat Completions 协议类型（网关对外）。

// Message 对外消息。
type Message struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

// ContentPart 内容块。
type ContentPart struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	ImageURL *ImageURLPart  `json:"image_url,omitempty"`
}

// ImageURLPart 图片 URL。
type ImageURLPart struct {
	URL string `json:"url"`
}

// ToolCall 工具调用。
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ToolDef 工具定义。
type ToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Parameters  any    `json:"parameters,omitempty"`
	} `json:"function"`
}

// ChatRequest 对外聊天请求（OpenAI 形状）。
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	MaxTokens     int       `json:"max_tokens,omitempty"`
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
	Temperature   float64   `json:"temperature,omitempty"`
	Tools         []ToolDef `json:"tools,omitempty"`
	ToolChoice    any       `json:"tool_choice,omitempty"`
	ParallelToolCalls any   `json:"parallel_tool_calls,omitempty"`
	User          string    `json:"user,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// ChatResponse 网关内部归一化结果。
type ChatResponse struct {
	Model            string
	Text             string
	Thinking         string
	ToolCalls        []ToolCall
	FinishReason     string // stop | tool_calls
	Usage            Usage
	SessionKey       string
	UpstreamAction   string // completion | ask | review（透出为 X-OrcaTerm-Action）
	UnsupportedTools []string
	ConversationID   string
}

// Usage token 用量（估算）。
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// ChatCompletionResponse 对外 OpenAI JSON 响应。
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice 选项。
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// StreamChunk SSE 块。
type StreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
}

// StreamChoice 流式选项。
type StreamChoice struct {
	Index        int            `json:"index"`
	Delta        StreamDelta    `json:"delta"`
	FinishReason *string        `json:"finish_reason,omitempty"`
}

// StreamDelta 增量。
type StreamDelta struct {
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// ModelInfo /v1/models 条目。
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

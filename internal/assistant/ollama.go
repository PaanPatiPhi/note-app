package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	DefaultOllamaURL   = "http://localhost:11434"
	DefaultOllamaModel = "qwen3:4b-instruct"

	// prompt ภาษาไทย + tools ยาวเกิน context ค่าเริ่มต้นของ Ollama ได้ ซึ่งจะถูกตัดทิ้งแบบเงียบๆ
	ollamaNumCtx     = 8192
	ollamaNumPredict = 1024

	// เก็บโมเดลไว้ใน GPU นานๆ: ถ้า Ollama unload ข้อความแรกหลังว่างนานต้องรอโหลดใหม่หลายวินาที
	DefaultOllamaKeepAlive = "24h"
)

// OllamaProvider เรียก local model ผ่าน Ollama /api/chat (รองรับ tool calling)
type OllamaProvider struct {
	baseURL   string
	model     string
	keepAlive string
	http      *http.Client
}

func NewOllamaProvider(baseURL, model string) *OllamaProvider {
	if baseURL == "" {
		baseURL = DefaultOllamaURL
	}
	if model == "" {
		model = DefaultOllamaModel
	}
	return &OllamaProvider{baseURL: strings.TrimRight(baseURL, "/"), model: model, keepAlive: DefaultOllamaKeepAlive, http: http.DefaultClient}
}

// WithKeepAlive ตั้งเวลาที่ Ollama เก็บโมเดลไว้ในหน่วยความจำ เช่น "24h", "-1" (ตลอด); ว่าง = ค่าเริ่มต้น
func (p *OllamaProvider) WithKeepAlive(d string) *OllamaProvider {
	if d != "" {
		p.keepAlive = d
	}
	return p
}

// Warmup โหลดโมเดลเข้า GPU และให้โมเดลประมวลผล system prompt + tools ไว้ล่วงหน้า
// Ollama จำส่วนต้นของ prompt ที่เหมือนกันไว้ ข้อความแรกจริงจึงไม่ต้องประมวลผลส่วนนี้ใหม่
// ไม่มีการรัน tool: คำตอบของโมเดลถูกทิ้ง
func (p *OllamaProvider) Warmup(ctx context.Context, system string, tools []ToolSpec, userMessage string) error {
	if _, err := p.Start(system, tools, userMessage).Next(ctx, nil, ""); err != nil {
		return fmt.Errorf("ollama warmup: %w", err)
	}
	return nil
}

func (p *OllamaProvider) Name() string { return "ollama/" + p.model }

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"`
}

type ollamaToolCall struct {
	ID       string `json:"id,omitempty"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type ollamaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type ollamaChatRequest struct {
	Model     string          `json:"model"`
	Messages  []ollamaMessage `json:"messages"`
	Tools     []ollamaTool    `json:"tools,omitempty"`
	Stream    bool            `json:"stream"`
	Think     bool            `json:"think"`
	KeepAlive string          `json:"keep_alive,omitempty"`
	Options   map[string]any  `json:"options,omitempty"`
}

type ollamaChatResponse struct {
	Message         ollamaMessage `json:"message"`
	DoneReason      string        `json:"done_reason"`
	PromptEvalCount int64         `json:"prompt_eval_count"`
	EvalCount       int64         `json:"eval_count"`
	Error           string        `json:"error"`
}

func (p *OllamaProvider) Start(system string, tools []ToolSpec, userMessage string) Conversation {
	defs := make([]ollamaTool, 0, len(tools))
	for _, t := range tools {
		var d ollamaTool
		d.Type = "function"
		d.Function.Name = t.Name
		d.Function.Description = t.Description
		d.Function.Parameters = t.schema()
		defs = append(defs, d)
	}
	return &ollamaConversation{
		p:     p,
		tools: defs,
		messages: []ollamaMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: userMessage},
		},
	}
}

type ollamaConversation struct {
	p        *OllamaProvider
	tools    []ollamaTool
	messages []ollamaMessage
	round    int
}

func (c *ollamaConversation) Next(ctx context.Context, results []ToolResult, followUp string) (*Turn, error) {
	for _, r := range results {
		content := r.Content
		if r.IsError {
			content = "ERROR: " + content
		}
		c.messages = append(c.messages, ollamaMessage{Role: "tool", Content: content, ToolName: r.Name})
	}
	if followUp != "" {
		c.messages = append(c.messages, ollamaMessage{Role: "user", Content: followUp})
	}

	body, err := json.Marshal(ollamaChatRequest{
		Model:    c.p.model,
		Messages: c.messages,
		Tools:    c.tools,
		Stream:   false,
		// ปิดโหมดคิดของ qwen3 ไม่งั้นช้ามากและเปลือง context
		Think:     false,
		KeepAlive: c.p.keepAlive,
		Options: map[string]any{
			"num_ctx":     ollamaNumCtx,
			"num_predict": ollamaNumPredict,
			"temperature": 0,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode ollama request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.p.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read ollama response: %w", err)
	}
	var out ollamaChatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode ollama response (HTTP %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || out.Error != "" {
		return nil, fmt.Errorf("ollama HTTP %d: %s", resp.StatusCode, out.Error)
	}

	c.round++
	c.messages = append(c.messages, out.Message)

	turn := &Turn{
		Text:  stripThink(out.Message.Content),
		Usage: Usage{InputTokens: out.PromptEvalCount, OutputTokens: out.EvalCount},
	}
	for i, tc := range out.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d_%d", c.round, i)
		}
		turn.ToolCalls = append(turn.ToolCalls, ToolCall{ID: id, Name: tc.Function.Name, Input: normalizeArgs(tc.Function.Arguments)})
	}

	switch {
	case out.DoneReason == "length":
		turn.Stop = StopMaxTokens
	case len(turn.ToolCalls) > 0:
		turn.Stop = StopToolUse
	default:
		turn.Stop = StopEnd
	}
	return turn, nil
}

// normalizeArgs รับ arguments ได้ทั้ง JSON object และ JSON string ที่ห่อ object ไว้
// (บางโมเดลส่งแบบหลัง)
func normalizeArgs(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return json.RawMessage(`{}`)
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return json.RawMessage(s)
		}
	}
	return trimmed
}

// stripThink ตัด <think>...</think> ที่บางโมเดลยังใส่มาในคำตอบแม้ปิดโหมดคิดแล้ว
// รุ่น thinking-only (เช่น Qwen3-*-Thinking-2507) ใส่ <think> ไว้ใน template จึงมีแต่ </think> ใน output
func stripThink(s string) string {
	if end := strings.Index(s, "</think>"); end >= 0 && !strings.Contains(s[:end], "<think>") {
		s = s[end+len("</think>"):]
	}
	for {
		start := strings.Index(s, "<think>")
		if start < 0 {
			break
		}
		end := strings.Index(s[start:], "</think>")
		if end < 0 {
			s = s[:start]
			break
		}
		s = s[:start] + s[start+end+len("</think>"):]
	}
	return strings.TrimSpace(s)
}

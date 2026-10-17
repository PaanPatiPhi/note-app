package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Provider คือ LLM ที่ assistant ใช้เลือก tool (Claude หรือ local model ผ่าน Ollama)
// loop ใน assistant.go ทำงานกับ type กลางในไฟล์นี้เท่านั้น ไม่ผูกกับ SDK ของเจ้าใด
type Provider interface {
	// Name คือชื่อที่บันทึกลง ai_actions.model เช่น "claude-opus-5-5" หรือ "ollama/qwen3:4b"
	Name() string
	// Start เริ่มบทสนทนาใหม่หนึ่งข้อความ
	Start(system string, tools []ToolSpec, userMessage string) Conversation
}

// Conversation เก็บประวัติของหนึ่งข้อความไว้ภายใน provider
type Conversation interface {
	// Next ส่งผลของ tool ในรอบก่อน (รอบแรกเป็น nil) และข้อความเพิ่มเติมถึงโมเดล (ว่างได้)
	// แล้วคืนคำตอบถัดไปของโมเดล
	Next(ctx context.Context, results []ToolResult, followUp string) (*Turn, error)
}

// ToolSpec คือนิยาม tool แบบกลาง; Properties/Required เป็น JSON Schema ของ object
type ToolSpec struct {
	Name        string
	Description string
	Properties  map[string]any
	Required    []string
}

func (t ToolSpec) schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           t.Properties,
		"required":             t.Required,
		"additionalProperties": false,
	}
}

type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

type ToolResult struct {
	CallID  string
	Name    string
	Content string
	IsError bool
}

type StopKind int

const (
	StopEnd StopKind = iota
	StopToolUse
	StopRefusal
	StopMaxTokens
)

type Usage struct {
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
}

// Turn คือคำตอบหนึ่งรอบของโมเดล
type Turn struct {
	Text      string
	ToolCalls []ToolCall
	Stop      StopKind
	Usage     Usage
}

// ProviderFromEnv สร้าง provider ตาม AI_PROVIDER (ollama|claude, ค่าเริ่มต้น ollama = local model)
// getenv แยกออกมาให้ test ส่งค่าเองได้
func ProviderFromEnv(getenv func(string) string) (Provider, error) {
	model := getenv("AI_MODEL")
	switch p := strings.ToLower(strings.TrimSpace(getenv("AI_PROVIDER"))); p {
	case "claude", "anthropic":
		client := anthropic.NewClient()
		return NewClaudeProvider(&client.Messages, model), nil
	case "", "ollama":
		return NewOllamaProvider(getenv("OLLAMA_URL"), model), nil
	default:
		return nil, fmt.Errorf("unknown AI_PROVIDER %q (use claude or ollama)", p)
	}
}

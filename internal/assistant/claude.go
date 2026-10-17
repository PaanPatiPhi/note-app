package assistant

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

const (
	DefaultClaudeModel = "claude-opus-5-5"
	claudeMaxTokens    = 4096
)

// MessageCreator คือส่วนของ Claude client ที่ใช้ (anthropic.Client.Messages implement อยู่แล้ว)
// แยกเป็น interface เพื่อให้ test ใส่ fake ได้
type MessageCreator interface {
	New(ctx context.Context, body anthropic.MessageNewParams, opts ...option.RequestOption) (*anthropic.Message, error)
}

var _ MessageCreator = (*anthropic.MessageService)(nil)

type ClaudeProvider struct {
	llm   MessageCreator
	model string
}

func NewClaudeProvider(llm MessageCreator, model string) *ClaudeProvider {
	if model == "" {
		model = DefaultClaudeModel
	}
	return &ClaudeProvider{llm: llm, model: model}
}

func (p *ClaudeProvider) Name() string { return p.model }

func (p *ClaudeProvider) Start(system string, tools []ToolSpec, userMessage string) Conversation {
	defs := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        t.Name,
			Description: anthropic.String(t.Description),
			Strict:      anthropic.Bool(true),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties:  t.Properties,
				Required:    t.Required,
				ExtraFields: map[string]any{"additionalProperties": false},
			},
		}})
	}
	return &claudeConversation{
		llm: p.llm,
		params: anthropic.MessageNewParams{
			Model:        anthropic.Model(p.model),
			MaxTokens:    claudeMaxTokens,
			OutputConfig: anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffortLow},
			// system + tools คงที่ทุก request → cache ใช้ซ้ำได้
			System: []anthropic.TextBlockParam{{
				Text:         system,
				CacheControl: anthropic.NewCacheControlEphemeralParam(),
			}},
			Tools: defs,
			Messages: []anthropic.MessageParam{
				anthropic.NewUserMessage(anthropic.NewTextBlock(userMessage)),
			},
		},
	}
}

type claudeConversation struct {
	llm    MessageCreator
	params anthropic.MessageNewParams
}

func (c *claudeConversation) Next(ctx context.Context, results []ToolResult, followUp string) (*Turn, error) {
	// tool_result ทุกตัวของรอบเดียวกัน (และข้อความเพิ่มเติม) ต้องอยู่ใน user message เดียว
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(results)+1)
	for _, r := range results {
		blocks = append(blocks, anthropic.NewToolResultBlock(r.CallID, r.Content, r.IsError))
	}
	if followUp != "" {
		blocks = append(blocks, anthropic.NewTextBlock(followUp))
	}
	if len(blocks) > 0 {
		c.params.Messages = append(c.params.Messages, anthropic.NewUserMessage(blocks...))
	}

	resp, err := c.llm.New(ctx, c.params)
	if err != nil {
		return nil, err
	}
	c.params.Messages = append(c.params.Messages, resp.ToParam())

	turn := &Turn{Usage: Usage{
		InputTokens:     resp.Usage.InputTokens,
		OutputTokens:    resp.Usage.OutputTokens,
		CacheReadTokens: resp.Usage.CacheReadInputTokens,
	}}
	var text strings.Builder
	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case anthropic.TextBlock:
			text.WriteString(b.Text)
		case anthropic.ToolUseBlock:
			turn.ToolCalls = append(turn.ToolCalls, ToolCall{
				ID: b.ID, Name: b.Name, Input: json.RawMessage(b.JSON.Input.Raw()),
			})
		}
	}
	turn.Text = text.String()

	switch resp.StopReason {
	case anthropic.StopReasonRefusal:
		turn.Stop = StopRefusal
	case anthropic.StopReasonMaxTokens:
		turn.Stop = StopMaxTokens
	case anthropic.StopReasonToolUse:
		turn.Stop = StopToolUse
	default:
		turn.Stop = StopEnd
	}
	return turn, nil
}

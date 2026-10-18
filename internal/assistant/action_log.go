package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Action คือหนึ่งแถวใน ai_actions
type Action struct {
	UserID         string
	WebhookEventID string
	InputText      string
	Model          string
	ToolCalls      []toolRun
	ReplyText      string
	Error          string

	CreatedNoteIDs []string
	AddedItemIDs   []string
	ItemChanges    []itemChange

	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64

	timing timing
}

func (a *Action) addUsage(u Usage) {
	a.InputTokens += u.InputTokens
	a.OutputTokens += u.OutputTokens
	a.CacheReadTokens += u.CacheReadTokens
}

// actionDetails คือสิ่งที่เก็บใน tool_calls (jsonb) — รวมข้อมูลที่ต้องใช้ตอน undo
type actionDetails struct {
	Calls        []toolRun    `json:"calls"`
	AddedItemIDs []string     `json:"added_item_ids,omitempty"`
	ItemChanges  []itemChange `json:"item_changes,omitempty"`
	Error        string       `json:"error,omitempty"`
}

type ActionRepository struct {
	pool *pgxpool.Pool
}

func NewActionRepository(pool *pgxpool.Pool) *ActionRepository {
	return &ActionRepository{pool: pool}
}

func (r *ActionRepository) Insert(ctx context.Context, a *Action) (string, error) {
	details, err := json.Marshal(actionDetails{
		Calls: a.ToolCalls, AddedItemIDs: a.AddedItemIDs, ItemChanges: a.ItemChanges, Error: a.Error,
	})
	if err != nil {
		return "", fmt.Errorf("encode tool calls: %w", err)
	}

	touched := make([]string, 0, len(a.AddedItemIDs)+len(a.ItemChanges))
	touched = append(touched, a.AddedItemIDs...)
	for _, c := range a.ItemChanges {
		touched = append(touched, c.ItemID)
	}

	var webhookEventID *string
	if a.WebhookEventID != "" {
		webhookEventID = &a.WebhookEventID
	}

	var id string
	err = r.pool.QueryRow(ctx, `
		INSERT INTO ai_actions (
			user_id, webhook_event_id, input_text, model, tool_calls, reply_text,
			created_note_ids, touched_item_ids, input_tokens, output_tokens, cache_read_tokens
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7::uuid[], $8::uuid[], $9, $10, $11)
		RETURNING id
	`,
		a.UserID, webhookEventID, a.InputText, a.Model, string(details), a.ReplyText,
		nonNil(a.CreatedNoteIDs), touched, a.InputTokens, a.OutputTokens, a.CacheReadTokens,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert ai_action: %w", err)
	}
	return id, nil
}

func nonNil(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

// timing เก็บเวลาที่ใช้แต่ละขั้น ไว้ log หาจุดที่ช้า (ไม่บันทึกลง DB)
type timing struct {
	lists time.Duration
	llm   []time.Duration
	tools time.Duration
	log   time.Duration
}

func (t timing) String() string {
	var total time.Duration
	parts := make([]string, len(t.llm))
	for i, d := range t.llm {
		total += d
		parts[i] = d.Round(time.Millisecond).String()
	}
	return fmt.Sprintf("lists=%s llm=%s [%s] tools=%s log=%s",
		t.lists.Round(time.Millisecond), total.Round(time.Millisecond), strings.Join(parts, " "),
		t.tools.Round(time.Millisecond), t.log.Round(time.Millisecond))
}

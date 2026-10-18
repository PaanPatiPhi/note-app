package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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

// undoWindow คือระยะเวลาที่กดยกเลิกได้หลัง AI ทำรายการ
const undoWindow = 24 * time.Hour

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// UndoResult สรุปสิ่งที่ undo ทำไป
type UndoResult struct {
	NotFound      bool
	AlreadyUndone bool
	Expired       bool

	DeletedNotes  int64 // โน้ต/ลิสต์/เตือน ที่ AI สร้าง แล้วถูกลบ
	RemovedItems  int64 // item ที่ AI เพิ่มเข้าลิสต์เดิม แล้วถูกเอาออก
	RestoredItems int64 // item ที่ AI ติ๊ก แล้วถูกคืนสถานะเดิม
}

// Undo ย้อนทุกอย่างที่ ai_action นี้ทำ ภายใน transaction เดียว
// ทุกคำสั่งผูก user_id เสมอ: กดปุ่มของคนอื่น (หรือแก้ postback data เอง) ทำอะไรข้อมูลคนอื่นไม่ได้
func (r *ActionRepository) Undo(ctx context.Context, userID, actionID string) (*UndoResult, error) {
	if !uuidPattern.MatchString(actionID) {
		return &UndoResult{NotFound: true}, nil
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		createdNoteIDs []string
		rawDetails     []byte
		undoneAt       *time.Time
		createdAt      time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT created_note_ids::text[], tool_calls::text, undone_at, created_at
		FROM ai_actions WHERE id = $1 AND user_id = $2
		FOR UPDATE
	`, actionID, userID).Scan(&createdNoteIDs, &rawDetails, &undoneAt, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return &UndoResult{NotFound: true}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load ai_action: %w", err)
	}
	if undoneAt != nil {
		return &UndoResult{AlreadyUndone: true}, nil
	}
	if time.Since(createdAt) > undoWindow {
		return &UndoResult{Expired: true}, nil
	}

	var details actionDetails
	if err := json.Unmarshal(rawDetails, &details); err != nil {
		return nil, fmt.Errorf("decode tool_calls: %w", err)
	}

	res := &UndoResult{}
	if len(details.ItemChanges) > 0 {
		ids := make([]string, len(details.ItemChanges))
		prev := make([]bool, len(details.ItemChanges))
		for i, c := range details.ItemChanges {
			ids[i], prev[i] = c.ItemID, c.PreviousDone
		}
		tag, err := tx.Exec(ctx, `
			UPDATE note_items ni SET is_done = c.prev
			FROM unnest($1::uuid[], $2::bool[]) AS c(id, prev), notes n
			WHERE ni.id = c.id AND ni.note_id = n.id AND n.user_id = $3
		`, ids, prev, userID)
		if err != nil {
			return nil, fmt.Errorf("restore items: %w", err)
		}
		res.RestoredItems = tag.RowsAffected()
	}
	if len(details.AddedItemIDs) > 0 {
		tag, err := tx.Exec(ctx, `
			DELETE FROM note_items ni USING notes n
			WHERE ni.id = ANY($1::uuid[]) AND ni.note_id = n.id AND n.user_id = $2
		`, details.AddedItemIDs, userID)
		if err != nil {
			return nil, fmt.Errorf("remove added items: %w", err)
		}
		res.RemovedItems = tag.RowsAffected()
	}
	if len(createdNoteIDs) > 0 {
		// note_items ของโน้ตเหล่านี้ถูกลบตามด้วย ON DELETE CASCADE
		tag, err := tx.Exec(ctx, `DELETE FROM notes WHERE id = ANY($1::uuid[]) AND user_id = $2`, createdNoteIDs, userID)
		if err != nil {
			return nil, fmt.Errorf("delete created notes: %w", err)
		}
		res.DeletedNotes = tag.RowsAffected()
	}

	if _, err := tx.Exec(ctx, `UPDATE ai_actions SET undone_at = now() WHERE id = $1`, actionID); err != nil {
		return nil, fmt.Errorf("mark undone: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}
	return res, nil
}

// HistoryItem คือข้อความหนึ่งข้อความที่ผู้ใช้เคยสั่ง AI (จาก LINE หรือเว็บ) ใช้แสดงในหน้าแชท
type HistoryItem struct {
	ID        string     `json:"id"`
	Text      string     `json:"text"`
	Reply     string     `json:"reply"`
	Channel   string     `json:"channel"` // "line" | "web"
	CreatedAt time.Time  `json:"created_at"`
	UndoneAt  *time.Time `json:"undone_at,omitempty"`
	// Undoable = เคยเขียนข้อมูล, ยังไม่ถูกยกเลิก และยังอยู่ในเวลาที่ยกเลิกได้
	Undoable bool `json:"undoable"`
}

// History คืนประวัติล่าสุดของ user เรียงจากเก่าไปใหม่ (ในหน้านั้น)
// before = โหลดหน้าที่เก่ากว่าเวลานี้ (nil = ล่าสุด)
func (r *ActionRepository) History(ctx context.Context, userID string, limit int, before *time.Time) ([]HistoryItem, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id::text, input_text, coalesce(reply_text, ''),
		       webhook_event_id IS NOT NULL,
		       created_at, undone_at,
		       cardinality(created_note_ids) > 0 OR cardinality(touched_item_ids) > 0
		FROM ai_actions
		WHERE user_id = $1 AND ($2::timestamptz IS NULL OR created_at < $2)
		ORDER BY created_at DESC
		LIMIT $3
	`, userID, before, limit)
	if err != nil {
		return nil, fmt.Errorf("load history: %w", err)
	}
	defer rows.Close()

	var items []HistoryItem
	for rows.Next() {
		var (
			it       HistoryItem
			fromLine bool
			wrote    bool
		)
		if err := rows.Scan(&it.ID, &it.Text, &it.Reply, &fromLine, &it.CreatedAt, &it.UndoneAt, &wrote); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		it.Channel = "web"
		if fromLine {
			it.Channel = "line"
		}
		it.Undoable = wrote && it.UndoneAt == nil && time.Since(it.CreatedAt) <= undoWindow
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Reverse(items) // หน้าแชทแสดงเก่าอยู่บน ใหม่อยู่ล่าง
	return items, nil
}

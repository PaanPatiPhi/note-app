package assistant

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"jodjod/internal/note"
)

func TestActionRepositoryInsert(t *testing.T) {
	_ = godotenv.Load("../../.env")
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("skipping db integration test: DATABASE_URL not set")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Skipf("skipping db integration test: %v", err)
	}
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Skipf("skipping db integration test: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()

	// user ชั่วคราว; ai_actions ถูกลบตามด้วย ON DELETE CASCADE
	var userID string
	if err := pool.QueryRow(ctx, `INSERT INTO users (display_name) VALUES ('jodjod-test-ai-actions') RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID); err != nil {
			t.Errorf("cleanup test user: %v", err)
		}
	})

	const noteID = "00000000-0000-0000-0000-000000000001"
	const itemID = "00000000-0000-0000-0000-000000000002"
	repo := NewActionRepository(pool)
	id, err := repo.Insert(ctx, &Action{
		UserID:         userID,
		InputText:      "ซื้อนมแล้ว",
		Model:          DefaultClaudeModel,
		ToolCalls:      []toolRun{{Name: "complete_items", Input: json.RawMessage(`{"queries":["นม"],"done":true}`), Result: map[string]any{"updated": []string{"นม"}}}},
		ReplyText:      "✅ ติ๊ก นม แล้วครับ",
		CreatedNoteIDs: []string{noteID},
		ItemChanges:    []itemChange{{ItemID: itemID, Text: "นม", PreviousDone: false, Done: true}},
		InputTokens:    120, OutputTokens: 30, CacheReadTokens: 0,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var (
		created, touched []string
		details          []byte
		webhookEventID   *string
	)
	err = pool.QueryRow(ctx, `
		SELECT created_note_ids::text[], touched_item_ids::text[], tool_calls::text, webhook_event_id
		FROM ai_actions WHERE id = $1`, id,
	).Scan(&created, &touched, &details, &webhookEventID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(created) != 1 || created[0] != noteID || len(touched) != 1 || touched[0] != itemID {
		t.Errorf("ids = created %v touched %v", created, touched)
	}
	if webhookEventID != nil {
		t.Errorf("webhook_event_id = %q, want NULL", *webhookEventID)
	}
	var d actionDetails
	if err := json.Unmarshal(details, &d); err != nil {
		t.Fatalf("decode tool_calls: %v", err)
	}
	if len(d.Calls) != 1 || d.Calls[0].Name != "complete_items" || len(d.ItemChanges) != 1 || d.ItemChanges[0].PreviousDone {
		t.Errorf("tool_calls = %s", details)
	}
}

// dbPool ต่อ DB จริงจาก DATABASE_URL (.env ที่ root); ไม่มี = skip
func dbPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	_ = godotenv.Load("../../.env")
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("skipping db integration test: DATABASE_URL not set")
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Skipf("skipping db integration test: %v", err)
	}
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Skipf("skipping db integration test: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// dbUser สร้าง user ชั่วคราว; notes/ai_actions ถูกลบตามด้วย ON DELETE CASCADE
func dbUser(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (display_name) VALUES ('jodjod-test-undo') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id); err != nil {
			t.Errorf("cleanup test user: %v", err)
		}
	})
	return id
}

func TestActionRepositoryUndo(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	notes := note.NewRepository(pool)
	actions := NewActionRepository(pool)
	userID := dbUser(t, pool)

	// สถานะก่อน AI: มีลิสต์ "ซื้อของ" ที่มี "นม" (ยังไม่ติ๊ก)
	title := "ซื้อของ"
	list, err := notes.SaveChecklist(ctx, userID, &title, []string{"นม"}, note.NoteSourceLine)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := notes.FindItemsByNoteID(ctx, list.ID)
	milk := items[0]

	// สิ่งที่ AI ทำ: สร้างโน้ตใหม่, เพิ่ม "ไข่" เข้าลิสต์เดิม, ติ๊ก "นม"
	content := "อย่าลืมซื้อของ"
	created := &note.Note{UserID: userID, Content: &content, Type: note.NoteTypeNote, Source: note.NoteSourceAI}
	if err := notes.SaveNote(ctx, created); err != nil {
		t.Fatal(err)
	}
	added, err := notes.AppendItems(ctx, userID, list.ID, []string{"ไข่"})
	if err != nil {
		t.Fatal(err)
	}
	if err := notes.SetItemDone(ctx, userID, milk.ID, true); err != nil {
		t.Fatal(err)
	}
	actionID, err := actions.Insert(ctx, &Action{
		UserID: userID, InputText: "ทดสอบ undo", Model: "test",
		CreatedNoteIDs: []string{created.ID},
		AddedItemIDs:   []string{added[0].ID},
		ItemChanges:    []itemChange{{ItemID: milk.ID, Text: "นม", PreviousDone: false, Done: true}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// คนอื่นกดปุ่มนี้ (หรือแก้ postback เอง) ต้องทำอะไรไม่ได้
	other := dbUser(t, pool)
	if res, err := actions.Undo(ctx, other, actionID); err != nil || !res.NotFound {
		t.Fatalf("Undo by another user = %+v, %v; want NotFound", res, err)
	}
	if res, err := actions.Undo(ctx, userID, "not-a-uuid"); err != nil || !res.NotFound {
		t.Fatalf("Undo with a bad id = %+v, %v; want NotFound", res, err)
	}

	res, err := actions.Undo(ctx, userID, actionID)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if res.DeletedNotes != 1 || res.RemovedItems != 1 || res.RestoredItems != 1 {
		t.Fatalf("Undo result = %+v, want 1 deleted note, 1 removed item, 1 restored item", res)
	}

	if _, err := notes.FindByUserAndID(ctx, userID, created.ID); err == nil {
		t.Error("the note created by AI should be deleted")
	}
	after, _ := notes.FindItemsByNoteID(ctx, list.ID)
	if len(after) != 1 || after[0].ID != milk.ID || after[0].IsDone {
		t.Errorf("list items after undo = %+v, want only นม, not done", after)
	}
	if _, err := notes.FindByUserAndID(ctx, userID, list.ID); err != nil {
		t.Error("the pre-existing list must not be deleted")
	}

	if res, err := actions.Undo(ctx, userID, actionID); err != nil || !res.AlreadyUndone {
		t.Fatalf("second Undo = %+v, %v; want AlreadyUndone", res, err)
	}
}

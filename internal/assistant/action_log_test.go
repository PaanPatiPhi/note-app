package assistant

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
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

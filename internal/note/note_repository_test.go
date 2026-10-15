package note

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// getTestPool ต่อ DB จริงจาก DATABASE_URL (อ่าน .env ที่ root ของ repo ถ้ามี); ไม่มี = skip
func getTestPool(t *testing.T) *pgxpool.Pool {
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
	// ปิด pool ผ่าน t.Cleanup (ไม่ใช่ defer) เพื่อให้รันหลัง cleanup ที่ลบข้อมูลทดสอบ (Cleanup เรียงแบบ LIFO)
	t.Cleanup(pool.Close)
	return pool
}

// createTestUser สร้าง user ชั่วคราว; notes ของ user จะถูกลบตามด้วย ON DELETE CASCADE
func createTestUser(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO users (display_name) VALUES ('jodjod-test-note-repo') RETURNING id`,
	).Scan(&id)
	if err != nil {
		t.Fatalf("create test user: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id); err != nil {
			t.Errorf("cleanup test user %s: %v", id, err)
		}
	})
	return id
}

func TestSaveAndReadReminder(t *testing.T) {
	pool := getTestPool(t)

	ctx := context.Background()
	repo := NewRepository(pool)
	userID := createTestUser(t, pool)

	content := "โทรหาหมอ"
	remindAt := time.Date(2026, 10, 16, 9, 0, 0, 0, bangkok)
	n := &Note{UserID: userID, Content: &content, Type: NoteTypeReminder, RemindAt: &remindAt, Source: NoteSourceAI}
	if err := repo.SaveNote(ctx, n); err != nil {
		t.Fatalf("SaveNote: %v", err)
	}

	got, err := repo.FindByUserAndID(ctx, userID, n.ID)
	if err != nil {
		t.Fatalf("FindByUserAndID: %v", err)
	}
	if got.Type != NoteTypeReminder {
		t.Errorf("Type = %q, want %q", got.Type, NoteTypeReminder)
	}
	if got.Source != NoteSourceAI {
		t.Errorf("Source = %q, want %q", got.Source, NoteSourceAI)
	}
	if got.RemindAt == nil || !got.RemindAt.Equal(remindAt) {
		t.Errorf("RemindAt = %v, want %v", got.RemindAt, remindAt)
	}
	if got.RemindedAt != nil {
		t.Errorf("RemindedAt = %v, want nil", got.RemindedAt)
	}

	reminder := NoteTypeReminder
	feed, err := repo.FindFeed(ctx, FeedQuery{UserID: userID, TypeFilter: &reminder, Desc: true, Limit: 10})
	if err != nil {
		t.Fatalf("FindFeed: %v", err)
	}
	if len(feed) != 1 || feed[0].ID != n.ID {
		t.Fatalf("FindFeed(type=reminder) = %d notes, want the saved reminder", len(feed))
	}
}

func TestSaveDefaultsAndChecklistSource(t *testing.T) {
	pool := getTestPool(t)

	ctx := context.Background()
	repo := NewRepository(pool)
	userID := createTestUser(t, pool)

	content := "จดธรรมดา"
	plain := &Note{UserID: userID, Content: &content, Type: NoteTypeNote}
	if err := repo.SaveNote(ctx, plain); err != nil {
		t.Fatalf("SaveNote: %v", err)
	}
	if plain.Source != NoteSourceLine {
		t.Errorf("default Source = %q, want %q", plain.Source, NoteSourceLine)
	}

	title := "ซื้อของ"
	list, err := repo.SaveChecklist(ctx, userID, &title, []string{"นม", "ไข่"}, NoteSourceWeb)
	if err != nil {
		t.Fatalf("SaveChecklist: %v", err)
	}

	latest, err := repo.FindLatestByUser(ctx, userID)
	if err != nil {
		t.Fatalf("FindLatestByUser: %v", err)
	}
	if latest.ID != list.ID || latest.Source != NoteSourceWeb || latest.RemindAt != nil {
		t.Errorf("latest = {id:%s source:%s remind_at:%v}, want checklist from web without remind_at",
			latest.ID, latest.Source, latest.RemindAt)
	}
}

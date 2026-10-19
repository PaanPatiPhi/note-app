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

func TestOpenChecklistAppendAndAgenda(t *testing.T) {
	pool := getTestPool(t)
	ctx := context.Background()
	repo := NewRepository(pool)
	userID := createTestUser(t, pool)

	title := "ซื้อของ"
	list, err := repo.SaveChecklist(ctx, userID, &title, []string{"ขนมปัง"}, NoteSourceLine)
	if err != nil {
		t.Fatalf("SaveChecklist: %v", err)
	}

	found, err := repo.FindOpenChecklistByTitle(ctx, userID, "  ซื้อของ ")
	if err != nil || found == nil || found.ID != list.ID {
		t.Fatalf("FindOpenChecklistByTitle = %v, %v; want %s", found, err, list.ID)
	}
	if missing, err := repo.FindOpenChecklistByTitle(ctx, userID, "ไม่มีลิสต์นี้"); err != nil || missing != nil {
		t.Fatalf("FindOpenChecklistByTitle(missing) = %v, %v; want nil, nil", missing, err)
	}

	added, err := repo.AppendItems(ctx, userID, list.ID, []string{"นม", "ไข่"})
	if err != nil {
		t.Fatalf("AppendItems: %v", err)
	}
	if len(added) != 2 || added[0].Position != 1 || added[1].Position != 2 {
		t.Fatalf("AppendItems positions = %+v, want 1 and 2", added)
	}
	if _, err := repo.AppendItems(ctx, createTestUser(t, pool), list.ID, []string{"x"}); err == nil {
		t.Fatal("AppendItems should reject a checklist owned by another user")
	}

	titles, err := repo.ListOpenChecklistTitles(ctx, userID, 10)
	if err != nil || len(titles) != 1 || titles[0] != title {
		t.Fatalf("ListOpenChecklistTitles = %v, %v", titles, err)
	}

	// ติ๊กครบทุก item แล้ว ลิสต์ต้องไม่ถือว่า "เปิดอยู่"
	items, _ := repo.FindItemsByNoteID(ctx, list.ID)
	for _, it := range items {
		if err := repo.SetItemDone(ctx, userID, it.ID, true); err != nil {
			t.Fatalf("SetItemDone: %v", err)
		}
	}
	if done, _ := repo.FindOpenChecklistByTitle(ctx, userID, title); done != nil {
		t.Error("a fully-done checklist should not be returned as open")
	}

	inRange := time.Date(2026, 10, 16, 9, 0, 0, 0, bangkok)
	outOfRange := time.Date(2026, 10, 20, 9, 0, 0, 0, bangkok)
	for _, at := range []time.Time{inRange, outOfRange} {
		text := "นัด " + at.Format("02/01")
		if err := repo.SaveNote(ctx, &Note{UserID: userID, Content: &text, Type: NoteTypeReminder, RemindAt: &at, Source: NoteSourceAI}); err != nil {
			t.Fatalf("SaveNote: %v", err)
		}
	}
	from := time.Date(2026, 10, 16, 0, 0, 0, 0, bangkok)
	agenda, err := repo.FindAgenda(ctx, userID, from, from.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("FindAgenda: %v", err)
	}
	if len(agenda) != 1 || !agenda[0].RemindAt.Equal(inRange) {
		t.Fatalf("FindAgenda = %d notes, want only the 16/10 reminder", len(agenda))
	}
}

func TestPinnedFeed(t *testing.T) {
	pool := getTestPool(t)

	ctx := context.Background()
	repo := NewRepository(pool)
	userID := createTestUser(t, pool)

	save := func(text string) *Note {
		n := &Note{UserID: userID, Content: &text, Type: NoteTypeNote, Source: NoteSourceWeb}
		if err := repo.SaveNote(ctx, n); err != nil {
			t.Fatalf("SaveNote: %v", err)
		}
		return n
	}
	pinned := save("ปักไว้")
	save("ไม่ได้ปัก")

	if err := repo.SetPinned(ctx, userID, pinned.ID, true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}
	first, _ := repo.FindByUserAndID(ctx, userID, pinned.ID)
	if first.PinnedAt == nil {
		t.Fatal("PinnedAt = nil after pinning")
	}
	// ปักซ้ำต้องไม่เลื่อนเวลาที่ปักไว้
	if err := repo.SetPinned(ctx, userID, pinned.ID, true); err != nil {
		t.Fatalf("SetPinned again: %v", err)
	}
	again, _ := repo.FindByUserAndID(ctx, userID, pinned.ID)
	if again.PinnedAt == nil || !again.PinnedAt.Equal(*first.PinnedAt) {
		t.Errorf("PinnedAt moved on re-pin: %v -> %v", first.PinnedAt, again.PinnedAt)
	}

	got, err := repo.FindFeed(ctx, FeedQuery{UserID: userID, Desc: true, Limit: 10, PinnedOnly: true})
	if err != nil {
		t.Fatalf("FindFeed: %v", err)
	}
	if len(got) != 1 || got[0].ID != pinned.ID {
		t.Fatalf("pinned feed = %d notes, want only %s", len(got), pinned.ID)
	}

	if err := repo.SetPinned(ctx, userID, pinned.ID, false); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	got, _ = repo.FindFeed(ctx, FeedQuery{UserID: userID, Desc: true, Limit: 10, PinnedOnly: true})
	if len(got) != 0 {
		t.Errorf("pinned feed after unpin = %d notes, want 0", len(got))
	}

	if err := repo.SetPinned(ctx, "00000000-0000-0000-0000-000000000000", pinned.ID, true); err == nil {
		t.Error("SetPinned by another user succeeded, want error")
	}
}

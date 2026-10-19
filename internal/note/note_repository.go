package note

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) SaveNote(ctx context.Context, n *Note) error {
	if n.Source == "" {
		n.Source = NoteSourceLine
	}
	query := `
		INSERT INTO notes (user_id, content, type, is_done, due_date, image_url, remind_at, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, created_at, updated_at
	`
	err := r.pool.QueryRow(ctx, query,
		n.UserID, n.Content, n.Type, n.IsDone, n.DueDate, n.ImageURL, n.RemindAt, n.Source,
	).Scan(&n.ID, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return fmt.Errorf("save note: %w", err)
	}
	return nil
}

func (r *Repository) SaveChecklist(ctx context.Context, userID string, content *string, items []string, source NoteSource) (*Note, error) {
	if source == "" {
		source = NoteSourceLine
	}
	n := &Note{UserID: userID, Content: content, Type: NoteTypeChecklist, Source: source}

	// คำสั่งเดียว (atomic) แทน BEGIN + insert ทีละ item + COMMIT: DB อยู่ไกล แต่ละ round trip ~0.4 วิ
	query := `
		WITH n AS (
			INSERT INTO notes (user_id, content, type, source)
			VALUES ($1, $2, $3, $4)
			RETURNING id, created_at, updated_at
		), i AS (
			INSERT INTO note_items (note_id, text, position)
			SELECT n.id, u.text, u.ord - 1
			FROM n, unnest($5::text[]) WITH ORDINALITY AS u(text, ord)
		)
		SELECT id, created_at, updated_at FROM n
	`
	err := r.pool.QueryRow(ctx, query, n.UserID, n.Content, n.Type, n.Source, nonNilStrings(items)).
		Scan(&n.ID, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("save checklist: %w", err)
	}
	return n, nil
}

func nonNilStrings(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

func (r *Repository) FindLatestByUser(ctx context.Context, userID string) (*Note, error) {
	query := `
		SELECT ` + noteColumns + `
		FROM notes WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1
	`
	n, err := scanNote(r.pool.QueryRow(ctx, query, userID))
	if err != nil {
		return nil, fmt.Errorf("find latest note: %w", err)
	}
	return n, nil
}

func (r *Repository) UpdateContent(ctx context.Context, noteID, content string) error {
	query := `UPDATE notes SET content = $1 WHERE id = $2`
	tag, err := r.pool.Exec(ctx, query, content, noteID)
	if err != nil {
		return fmt.Errorf("update content: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("note not found: %s", noteID)
	}
	return nil
}

func (r *Repository) Delete(ctx context.Context, noteID string) error {
	query := `DELETE FROM notes WHERE id = $1`
	tag, err := r.pool.Exec(ctx, query, noteID)
	if err != nil {
		return fmt.Errorf("delete note: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("note not found: %s", noteID)
	}
	return nil
}

// buildTypeFilter คืน SQL fragment " AND type = $N" (หรือ "" ถ้าไม่กรอง) พร้อม args ที่เติมแล้ว
func buildTypeFilter(typeFilter *NoteType, args []any) (clause string, newArgs []any) {
	if typeFilter == nil {
		return "", args
	}
	args = append(args, string(*typeFilter))
	return fmt.Sprintf(" AND type = $%d", len(args)), args
}

// FindByUserAndDate หา note ทั้งหมดของ user ในวันที่ระบุ (ตามปฏิทินวันไทย) — ใช้กับคำสั่ง LINE เท่านั้น
func (r *Repository) FindByUserAndDate(ctx context.Context, userID string, date time.Time, typeFilter *NoteType, desc bool) ([]*Note, error) {
	dateStr := date.Format("2006-01-02")
	order := "ASC"
	if desc {
		order = "DESC"
	}

	args := []any{userID, dateStr}
	typeClause, args := buildTypeFilter(typeFilter, args)

	query := fmt.Sprintf(`
		SELECT `+noteColumns+`
		FROM notes
		WHERE user_id = $1
		  AND (created_at AT TIME ZONE 'Asia/Bangkok')::date = $2::date%s
		ORDER BY created_at %s
	`, typeClause, order)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("find notes by date: %w", err)
	}
	defer rows.Close()
	return scanNotes(rows)
}

// SearchByUser ค้นหาโน้ตด้วย keyword — ใช้กับคำสั่ง LINE "หา" เท่านั้น (ฝั่ง API ใช้ FindFeed แทน)
func (r *Repository) SearchByUser(ctx context.Context, userID, query string) ([]*Note, error) {
	q := `
		SELECT ` + noteColumns + `
		FROM notes
		WHERE user_id = $1 AND content ILIKE '%' || $2 || '%'
		ORDER BY created_at DESC
		LIMIT 20
	`
	rows, err := r.pool.Query(ctx, q, userID, query)
	if err != nil {
		return nil, fmt.Errorf("search notes: %w", err)
	}
	defer rows.Close()
	return scanNotes(rows)
}

// FeedQuery รวมเงื่อนไขทั้งหมดของ feed ฝั่ง LIFF: กรอง type, ค้นหาคำ, แบ่งหน้าแบบ cursor
type FeedQuery struct {
	UserID     string
	TypeFilter *NoteType
	Search     string     // ว่าง = ไม่กรองคำค้น
	After      *time.Time // cursor: created_at ของรายการสุดท้ายที่เห็นแล้ว, nil = หน้าแรก
	Desc       bool
	Limit      int
	PinnedOnly bool // true = เฉพาะโน้ตที่ปักหมุด (ใช้กับ widget)
}

// FindFeed ดึงโน้ตแบบรวม type filter + search + cursor pagination ในคำสั่งเดียว
// ใช้กับ endpoint GET /api/notes ฝั่ง LIFF (แทนที่ list กับ search แยกกันแบบเดิม)
//
// หมายเหตุ: cursor อ้างอิงแค่ created_at (ไม่ได้ผูกกับ id เป็น tie-breaker) ถ้ามี 2 โน้ต
// created_at ตรงกันเป๊ะ (ยากมากในทางปฏิบัติ ต้องเซฟในมิลลิวินาทีเดียวกัน) อาจมีข้ามหรือ
// ซ้ำ 1 รายการได้ ยอมรับ trade-off นี้เพื่อความง่ายของ MVP
func (r *Repository) FindFeed(ctx context.Context, q FeedQuery) ([]*Note, error) {
	order := "ASC"
	cmp := ">"
	if q.Desc {
		order = "DESC"
		cmp = "<"
	}

	args := []any{q.UserID}
	where := "user_id = $1"

	log.Printf("TypeFilter: %v", q.TypeFilter)

	if q.TypeFilter != nil {
		args = append(args, string(*q.TypeFilter))
		where += fmt.Sprintf(" AND type = $%d", len(args))
	}
	if q.Search != "" {
		args = append(args, q.Search)
		where += fmt.Sprintf(" AND content ILIKE '%%' || $%d || '%%'", len(args))
	}
	if q.PinnedOnly {
		where += " AND pinned_at IS NOT NULL"
	}
	if q.After != nil {
		args = append(args, *q.After)
		where += fmt.Sprintf(" AND created_at %s $%d", cmp, len(args))
	}

	args = append(args, q.Limit)
	limitPh := fmt.Sprintf("$%d", len(args))

	query := fmt.Sprintf(`
		SELECT `+noteColumns+`
		FROM notes
		WHERE %s
		ORDER BY created_at %s
		LIMIT %s
	`, where, order, limitPh)

	log.Printf("Feed query: %s, args: %v", query, args)

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("find feed: %w", err)
	}
	defer rows.Close()
	return scanNotes(rows)
}

// CountByUser นับจำนวนโน้ตทั้งหมดของ user (ไม่กรอง type/วัน) ใช้โชว์เป็นสถิติ เช่น
// "จดไปแล้ว N ครั้ง" — เป็น endpoint แยกต่างหาก ไม่เกี่ยวกับ pagination ของ feed
func (r *Repository) CountByUser(ctx context.Context, userID string) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM notes WHERE user_id = $1`, userID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count notes: %w", err)
	}
	return count, nil
}

// CountAll นับจำนวนโน้ตทั้งหมดในระบบ (สำหรับ Admin)
func (r *Repository) CountAll(ctx context.Context) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM notes`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count all notes: %w", err)
	}
	return count, nil
}

func (r *Repository) FindItemsByNoteID(ctx context.Context, noteID string) ([]*NoteItem, error) {
	query := `
		SELECT id, note_id, text, is_done, position
		FROM note_items WHERE note_id = $1 ORDER BY position ASC
	`
	rows, err := r.pool.Query(ctx, query, noteID)
	if err != nil {
		return nil, fmt.Errorf("find note items: %w", err)
	}
	defer rows.Close()

	var items []*NoteItem
	for rows.Next() {
		it := &NoteItem{}
		if err := rows.Scan(&it.ID, &it.NoteID, &it.Text, &it.IsDone, &it.Position); err != nil {
			return nil, fmt.Errorf("scan note item: %w", err)
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func (r *Repository) DeleteByUserAndID(ctx context.Context, userID, noteID string) error {
	query := `DELETE FROM notes WHERE id = $1 AND user_id = $2`
	tag, err := r.pool.Exec(ctx, query, noteID, userID)
	if err != nil {
		return fmt.Errorf("delete note: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("note not found or not owned by user")
	}
	return nil
}

func (r *Repository) FindByUserAndID(ctx context.Context, userID, noteID string) (*Note, error) {
	q := `
		SELECT ` + noteColumns + `
		FROM notes WHERE id = $1 AND user_id = $2
	`
	n, err := scanNote(r.pool.QueryRow(ctx, q, noteID, userID))
	if err != nil {
		return nil, fmt.Errorf("find note: %w", err)
	}
	return n, nil
}

func (r *Repository) UpdateContentByUser(ctx context.Context, userID, noteID, content string) error {
	q := `UPDATE notes SET content = $1 WHERE id = $2 AND user_id = $3`
	tag, err := r.pool.Exec(ctx, q, content, noteID, userID)
	if err != nil {
		return fmt.Errorf("update content: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("note not found or not owned by user")
	}
	return nil
}

func (r *Repository) SetNoteDone(ctx context.Context, userID, noteID string, done bool) error {
	q := `UPDATE notes SET is_done = $1 WHERE id = $2 AND user_id = $3`
	tag, err := r.pool.Exec(ctx, q, done, noteID, userID)
	if err != nil {
		return fmt.Errorf("set note done: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("note not found or not owned by user")
	}
	return nil
}

// SetPinned ปัก/ถอนหมุดโน้ต (ปักซ้ำไม่เลื่อนเวลาที่ปักไว้เดิม)
func (r *Repository) SetPinned(ctx context.Context, userID, noteID string, pinned bool) error {
	q := `UPDATE notes SET pinned_at = CASE WHEN $1::boolean THEN coalesce(pinned_at, now()) END
		WHERE id = $2 AND user_id = $3`
	tag, err := r.pool.Exec(ctx, q, pinned, noteID, userID)
	if err != nil {
		return fmt.Errorf("set pinned: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("note not found or not owned by user")
	}
	return nil
}

func (r *Repository) SetItemDone(ctx context.Context, userID, itemID string, done bool) error {
	q := `
		UPDATE note_items SET is_done = $1
		FROM notes
		WHERE note_items.id = $2 AND note_items.note_id = notes.id AND notes.user_id = $3
	`
	tag, err := r.pool.Exec(ctx, q, done, itemID, userID)
	if err != nil {
		return fmt.Errorf("set item done: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("item not found or not owned by user")
	}
	return nil
}

func (r *Repository) FindItemByPosition(ctx context.Context, noteID string, position int) (*NoteItem, error) {
	it := &NoteItem{}
	q := `
		SELECT id, note_id, text, is_done, position
		FROM note_items WHERE note_id = $1 ORDER BY position ASC OFFSET $2 LIMIT 1
	`
	err := r.pool.QueryRow(ctx, q, noteID, position-1).Scan(
		&it.ID, &it.NoteID, &it.Text, &it.IsDone, &it.Position,
	)
	if err != nil {
		return nil, fmt.Errorf("find item by position: %w", err)
	}
	return it, nil
}

// noteColumns คือคอลัมน์ของ notes ที่ SELECT ทุกครั้ง ต้องเรียงตรงกับ scanNote
const noteColumns = `id, user_id, content, type, is_done, due_date, image_url,
		remind_at, reminded_at, source, pinned_at, created_at, updated_at`

// scanNote scan หนึ่งแถวที่ SELECT ด้วย noteColumns
func scanNote(row interface{ Scan(...any) error }) (*Note, error) {
	n := &Note{}
	err := row.Scan(
		&n.ID, &n.UserID, &n.Content, &n.Type, &n.IsDone,
		&n.DueDate, &n.ImageURL, &n.RemindAt, &n.RemindedAt, &n.Source,
		&n.PinnedAt, &n.CreatedAt, &n.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// scanNotes helper รวม logic การ scan rows -> []*Note ใช้ร่วมกันหลายฟังก์ชัน
func scanNotes(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]*Note, error) {
	var notes []*Note
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, fmt.Errorf("scan note: %w", err)
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
}

// TryMarkEventProcessed ตรวจสอบและ mark webhook event ID ว่าถูกประมวลแล้ว
// คืน (isNew bool, error) - isNew=true คือ event ใหม่ ยังไม่เคยถูก mark
// ใช้ INSERT ... ON CONFLICT DO NOTHING แล้วดู RowsAffected ในคำสั่งเดียว (atomic):
// 1 = insert สำเร็จ (event ใหม่), 0 = ชน unique key (เคยประมวลแล้ว)
func (r *Repository) TryMarkEventProcessed(ctx context.Context, webhookEventID string) (bool, error) {
	query := `
		INSERT INTO webhook_events (webhook_event_id)
		VALUES ($1)
		ON CONFLICT (webhook_event_id) DO NOTHING
	`
	tag, err := r.pool.Exec(ctx, query, webhookEventID)
	if err != nil {
		return false, fmt.Errorf("mark event processed: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}



// FindItemsBySimilarText ค้นหา checklist item ของ user ที่ข้อความ "คล้าย" กับ query
// ใช้ pg_trgm operator "%" (เทียบกับ similarity_threshold ของ Postgres, default 0.3)
// เรียงจากคล้ายที่สุดไปน้อยที่สุด จำกัดจำนวนผลลัพธ์ตาม limit
func (r *Repository) FindItemsBySimilarText(ctx context.Context, userID, query string, limit int) ([]*ItemMatch, error) {
	q := `
		SELECT ni.id, ni.note_id, ni.text, ni.is_done, ni.position,
		       n.content, n.created_at,
		       similarity(ni.text, $2) AS sim
		FROM note_items ni
		JOIN notes n ON n.id = ni.note_id
		WHERE n.user_id = $1
		  AND ni.text % $2
		ORDER BY sim DESC
		LIMIT $3
	`
	rows, err := r.pool.Query(ctx, q, userID, query, limit)
	if err != nil {
		return nil, fmt.Errorf("find items by similar text: %w", err)
	}
	defer rows.Close()

	var matches []*ItemMatch
	for rows.Next() {
		it := &NoteItem{}
		var noteContent *string
		m := &ItemMatch{Item: it}

		err := rows.Scan(
			&it.ID, &it.NoteID, &it.Text, &it.IsDone, &it.Position,
			&noteContent, &m.NoteCreated, &m.Similarity,
		)
		if err != nil {
			return nil, fmt.Errorf("scan item match: %w", err)
		}
		if noteContent != nil {
			m.NoteContent = *noteContent
		}
		matches = append(matches, m)
	}
	return matches, rows.Err()
}

// FindOpenChecklistByTitle หา checklist ล่าสุดของ user ที่ชื่อตรงกับ title (ไม่สนตัวพิมพ์/ช่องว่างหัวท้าย)
// และยังมี item ที่ไม่เสร็จ (หรือยังไม่มี item) — ใช้รวมรายการใหม่เข้าลิสต์เดิม
// คืน (nil, nil) ถ้าไม่เจอ
func (r *Repository) FindOpenChecklistByTitle(ctx context.Context, userID, title string) (*Note, error) {
	q := `
		SELECT ` + noteColumns + `
		FROM notes n
		WHERE n.user_id = $1
		  AND n.type = 'checklist'
		  AND lower(btrim(n.content)) = lower(btrim($2))
		  AND (
		    NOT EXISTS (SELECT 1 FROM note_items ni WHERE ni.note_id = n.id)
		    OR EXISTS (SELECT 1 FROM note_items ni WHERE ni.note_id = n.id AND NOT ni.is_done)
		  )
		ORDER BY n.created_at DESC
		LIMIT 1
	`
	n, err := scanNote(r.pool.QueryRow(ctx, q, userID, title))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find open checklist: %w", err)
	}
	return n, nil
}

// ListOpenChecklistTitles คืนชื่อ checklist ที่ยังมี item ไม่เสร็จ (ไม่ซ้ำ, ล่าสุดก่อน)
// ใช้เป็น context ให้ AI รวมรายการเข้าลิสต์เดิม
func (r *Repository) ListOpenChecklistTitles(ctx context.Context, userID string, limit int) ([]string, error) {
	q := `
		SELECT btrim(n.content) AS title
		FROM notes n
		WHERE n.user_id = $1
		  AND n.type = 'checklist'
		  AND n.content IS NOT NULL AND btrim(n.content) <> ''
		  AND EXISTS (SELECT 1 FROM note_items ni WHERE ni.note_id = n.id AND NOT ni.is_done)
		GROUP BY btrim(n.content)
		ORDER BY max(n.created_at) DESC
		LIMIT $2
	`
	rows, err := r.pool.Query(ctx, q, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("list open checklist titles: %w", err)
	}
	defer rows.Close()

	var titles []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scan checklist title: %w", err)
		}
		titles = append(titles, t)
	}
	return titles, rows.Err()
}

// AppendItems เพิ่ม item ต่อท้าย checklist ของ user (ตรวจ ownership ด้วย userID)
func (r *Repository) AppendItems(ctx context.Context, userID, noteID string, items []string) ([]*NoteItem, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// lock note ไว้ (พร้อมอัปเดต updated_at) กันสอง request เพิ่มพร้อมกันแล้วได้ position ซ้ำ
	tag, err := tx.Exec(ctx, `
		UPDATE notes SET updated_at = now()
		WHERE id = $1 AND user_id = $2 AND type = 'checklist'
	`, noteID, userID)
	if err != nil {
		return nil, fmt.Errorf("lock checklist: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("checklist not found or not owned by user")
	}

	// insert ทุก item ในคำสั่งเดียว; คำสั่งนี้เริ่มหลังได้ lock แล้ว จึงเห็น item ที่ request อื่นเพิ่งเพิ่ม
	rows, err := tx.Query(ctx, `
		INSERT INTO note_items (note_id, text, position)
		SELECT $1, u.text,
		       (SELECT coalesce(max(position) + 1, 0) FROM note_items WHERE note_id = $1) + u.ord - 1
		FROM unnest($2::text[]) WITH ORDINALITY AS u(text, ord)
		RETURNING id, text, position
	`, noteID, nonNilStrings(items))
	if err != nil {
		return nil, fmt.Errorf("insert note_items: %w", err)
	}
	added := make([]*NoteItem, 0, len(items))
	for rows.Next() {
		it := &NoteItem{NoteID: noteID}
		if err := rows.Scan(&it.ID, &it.Text, &it.Position); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan note_item: %w", err)
		}
		added = append(added, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("insert note_items: %w", err)
	}
	sort.Slice(added, func(i, j int) bool { return added[i].Position < added[j].Position })

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}
	return added, nil
}

// FindAgenda คืน reminder (remind_at) และโน้ตที่มี due_date อยู่ในช่วง [from, to)
// เรียงตามเวลาที่เกี่ยวข้อง
func (r *Repository) FindAgenda(ctx context.Context, userID string, from, to time.Time) ([]*Note, error) {
	q := `
		SELECT ` + noteColumns + `
		FROM notes
		WHERE user_id = $1
		  AND ((remind_at >= $2 AND remind_at < $3) OR (due_date >= $2 AND due_date < $3))
		ORDER BY coalesce(remind_at, due_date)
		LIMIT 50
	`
	rows, err := r.pool.Query(ctx, q, userID, from, to)
	if err != nil {
		return nil, fmt.Errorf("find agenda: %w", err)
	}
	defer rows.Close()
	return scanNotes(rows)
}

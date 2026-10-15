package note

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) SaveNote(ctx context.Context, n *Note) error {
	query := `
		INSERT INTO notes (user_id, content, type, is_done, due_date, image_url)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, created_at, updated_at
	`
	err := r.pool.QueryRow(ctx, query,
		n.UserID, n.Content, n.Type, n.IsDone, n.DueDate, n.ImageURL,
	).Scan(&n.ID, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return fmt.Errorf("save note: %w", err)
	}
	return nil
}

func (r *Repository) SaveChecklist(ctx context.Context, userID string, content *string, items []string) (*Note, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	n := &Note{UserID: userID, Content: content, Type: NoteTypeChecklist}

	noteQuery := `
		INSERT INTO notes (user_id, content, type)
		VALUES ($1, $2, $3)
		RETURNING id, created_at, updated_at
	`
	err = tx.QueryRow(ctx, noteQuery, n.UserID, n.Content, n.Type).
		Scan(&n.ID, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert note: %w", err)
	}

	itemQuery := `INSERT INTO note_items (note_id, text, position) VALUES ($1, $2, $3)`
	for i, itemText := range items {
		if _, err := tx.Exec(ctx, itemQuery, n.ID, itemText, i); err != nil {
			return nil, fmt.Errorf("insert note_item %d: %w", i, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}
	return n, nil
}

func (r *Repository) FindLatestByUser(ctx context.Context, userID string) (*Note, error) {
	n := &Note{}
	query := `
		SELECT id, user_id, content, type, is_done, due_date, image_url, created_at, updated_at
		FROM notes WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1
	`
	err := r.pool.QueryRow(ctx, query, userID).Scan(
		&n.ID, &n.UserID, &n.Content, &n.Type, &n.IsDone,
		&n.DueDate, &n.ImageURL, &n.CreatedAt, &n.UpdatedAt,
	)
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
		SELECT id, user_id, content, type, is_done, due_date, image_url, created_at, updated_at
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
		SELECT id, user_id, content, type, is_done, due_date, image_url, created_at, updated_at
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
	if q.After != nil {
		args = append(args, *q.After)
		where += fmt.Sprintf(" AND created_at %s $%d", cmp, len(args))
	}

	args = append(args, q.Limit)
	limitPh := fmt.Sprintf("$%d", len(args))

	query := fmt.Sprintf(`
		SELECT id, user_id, content, type, is_done, due_date, image_url, created_at, updated_at
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
	n := &Note{}
	q := `
		SELECT id, user_id, content, type, is_done, due_date, image_url, created_at, updated_at
		FROM notes WHERE id = $1 AND user_id = $2
	`
	err := r.pool.QueryRow(ctx, q, noteID, userID).Scan(
		&n.ID, &n.UserID, &n.Content, &n.Type, &n.IsDone,
		&n.DueDate, &n.ImageURL, &n.CreatedAt, &n.UpdatedAt,
	)
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

// scanNotes helper รวม logic การ scan rows -> []*Note ใช้ร่วมกันหลายฟังก์ชัน
func scanNotes(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]*Note, error) {
	var notes []*Note
	for rows.Next() {
		n := &Note{}
		err := rows.Scan(
			&n.ID, &n.UserID, &n.Content, &n.Type, &n.IsDone,
			&n.DueDate, &n.ImageURL, &n.CreatedAt, &n.UpdatedAt,
		)
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

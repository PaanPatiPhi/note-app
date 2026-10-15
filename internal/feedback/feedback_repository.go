package feedback

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func (r *Repository) Create(ctx context.Context, f *Feedback) error {
	query := `
		INSERT INTO feedbacks (user_id, type, title, message, contact_email)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, status, created_at, updated_at
	`
	return r.pool.QueryRow(ctx, query, f.UserID, f.Type, f.Title, f.Message, f.ContactEmail).
		Scan(&f.ID, &f.Status, &f.CreatedAt, &f.UpdatedAt)
}

func (r *Repository) ListByUserID(ctx context.Context, userID string) ([]*Feedback, error) {
	query := `
		SELECT id, user_id, type, title, message, contact_email, status, created_at, updated_at
		FROM feedbacks
		WHERE user_id = $1
		ORDER BY created_at DESC
	`
	rows, err := r.pool.Query(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("list feedbacks: %w", err)
	}
	defer rows.Close()

	var list []*Feedback
	for rows.Next() {
		f := &Feedback{}
		if err := rows.Scan(&f.ID, &f.UserID, &f.Type, &f.Title, &f.Message, &f.ContactEmail, &f.Status, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		list = append(list, f)
	}
	return list, nil
}

// ListAll ดึงรายการ feedback ทั้งหมดสำหรับหน้า Admin พร้อมข้อมูล User (ถ้ามี)
func (r *Repository) ListAll(ctx context.Context, typeFilter, statusFilter string) ([]*Feedback, error) {
	query := `
		SELECT 
			f.id, f.user_id, f.type, f.title, f.message, f.contact_email, f.status, f.created_at, f.updated_at,
			u.email, u.display_name
		FROM feedbacks f
		LEFT JOIN users u ON f.user_id = u.id
		WHERE ($1 = '' OR f.type = $1)
		  AND ($2 = '' OR f.status = $2)
		ORDER BY f.created_at DESC
	`
	rows, err := r.pool.Query(ctx, query, typeFilter, statusFilter)
	if err != nil {
		return nil, fmt.Errorf("list all feedbacks: %w", err)
	}
	defer rows.Close()

	var list []*Feedback
	for rows.Next() {
		f := &Feedback{}
		var email, name *string
		if err := rows.Scan(
			&f.ID, &f.UserID, &f.Type, &f.Title, &f.Message, &f.ContactEmail, &f.Status, &f.CreatedAt, &f.UpdatedAt,
			&email, &name,
		); err != nil {
			return nil, fmt.Errorf("scan feedback: %w", err)
		}
		f.UserEmail = email
		f.UserName = name
		list = append(list, f)
	}
	return list, nil
}

// UpdateStatus อัปเดตสถานะของ feedback
func (r *Repository) UpdateStatus(ctx context.Context, id, status string) error {
	query := `
		UPDATE feedbacks
		SET status = $1, updated_at = NOW()
		WHERE id = $2
	`
	_, err := r.pool.Exec(ctx, query, status, id)
	return err
}

// CountFeedbacks นับจำนวน feedback ทั้งหมด
func (r *Repository) CountFeedbacks(ctx context.Context) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM feedbacks`).Scan(&count)
	return count, err
}

// CountPendingFeedbacks นับจำนวน feedback ที่ยังค้างอยู่
func (r *Repository) CountPendingFeedbacks(ctx context.Context) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM feedbacks WHERE status = 'pending'`).Scan(&count)
	return count, err
}

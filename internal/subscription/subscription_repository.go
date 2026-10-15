package subscription

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// Create บันทึกข้อมูล subscription ใหม่
func (r *Repository) Create(ctx context.Context, s *Subscription) error {
	query := `
		INSERT INTO subscriptions (
			user_id, tier, billing_cycle, amount, currency, status, starts_at, expires_at, payment_method, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, NOW(), NOW()
		)
		RETURNING id, created_at, updated_at
	`
	return r.pool.QueryRow(
		ctx, query,
		s.UserID, s.Tier, s.BillingCycle, s.Amount, s.Currency, s.Status, s.StartsAt, s.ExpiresAt, s.PaymentMethod,
	).Scan(&s.ID, &s.CreatedAt, &s.UpdatedAt)
}

// CancelActiveByUser ยกเลิก subscription ที่ active อยู่ของผู้ใช้ (เมื่อมีการเปลี่ยนแพ็กเกจใหม่)
func (r *Repository) CancelActiveByUser(ctx context.Context, userID string) error {
	query := `
		UPDATE subscriptions
		SET status = 'cancelled', updated_at = NOW()
		WHERE user_id = $1 AND status = 'active'
	`
	_, err := r.pool.Exec(ctx, query, userID)
	return err
}

// GetActiveByUserID ดึง subscription ที่กำลังใช้งานอยู่ของผู้ใช้
func (r *Repository) GetActiveByUserID(ctx context.Context, userID string) (*Subscription, error) {
	query := `
		SELECT id, user_id, tier, billing_cycle, amount, currency, status, starts_at, expires_at, payment_method, created_at, updated_at
		FROM subscriptions
		WHERE user_id = $1 AND status = 'active'
		ORDER BY created_at DESC
		LIMIT 1
	`
	s := &Subscription{}
	err := r.pool.QueryRow(ctx, query, userID).Scan(
		&s.ID, &s.UserID, &s.Tier, &s.BillingCycle, &s.Amount, &s.Currency,
		&s.Status, &s.StartsAt, &s.ExpiresAt, &s.PaymentMethod, &s.CreatedAt, &s.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get active subscription: %w", err)
	}
	return s, nil
}

// ListByUserID ดึงประวัติ subscription ทั้งหมดของ user
func (r *Repository) ListByUserID(ctx context.Context, userID string) ([]*Subscription, error) {
	query := `
		SELECT id, user_id, tier, billing_cycle, amount, currency, status, starts_at, expires_at, payment_method, created_at, updated_at
		FROM subscriptions
		WHERE user_id = $1
		ORDER BY created_at DESC
	`
	rows, err := r.pool.Query(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("list user subscriptions: %w", err)
	}
	defer rows.Close()

	var list []*Subscription
	for rows.Next() {
		s := &Subscription{}
		if err := rows.Scan(
			&s.ID, &s.UserID, &s.Tier, &s.BillingCycle, &s.Amount, &s.Currency,
			&s.Status, &s.StartsAt, &s.ExpiresAt, &s.PaymentMethod, &s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan subscription: %w", err)
		}
		list = append(list, s)
	}
	return list, rows.Err()
}

// ListAll ดึงรายการ subscriptions ทั้งหมดในระบบ (สำหรับ Admin) พร้อมข้อมูล User
func (r *Repository) ListAll(ctx context.Context) ([]*Subscription, error) {
	query := `
		SELECT 
			s.id, s.user_id, s.tier, s.billing_cycle, s.amount, s.currency,
			s.status, s.starts_at, s.expires_at, s.payment_method, s.created_at, s.updated_at,
			u.email, u.display_name
		FROM subscriptions s
		LEFT JOIN users u ON s.user_id = u.id
		ORDER BY s.created_at DESC
	`
	rows, err := r.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list all subscriptions: %w", err)
	}
	defer rows.Close()

	var list []*Subscription
	for rows.Next() {
		s := &Subscription{}
		var email, name *string
		if err := rows.Scan(
			&s.ID, &s.UserID, &s.Tier, &s.BillingCycle, &s.Amount, &s.Currency,
			&s.Status, &s.StartsAt, &s.ExpiresAt, &s.PaymentMethod, &s.CreatedAt, &s.UpdatedAt,
			&email, &name,
		); err != nil {
			return nil, fmt.Errorf("scan subscription with user: %w", err)
		}
		s.UserEmail = email
		s.UserName = name
		list = append(list, s)
	}
	return list, rows.Err()
}

// Cancel ยกเลิก subscription ตาม ID
func (r *Repository) Cancel(ctx context.Context, subscriptionID, userID string) error {
	query := `
		UPDATE subscriptions
		SET status = 'cancelled', updated_at = NOW()
		WHERE id = $1 AND user_id = $2
	`
	res, err := r.pool.Exec(ctx, query, subscriptionID, userID)
	if err != nil {
		return fmt.Errorf("cancel subscription: %w", err)
	}
	if res.RowsAffected() == 0 {
		return errors.New("subscription not found or unauthorized")
	}
	return nil
}

// CountActiveSubscriptions นับจำนวน subscription ที่ active
func (r *Repository) CountActiveSubscriptions(ctx context.Context) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM subscriptions WHERE status = 'active' AND tier = 'premium'`).Scan(&count)
	return count, err
}

// TotalRevenue คำนวณรายได้รวม
func (r *Repository) TotalRevenue(ctx context.Context) (float64, error) {
	var total *float64
	err := r.pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount), 0) FROM subscriptions WHERE status = 'active'`).Scan(&total)
	if err != nil || total == nil {
		return 0, err
	}
	return *total, nil
}

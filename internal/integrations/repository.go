package integrations

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const ProviderGoogle = "google"

// Connection คือบริการที่ user เชื่อมไว้ (token ถูกถอดรหัสแล้ว อยู่ในหน่วยความจำเท่านั้น)
type Connection struct {
	UserID       string
	Provider     string
	AccountEmail string
	Scopes       string
	AccessToken  string
	RefreshToken string
	TokenExpiry  time.Time
	CalendarID   string
	CreatedAt    time.Time
}

type Repository struct {
	pool   *pgxpool.Pool
	sealer *Sealer
}

func NewRepository(pool *pgxpool.Pool, sealer *Sealer) *Repository {
	return &Repository{pool: pool, sealer: sealer}
}

// Save สร้างหรือแทนที่การเชื่อมต่อของ user กับ provider นั้น
func (r *Repository) Save(ctx context.Context, c *Connection) error {
	access, err := r.sealer.Seal(c.AccessToken)
	if err != nil {
		return fmt.Errorf("seal access token: %w", err)
	}
	refresh, err := r.sealer.Seal(c.RefreshToken)
	if err != nil {
		return fmt.Errorf("seal refresh token: %w", err)
	}
	calendarID := c.CalendarID
	if calendarID == "" {
		calendarID = "primary"
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO integrations (user_id, provider, account_email, scopes, access_token, refresh_token, token_expiry, calendar_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (user_id, provider) DO UPDATE SET
			account_email = EXCLUDED.account_email,
			scopes        = EXCLUDED.scopes,
			access_token  = EXCLUDED.access_token,
			refresh_token = EXCLUDED.refresh_token,
			token_expiry  = EXCLUDED.token_expiry,
			updated_at    = now()
	`, c.UserID, c.Provider, c.AccountEmail, c.Scopes, access, refresh, c.TokenExpiry, calendarID)
	if err != nil {
		return fmt.Errorf("save integration: %w", err)
	}
	return nil
}

// UpdateAccessToken เก็บ access token ใหม่หลัง refresh (refresh token เดิมยังใช้ต่อ)
func (r *Repository) UpdateAccessToken(ctx context.Context, userID, provider, accessToken string, expiry time.Time) error {
	sealed, err := r.sealer.Seal(accessToken)
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `
		UPDATE integrations SET access_token = $3, token_expiry = $4, updated_at = now()
		WHERE user_id = $1 AND provider = $2
	`, userID, provider, sealed, expiry)
	return err
}

// Get คืน (nil, nil) ถ้ายังไม่ได้เชื่อม
func (r *Repository) Get(ctx context.Context, userID, provider string) (*Connection, error) {
	var (
		c                      = &Connection{UserID: userID, Provider: provider}
		access, refresh, email string
		expiry                 *time.Time
	)
	err := r.pool.QueryRow(ctx, `
		SELECT coalesce(account_email, ''), scopes, access_token, refresh_token, token_expiry, calendar_id, created_at
		FROM integrations WHERE user_id = $1 AND provider = $2
	`, userID, provider).Scan(&email, &c.Scopes, &access, &refresh, &expiry, &c.CalendarID, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load integration: %w", err)
	}
	c.AccountEmail = email
	if expiry != nil {
		c.TokenExpiry = *expiry
	}
	if c.AccessToken, err = r.sealer.Open(access); err != nil {
		return nil, err
	}
	if c.RefreshToken, err = r.sealer.Open(refresh); err != nil {
		return nil, err
	}
	return c, nil
}

func (r *Repository) Delete(ctx context.Context, userID, provider string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM integrations WHERE user_id = $1 AND provider = $2`, userID, provider)
	return err
}

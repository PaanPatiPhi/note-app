package user

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrIdentityAlreadyLinked  = errors.New("identity is already linked to another account")
	ErrCannotUnlinkOnlyMethod = errors.New("cannot unlink the only login method")
	ErrEmailAlreadyRegistered = errors.New("email is already registered")
)

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

// FindByID หา user จาก internal user ID (UUID)
func (r *Repository) FindByID(ctx context.Context, id string) (*User, error) {
	u := &User{}
	query := `
		SELECT id, email, password_hash, display_name, avatar_url, tier, role, created_at, updated_at
		FROM users
		WHERE id = $1
	`
	err := r.pool.QueryRow(ctx, query, id).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.AvatarURL, &u.Tier, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("find user by id: %w", err)
	}
	return u, nil
}

// FindByEmail ค้นหา user จากอีเมล (ใช้กับ Email login)
func (r *Repository) FindByEmail(ctx context.Context, email string) (*User, error) {
	u := &User{}
	query := `
		SELECT id, email, password_hash, display_name, avatar_url, tier, role, created_at, updated_at
		FROM users
		WHERE LOWER(email) = LOWER($1)
	`
	err := r.pool.QueryRow(ctx, query, email).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.AvatarURL, &u.Tier, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("find user by email: %w", err)
	}
	return u, nil
}

// CreateUserWithPassword สร้าง user ใหม่พร้อมรหัสผ่านที่ hash แล้ว
func (r *Repository) CreateUserWithPassword(ctx context.Context, email, passwordHash, displayName string) (*User, error) {
	// ตรวจสอบก่อนว่าอีเมลนี้มีอยู่แล้วหรือไม่
	existing, err := r.FindByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.PasswordHash != nil && *existing.PasswordHash != "" {
			return nil, ErrEmailAlreadyRegistered
		}
		// มี user ที่สร้างผ่าน Google อยู่แล้วแต่ยังไม่มีรหัสผ่าน ให้เพิ่มรหัสผ่านและ identity 'email'
		_, err = r.pool.Exec(ctx, `UPDATE users SET password_hash = $1 WHERE id = $2`, passwordHash, existing.ID)
		if err != nil {
			return nil, fmt.Errorf("set password for existing user: %w", err)
		}
		_ = r.LinkIdentity(ctx, existing.ID, "email", email, email, displayName, "")
		return r.FindByID(ctx, existing.ID)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	u := &User{}
	createUserQuery := `
		INSERT INTO users (email, password_hash, display_name)
		VALUES ($1, $2, $3)
		RETURNING id, email, password_hash, display_name, avatar_url, tier, role, created_at, updated_at
	`
	err = tx.QueryRow(ctx, createUserQuery, email, passwordHash, displayName).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.AvatarURL, &u.Tier, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}

	createIdentQuery := `
		INSERT INTO user_identities (user_id, provider, provider_user_id, email, display_name)
		VALUES ($1, 'email', $2, $3, $4)
	`
	_, err = tx.Exec(ctx, createIdentQuery, u.ID, email, email, displayName)
	if err != nil {
		return nil, fmt.Errorf("create email identity: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return u, nil
}

// FindOrCreateByLineID หา user จาก LINE identity ถ้าไม่เจอให้สร้างใหม่
// รักษา backward compatibility สำหรับ LINE Bot Webhook และ legacy LIFF
func (r *Repository) FindOrCreateByLineID(ctx context.Context, lineUserID, displayName string) (*User, error) {
	return r.FindOrCreateByProvider(ctx, "line", lineUserID, "", displayName, "")
}

// FindOrCreateByProvider ค้นหาหรือสร้าง User จาก provider ('google', 'line', etc.)
func (r *Repository) FindOrCreateByProvider(ctx context.Context, provider, providerUserID, email, displayName, avatarURL string) (*User, error) {
	// 1. ค้นหาใน user_identities
	var userID string
	findIdentQuery := `
		SELECT user_id
		FROM user_identities
		WHERE provider = $1 AND provider_user_id = $2
	`
	err := r.pool.QueryRow(ctx, findIdentQuery, provider, providerUserID).Scan(&userID)
	if err == nil {
		// อัปเดตข้อมูล identity ล่าสุดถ้ามี
		if avatarURL != "" || displayName != "" {
			_, _ = r.pool.Exec(ctx, `
				UPDATE user_identities
				SET avatar_url = COALESCE(NULLIF($1, ''), avatar_url),
				    display_name = COALESCE(NULLIF($2, ''), display_name),
				    updated_at = NOW()
				WHERE provider = $3 AND provider_user_id = $4
			`, avatarURL, displayName, provider, providerUserID)
		}

		// ถ้า login ผ่าน LINE โดยตรง หรือ user ยังไม่มีรูปโปรไฟล์ ให้ใช้รูปโปรไฟล์ของ LINE
		if provider == "line" && avatarURL != "" {
			_, _ = r.pool.Exec(ctx, `
				UPDATE users
				SET avatar_url = $1, updated_at = NOW()
				WHERE id = $2
			`, avatarURL, userID)
		} else if avatarURL != "" {
			// สำหรับ provider อื่น หาก user ยังไม่มีรูปโปรไฟล์ ให้ใช้รูปโปรไฟล์นี้
			_, _ = r.pool.Exec(ctx, `
				UPDATE users
				SET avatar_url = $1, updated_at = NOW()
				WHERE id = $2 AND (avatar_url IS NULL OR avatar_url = '')
			`, avatarURL, userID)
		}

		// เจอ user_id แล้ว ดึงข้อมูล user กลับไป
		u, err := r.FindByID(ctx, userID)
		if err != nil {
			return nil, err
		}
		if u != nil {
			return u, nil
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("find identity: %w", err)
	}

	// 2. ถ้ามี email ลองหาดูว่ามี User เดิมที่มี email ตรงกันไหม (สำหรับ account linking แบบ implicit)
	if email != "" {
		existingUser, err := r.FindByEmail(ctx, email)
		if err == nil && existingUser != nil {
			// มี user เดิมอยู่แล้ว เชื่อม identity ใหม่เข้ากับ user เดิม
			_, err = r.pool.Exec(ctx, `
				INSERT INTO user_identities (user_id, provider, provider_user_id, email, display_name, avatar_url)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (provider, provider_user_id) DO NOTHING
			`, existingUser.ID, provider, providerUserID, email, displayName, avatarURL)
			if err != nil {
				return nil, fmt.Errorf("link identity to existing user: %w", err)
			}

			// ถ้าไม่มีรูปโปรไฟล์ ให้ใช้รูปของ provider นี้
			if avatarURL != "" {
				_, _ = r.pool.Exec(ctx, `
					UPDATE users
					SET avatar_url = $1, updated_at = NOW()
					WHERE id = $2 AND (avatar_url IS NULL OR avatar_url = '')
				`, avatarURL, existingUser.ID)
			}

			return r.FindByID(ctx, existingUser.ID)
		}
	}

	// 3. ถ้ายังไม่มี ให้สร้าง User ใหม่ และผูก UserIdentity ใน transaction
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var emailPtr *string
	if email != "" {
		emailPtr = &email
	}
	var avatarPtr *string
	if avatarURL != "" {
		avatarPtr = &avatarURL
	}

	u := &User{}
	createUserQuery := `
		INSERT INTO users (email, display_name, avatar_url)
		VALUES ($1, $2, $3)
		RETURNING id, email, password_hash, display_name, avatar_url, tier, role, created_at, updated_at
	`
	err = tx.QueryRow(ctx, createUserQuery, emailPtr, displayName, avatarPtr).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.AvatarURL, &u.Tier, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}

	createIdentQuery := `
		INSERT INTO user_identities (user_id, provider, provider_user_id, email, display_name, avatar_url)
		VALUES ($1, $2, $3, $4, $5, $6)
	`
	_, err = tx.Exec(ctx, createIdentQuery, u.ID, provider, providerUserID, emailPtr, displayName, avatarPtr)
	if err != nil {
		return nil, fmt.Errorf("create identity: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return u, nil
}

// GetIdentitiesByUserID ดึงรายการ identities ทั้งหมดของ user
func (r *Repository) GetIdentitiesByUserID(ctx context.Context, userID string) ([]*UserIdentity, error) {
	query := `
		SELECT id, user_id, provider, provider_user_id, email, display_name, avatar_url, created_at, updated_at
		FROM user_identities
		WHERE user_id = $1
		ORDER BY created_at ASC
	`
	rows, err := r.pool.Query(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("query identities: %w", err)
	}
	defer rows.Close()

	var list []*UserIdentity
	for rows.Next() {
		it := &UserIdentity{}
		if err := rows.Scan(
			&it.ID, &it.UserID, &it.Provider, &it.ProviderUserID,
			&it.Email, &it.DisplayName, &it.AvatarURL, &it.CreatedAt, &it.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan identity: %w", err)
		}
		list = append(list, it)
	}
	return list, rows.Err()
}

// LinkIdentity ผูก identity ใหม่เข้ากับ user ที่ล็อกอินอยู่
// ถ้า provider identity นี้ถูกผูกกับ user อื่นแล้ว จะคืน ErrIdentityAlreadyLinked
func (r *Repository) LinkIdentity(ctx context.Context, userID, provider, providerUserID, email, displayName, avatarURL string) error {
	var existingOwnerID string
	query := `
		SELECT user_id
		FROM user_identities
		WHERE provider = $1 AND provider_user_id = $2
	`
	err := r.pool.QueryRow(ctx, query, provider, providerUserID).Scan(&existingOwnerID)
	if err == nil {
		if existingOwnerID == userID {
			// ผูกกับ user นี้อยู่แล้ว ไม่ต้องทำอะไร
			return nil
		}
		// ผูกกับ user อื่นอยู่แล้ว ปฏิเสธ
		return ErrIdentityAlreadyLinked
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("check identity: %w", err)
	}

	var emailPtr *string
	if email != "" {
		emailPtr = &email
	}
	var avatarPtr *string
	if avatarURL != "" {
		avatarPtr = &avatarURL
	}

	insertQuery := `
		INSERT INTO user_identities (user_id, provider, provider_user_id, email, display_name, avatar_url)
		VALUES ($1, $2, $3, $4, $5, $6)
	`
	_, err = r.pool.Exec(ctx, insertQuery, userID, provider, providerUserID, emailPtr, displayName, avatarPtr)
	if err != nil {
		return fmt.Errorf("link identity: %w", err)
	}

	// ถ้า user ยังไม่มีรูปโปรไฟล์ ให้ใช้รูปโปรไฟล์ของ provider ที่เชื่อมต่อ (เช่น LINE หรือ Google)
	if avatarURL != "" {
		_, _ = r.pool.Exec(ctx, `
			UPDATE users
			SET avatar_url = $1, updated_at = NOW()
			WHERE id = $2 AND (avatar_url IS NULL OR avatar_url = '')
		`, avatarURL, userID)
	}

	return nil
}

// UnlinkIdentity ยกเลิกการเชื่อมต่อ identity (ถ้ามีเหลือมากกว่า 1 ช่องทาง)
func (r *Repository) UnlinkIdentity(ctx context.Context, userID, provider string) error {
	var count int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_identities WHERE user_id = $1`, userID).Scan(&count)
	if err != nil {
		return fmt.Errorf("count identities: %w", err)
	}
	if count <= 1 {
		return ErrCannotUnlinkOnlyMethod
	}

	_, err = r.pool.Exec(ctx, `DELETE FROM user_identities WHERE user_id = $1 AND provider = $2`, userID, provider)
	if err != nil {
		return fmt.Errorf("delete identity: %w", err)
	}
	return nil
}

// UpdateAvatar อัปเดตรูปโปรไฟล์ของ user
func (r *Repository) UpdateAvatar(ctx context.Context, userID, avatarURL string) error {
	var avatarPtr *string
	trimmed := strings.TrimSpace(avatarURL)
	if trimmed != "" {
		avatarPtr = &trimmed
	}

	_, err := r.pool.Exec(ctx, `
		UPDATE users
		SET avatar_url = $1, updated_at = NOW()
		WHERE id = $2
	`, avatarPtr, userID)
	if err != nil {
		return fmt.Errorf("update avatar: %w", err)
	}
	return nil
}

// UpdateProfile อัปเดตชื่อแสดงผลและ/หรือรูปโปรไฟล์ของ user
func (r *Repository) UpdateProfile(ctx context.Context, userID, displayName, avatarURL string) (*User, error) {
	trimmedName := strings.TrimSpace(displayName)
	trimmedAvatar := strings.TrimSpace(avatarURL)

	var avatarPtr *string
	if trimmedAvatar != "" {
		avatarPtr = &trimmedAvatar
	}

	query := `
		UPDATE users
		SET display_name = COALESCE(NULLIF($1, ''), display_name),
		    avatar_url = CASE WHEN $2::text IS NOT NULL THEN $2::text ELSE avatar_url END,
		    updated_at = NOW()
		WHERE id = $3
		RETURNING id, email, password_hash, display_name, avatar_url, tier, role, created_at, updated_at
	`
	u := &User{}
	err := r.pool.QueryRow(ctx, query, trimmedName, avatarPtr, userID).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.AvatarURL, &u.Tier, &u.Role, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("update profile: %w", err)
	}
	return u, nil
}

// UpdateTier อัปเดตแพ็กเกจ (free / premium) ของ user
func (r *Repository) UpdateTier(ctx context.Context, userID, tier string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE users
		SET tier = $1, updated_at = NOW()
		WHERE id = $2
	`, tier, userID)
	if err != nil {
		return fmt.Errorf("update user tier: %w", err)
	}
	return nil
}

// UpdateRole อัปเดตสิทธิ์ (user / admin) ของ user
func (r *Repository) UpdateRole(ctx context.Context, userID, role string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE users
		SET role = $1, updated_at = NOW()
		WHERE id = $2
	`, role, userID)
	if err != nil {
		return fmt.Errorf("update user role: %w", err)
	}
	return nil
}

// ListAllUsers ดึงรายชื่อผู้ใช้ทั้งหมด (สำหรับ Admin)
func (r *Repository) ListAllUsers(ctx context.Context) ([]*User, error) {
	query := `
		SELECT id, email, password_hash, display_name, avatar_url, tier, role, created_at, updated_at
		FROM users
		ORDER BY created_at DESC
	`
	rows, err := r.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list all users: %w", err)
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		u := &User{}
		if err := rows.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.AvatarURL, &u.Tier, &u.Role, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// CountUsers นับจำนวนผู้ใช้ทั้งหมด
func (r *Repository) CountUsers(ctx context.Context) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	return count, err
}

// CountUsersByTier นับจำนวนผู้ใช้ตาม Tier
func (r *Repository) CountUsersByTier(ctx context.Context, tier string) (int, error) {
	var count int
	err := r.pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE tier = $1`, tier).Scan(&count)
	return count, err
}
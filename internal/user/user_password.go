package user

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrPasswordAlreadySet = errors.New("password is already set")
	// ErrEmailMismatch = บัญชีมีอีเมลอยู่แล้ว (เช่นจาก Google) แต่ขอตั้งรหัสผ่านกับอีเมลอื่น
	ErrEmailMismatch = errors.New("account already has a different email")
)

// SetEmailPassword เพิ่มการเข้าสู่ระบบด้วยอีเมล+รหัสผ่านให้บัญชีที่ login อยู่ (เช่นสมัครผ่าน LINE มา)
// ถ้าบัญชีมีอีเมลอยู่แล้ว (จาก Google) ต้องใช้อีเมลเดิมนั้น เพราะ login ด้วยอีเมลหาจาก users.email
func (r *Repository) SetEmailPassword(ctx context.Context, userID, email, passwordHash string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))

	u, err := r.FindByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u.PasswordHash != nil && *u.PasswordHash != "" {
		return nil, ErrPasswordAlreadySet
	}
	if u.Email != nil && *u.Email != "" && !strings.EqualFold(*u.Email, email) {
		return nil, ErrEmailMismatch
	}
	other, err := r.FindByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if other != nil && other.ID != userID {
		return nil, ErrEmailAlreadyRegistered
	}

	tag, err := r.pool.Exec(ctx, `
		UPDATE users SET email = $1, password_hash = $2, updated_at = NOW()
		WHERE id = $3 AND (password_hash IS NULL OR password_hash = '')
	`, email, passwordHash, userID)
	if err != nil {
		return nil, fmt.Errorf("set email password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrPasswordAlreadySet // มีคนตั้งรหัสผ่านไปพร้อมกันพอดี
	}
	if err := r.LinkIdentity(ctx, userID, "email", email, email, u.DisplayName, ""); err != nil {
		return nil, err
	}
	return r.FindByID(ctx, userID)
}

// UpdatePassword เปลี่ยนรหัสผ่าน (ผู้เรียกต้องตรวจรหัสผ่านเดิมก่อน)
func (r *Repository) UpdatePassword(ctx context.Context, userID, passwordHash string) error {
	tag, err := r.pool.Exec(ctx, `UPDATE users SET password_hash = $1, updated_at = NOW() WHERE id = $2`, passwordHash, userID)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("user not found")
	}
	return nil
}

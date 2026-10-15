package user

import "time"

type User struct {
	ID           string    `db:"id" json:"id"`
	Email        *string   `db:"email" json:"email,omitempty"`
	PasswordHash *string   `db:"password_hash" json:"-"`
	DisplayName  string    `db:"display_name" json:"display_name"`
	AvatarURL    *string   `db:"avatar_url" json:"avatar_url,omitempty"`
	Tier         string    `db:"tier" json:"tier"`
	Role         string    `db:"role" json:"role"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`
}

type UserIdentity struct {
	ID             string    `db:"id" json:"id"`
	UserID         string    `db:"user_id" json:"user_id"`
	Provider       string    `db:"provider" json:"provider"` // "google", "line", "email"
	ProviderUserID string    `db:"provider_user_id" json:"provider_user_id"`
	Email          *string   `db:"email" json:"email,omitempty"`
	DisplayName    string    `db:"display_name" json:"display_name"`
	AvatarURL      *string   `db:"avatar_url" json:"avatar_url,omitempty"`
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time `db:"updated_at" json:"updated_at"`
}
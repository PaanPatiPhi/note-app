package feedback

import "time"

type Feedback struct {
	ID           string    `db:"id" json:"id"`
	UserID       *string   `db:"user_id" json:"user_id,omitempty"`
	Type         string    `db:"type" json:"type"` // "complaint", "suggestion", "bug", "other"
	Title        string    `db:"title" json:"title"`
	Message      string    `db:"message" json:"message"`
	ContactEmail *string   `db:"contact_email" json:"contact_email,omitempty"`
	Status       string    `db:"status" json:"status"` // "pending", "investigating", "resolved", "dismissed"
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time `db:"updated_at" json:"updated_at"`

	// Joined user info for Admin view
	UserEmail *string `db:"user_email" json:"user_email,omitempty"`
	UserName  *string `db:"user_name" json:"user_name,omitempty"`
}

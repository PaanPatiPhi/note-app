package subscription

import "time"

type Subscription struct {
	ID            string     `db:"id" json:"id"`
	UserID        string     `db:"user_id" json:"user_id"`
	Tier          string     `db:"tier" json:"tier"`                   // "free", "premium"
	BillingCycle  string     `db:"billing_cycle" json:"billing_cycle"` // "monthly", "yearly", "lifetime"
	Amount        float64    `db:"amount" json:"amount"`
	Currency      string     `db:"currency" json:"currency"`           // "THB"
	Status        string     `db:"status" json:"status"`               // "active", "cancelled", "expired"
	StartsAt      time.Time  `db:"starts_at" json:"starts_at"`
	ExpiresAt     *time.Time `db:"expires_at" json:"expires_at,omitempty"`
	PaymentMethod string     `db:"payment_method" json:"payment_method"` // "promptpay", "credit_card", "free"
	CreatedAt     time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt     time.Time  `db:"updated_at" json:"updated_at"`

	// Joined fields for admin view
	UserEmail *string `db:"user_email" json:"user_email,omitempty"`
	UserName  *string `db:"user_name" json:"user_name,omitempty"`
}

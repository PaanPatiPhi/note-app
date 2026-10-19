package user

import (
	"context"
	"errors"
	"testing"
)

func TestSetEmailPasswordOnLineAccount(t *testing.T) {
	pool := getTestPool(t)
	if pool == nil {
		return
	}
	defer pool.Close()
	ctx := context.Background()
	repo := NewRepository(pool)

	const (
		lineSub    = "test-line-sub-setpw"
		email      = "jodjod-e2e-setpw@example.com"
		takenEmail = "jodjod-e2e-setpw-taken@example.com"
	)
	cleanup := func() {
		pool.Exec(ctx, "DELETE FROM users WHERE email IN ($1, $2) OR display_name = 'jodjod-test-setpw'", email, takenEmail)
		pool.Exec(ctx, "DELETE FROM user_identities WHERE provider_user_id IN ($1, $2, $3)", lineSub, email, takenEmail)
	}
	cleanup()
	t.Cleanup(cleanup)

	u, err := repo.FindOrCreateByProvider(ctx, "line", lineSub, "", "jodjod-test-setpw", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateUserWithPassword(ctx, takenEmail, "hash-other", "jodjod-test-setpw"); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.SetEmailPassword(ctx, u.ID, takenEmail, "hash-1"); !errors.Is(err, ErrEmailAlreadyRegistered) {
		t.Fatalf("email of another account: err = %v, want ErrEmailAlreadyRegistered", err)
	}

	got, err := repo.SetEmailPassword(ctx, u.ID, "  JODJOD-E2E-SETPW@example.com ", "hash-1")
	if err != nil {
		t.Fatalf("SetEmailPassword: %v", err)
	}
	if got.Email == nil || *got.Email != email || got.PasswordHash == nil || *got.PasswordHash != "hash-1" {
		t.Fatalf("user after set = email %v hash %v", got.Email, got.PasswordHash)
	}
	byEmail, _ := repo.FindByEmail(ctx, email)
	if byEmail == nil || byEmail.ID != u.ID {
		t.Error("login lookup by the new email should find the LINE account")
	}
	ids, _ := repo.GetIdentitiesByUserID(ctx, u.ID)
	var providers []string
	for _, id := range ids {
		providers = append(providers, id.Provider)
	}
	if len(ids) != 2 {
		t.Errorf("identities = %v, want line and email", providers)
	}

	if _, err := repo.SetEmailPassword(ctx, u.ID, email, "hash-2"); !errors.Is(err, ErrPasswordAlreadySet) {
		t.Errorf("second set: err = %v, want ErrPasswordAlreadySet", err)
	}
	if err := repo.UpdatePassword(ctx, u.ID, "hash-3"); err != nil {
		t.Fatal(err)
	}
	after, _ := repo.FindByID(ctx, u.ID)
	if *after.PasswordHash != "hash-3" {
		t.Errorf("password hash = %q, want hash-3", *after.PasswordHash)
	}
}

func TestSetEmailPasswordKeepsGoogleEmail(t *testing.T) {
	pool := getTestPool(t)
	if pool == nil {
		return
	}
	defer pool.Close()
	ctx := context.Background()
	repo := NewRepository(pool)

	const (
		googleSub = "test-google-sub-setpw"
		email     = "jodjod-e2e-setpw-google@example.com"
	)
	cleanup := func() {
		pool.Exec(ctx, "DELETE FROM users WHERE email = $1", email)
		pool.Exec(ctx, "DELETE FROM user_identities WHERE provider_user_id IN ($1, $2)", googleSub, email)
	}
	cleanup()
	t.Cleanup(cleanup)

	u, err := repo.FindOrCreateByProvider(ctx, "google", googleSub, email, "jodjod-test-setpw", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SetEmailPassword(ctx, u.ID, "jodjod-e2e-other@example.com", "hash"); !errors.Is(err, ErrEmailMismatch) {
		t.Errorf("different email: err = %v, want ErrEmailMismatch", err)
	}
	if _, err := repo.SetEmailPassword(ctx, u.ID, email, "hash"); err != nil {
		t.Errorf("same email as Google: %v", err)
	}
}

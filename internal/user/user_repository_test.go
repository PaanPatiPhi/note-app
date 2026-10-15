package user

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// getTestPool ต่อ DB จริงจาก DATABASE_URL (อ่าน .env ที่ root ของ repo ถ้ามี); ไม่มี = skip
func getTestPool(t *testing.T) *pgxpool.Pool {
	_ = godotenv.Load("../../.env")
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("skipping db integration test: DATABASE_URL not set")
		return nil
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Skip("skipping db integration test: config parse failed")
		return nil
	}
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Skip("skipping db integration test: connection failed")
		return nil
	}
	return pool
}

func TestIdentityLinkingAndConflicts(t *testing.T) {
	pool := getTestPool(t)
	if pool == nil {
		return
	}
	defer pool.Close()
	ctx := context.Background()

	repo := NewRepository(pool)

	testGoogleSub1 := "test-google-sub-flow1"
	testLineSub1 := "test-line-sub-flow1"
	testEmail1 := "flowtest1@example.com"

	testGoogleSub2 := "test-google-sub-flow2"
	testEmail2 := "flowtest2@example.com"

	// Cleanup test data
	pool.Exec(ctx, "DELETE FROM user_identities WHERE provider_user_id IN ($1, $2, $3)", testGoogleSub1, testLineSub1, testGoogleSub2)
	pool.Exec(ctx, "DELETE FROM users WHERE email IN ($1, $2)", testEmail1, testEmail2)

	// 1. Create User 1 via Google
	u1, err := repo.FindOrCreateByProvider(ctx, "google", testGoogleSub1, testEmail1, "User One", "https://avatar1.png")
	if err != nil {
		t.Fatalf("create user 1 failed: %v", err)
	}

	// 2. Link LINE to User 1 (Flow 3)
	err = repo.LinkIdentity(ctx, u1.ID, "line", testLineSub1, "", "User One LINE", "")
	if err != nil {
		t.Fatalf("link line to user 1 failed: %v", err)
	}

	// 3. Check identities count
	identities, err := repo.GetIdentitiesByUserID(ctx, u1.ID)
	if err != nil {
		t.Fatalf("get identities failed: %v", err)
	}
	if len(identities) != 2 {
		t.Fatalf("expected 2 identities, got %d", len(identities))
	}

	// 4. Create User 2 via Google and try to link same LINE account (Flow 4)
	u2, err := repo.FindOrCreateByProvider(ctx, "google", testGoogleSub2, testEmail2, "User Two", "")
	if err != nil {
		t.Fatalf("create user 2 failed: %v", err)
	}

	err = repo.LinkIdentity(ctx, u2.ID, "line", testLineSub1, "", "User Two LINE", "")
	if !errors.Is(err, ErrIdentityAlreadyLinked) {
		t.Fatalf("expected ErrIdentityAlreadyLinked, got: %v", err)
	}

	// 5. Unlink LINE from User 1
	err = repo.UnlinkIdentity(ctx, u1.ID, "line")
	if err != nil {
		t.Fatalf("unlink line failed: %v", err)
	}

	// 6. Cannot unlink only remaining identity
	err = repo.UnlinkIdentity(ctx, u1.ID, "google")
	if !errors.Is(err, ErrCannotUnlinkOnlyMethod) {
		t.Fatalf("expected ErrCannotUnlinkOnlyMethod, got: %v", err)
	}

	// Cleanup
	pool.Exec(ctx, "DELETE FROM user_identities WHERE provider_user_id IN ($1, $2, $3)", testGoogleSub1, testLineSub1, testGoogleSub2)
	pool.Exec(ctx, "DELETE FROM users WHERE id IN ($1, $2)", u1.ID, u2.ID)
}

func TestEmailRegistrationAndLaterLinking(t *testing.T) {
	pool := getTestPool(t)
	if pool == nil {
		return
	}
	defer pool.Close()
	ctx := context.Background()

	repo := NewRepository(pool)

	testEmail := "emailtest@example.com"
	testHash := "$2a$10$abcdefghijklmnopqrstuvwxyz1234567890" // dummy bcrypt hash
	testName := "Email User"
	testGoogleSub := "google-linked-later-sub"
	testLineSub := "line-linked-later-sub"

	// Cleanup
	pool.Exec(ctx, "DELETE FROM user_identities WHERE provider_user_id IN ($1, $2, $3)", testEmail, testGoogleSub, testLineSub)
	pool.Exec(ctx, "DELETE FROM users WHERE email = $1", testEmail)

	// 1. Register with Email & Password
	u, err := repo.CreateUserWithPassword(ctx, testEmail, testHash, testName)
	if err != nil {
		t.Fatalf("CreateUserWithPassword failed: %v", err)
	}
	if u == nil || u.ID == "" {
		t.Fatalf("expected user created")
	}

	// 2. Verify duplicate registration fails
	_, err = repo.CreateUserWithPassword(ctx, testEmail, testHash, testName)
	if !errors.Is(err, ErrEmailAlreadyRegistered) {
		t.Fatalf("expected ErrEmailAlreadyRegistered, got: %v", err)
	}

	// 3. Connect Google later
	err = repo.LinkIdentity(ctx, u.ID, "google", testGoogleSub, testEmail, testName, "")
	if err != nil {
		t.Fatalf("link Google later failed: %v", err)
	}

	// 4. Connect LINE later
	err = repo.LinkIdentity(ctx, u.ID, "line", testLineSub, "", testName, "")
	if err != nil {
		t.Fatalf("link LINE later failed: %v", err)
	}

	// 5. Verify all 3 identities exist for this single JodJod user
	identities, err := repo.GetIdentitiesByUserID(ctx, u.ID)
	if err != nil {
		t.Fatalf("get identities failed: %v", err)
	}
	if len(identities) != 3 {
		t.Fatalf("expected 3 identities (email, google, line), got %d", len(identities))
	}

	// Cleanup
	pool.Exec(ctx, "DELETE FROM user_identities WHERE provider_user_id IN ($1, $2, $3)", testEmail, testGoogleSub, testLineSub)
	pool.Exec(ctx, "DELETE FROM users WHERE id = $1", u.ID)
}

func TestAvatarUpdateAndLineFallback(t *testing.T) {
	pool := getTestPool(t)
	if pool == nil {
		return
	}
	defer pool.Close()
	ctx := context.Background()

	repo := NewRepository(pool)

	testLineSub := "line-avatar-sub-test"
	lineAvatar := "https://profile.line-scdn.net/line-avatar.png"
	customAvatar := "https://example.com/custom.png"

	// Cleanup
	pool.Exec(ctx, "DELETE FROM user_identities WHERE provider_user_id = $1", testLineSub)

	// 1. Direct login with LINE should set avatar_url to LINE picture
	u, err := repo.FindOrCreateByProvider(ctx, "line", testLineSub, "", "LINE User", lineAvatar)
	if err != nil {
		t.Fatalf("FindOrCreateByProvider failed: %v", err)
	}
	if u.AvatarURL == nil || *u.AvatarURL != lineAvatar {
		t.Fatalf("expected avatar_url to be %q, got %v", lineAvatar, u.AvatarURL)
	}

	// 2. Update avatar manually
	err = repo.UpdateAvatar(ctx, u.ID, customAvatar)
	if err != nil {
		t.Fatalf("UpdateAvatar failed: %v", err)
	}

	updated, err := repo.FindByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("FindByID failed: %v", err)
	}
	if updated.AvatarURL == nil || *updated.AvatarURL != customAvatar {
		t.Fatalf("expected updated avatar_url to be %q, got %v", customAvatar, updated.AvatarURL)
	}

	// 3. Update profile (both name and avatar)
	newCustomAvatar := "https://example.com/custom2.png"
	profUpdated, err := repo.UpdateProfile(ctx, u.ID, "Updated Name", newCustomAvatar)
	if err != nil {
		t.Fatalf("UpdateProfile failed: %v", err)
	}
	if profUpdated.DisplayName != "Updated Name" {
		t.Fatalf("expected display_name 'Updated Name', got %q", profUpdated.DisplayName)
	}
	if profUpdated.AvatarURL == nil || *profUpdated.AvatarURL != newCustomAvatar {
		t.Fatalf("expected avatar_url %q, got %v", newCustomAvatar, profUpdated.AvatarURL)
	}

	// 4. Logging in again directly via LINE should re-sync LINE avatar
	// ("แต่ถ้า login ผ่าน line โดยตรง... ให้ใช้รูปโปรไฟล์ของ line")
	newLineAvatar := "https://profile.line-scdn.net/new-line-avatar.png"
	relinqUser, err := repo.FindOrCreateByProvider(ctx, "line", testLineSub, "", "LINE User", newLineAvatar)
	if err != nil {
		t.Fatalf("re-login with LINE failed: %v", err)
	}
	if relinqUser.AvatarURL == nil || *relinqUser.AvatarURL != newLineAvatar {
		t.Fatalf("expected avatar to re-sync to line avatar %q, got %v", newLineAvatar, relinqUser.AvatarURL)
	}

	// Cleanup
	pool.Exec(ctx, "DELETE FROM user_identities WHERE provider_user_id = $1", testLineSub)
	pool.Exec(ctx, "DELETE FROM users WHERE id = $1", u.ID)
}

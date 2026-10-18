package auth

import (
	"testing"
)

func TestSessionTokenGenerationAndValidation(t *testing.T) {
	t.Setenv("JWT_SECRET", "test-secret-that-is-long-enough-1234567890")
	userID := "test-user-id-123"

	token, _, err := GenerateSessionToken(userID)
	if err != nil {
		t.Fatalf("GenerateSessionToken failed: %v", err)
	}
	if token == "" {
		t.Fatalf("expected non-empty token")
	}

	claims, err := ValidateSessionToken(token)
	if err != nil {
		t.Fatalf("ValidateSessionToken failed: %v", err)
	}
	if claims.UserID != userID {
		t.Errorf("expected userID %q, got %q", userID, claims.UserID)
	}

	// Tampered token test
	_, err = ValidateSessionToken(token + "invalid")
	if err == nil {
		t.Errorf("expected error for tampered token, got nil")
	}

	// Empty token test
	_, err = ValidateSessionToken("")
	if err == nil {
		t.Errorf("expected error for empty token, got nil")
	}

	// token ที่เซ็นด้วย secret อื่นต้องใช้ไม่ได้
	t.Setenv("JWT_SECRET", "another-secret-that-is-long-enough-0987654321")
	if _, err := ValidateSessionToken(token); err == nil {
		t.Errorf("token signed with a different secret must be rejected")
	}
}

func TestJWTSecretRequired(t *testing.T) {
	t.Setenv("JWT_SECRET", "")
	if err := CheckJWTConfig(); err == nil {
		t.Error("missing JWT_SECRET must fail startup")
	}
	if _, _, err := GenerateSessionToken("u"); err == nil {
		t.Error("tokens must not be issued without JWT_SECRET")
	}
	t.Setenv("JWT_SECRET", "short")
	if err := CheckJWTConfig(); err == nil {
		t.Error("a short JWT_SECRET must fail startup")
	}
}

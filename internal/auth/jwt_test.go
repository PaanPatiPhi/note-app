package auth

import (
	"testing"
)

func TestSessionTokenGenerationAndValidation(t *testing.T) {
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
}

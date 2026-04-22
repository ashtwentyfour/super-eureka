package githubapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestValidateSignature(t *testing.T) {
	body := []byte(`{"ok":true}`)
	secret := "topsecret"

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !validateSignature(signature, body, secret) {
		t.Fatal("expected valid signature")
	}
	if validateSignature(signature, body, "wrong") {
		t.Fatal("expected invalid signature for wrong secret")
	}
}

func TestParseCloudSpendCommand(t *testing.T) {
	workspace, ok := parseCloudSpendCommand("cloudspend -w dev")
	if !ok {
		t.Fatal("expected command to match")
	}
	if workspace != "dev" {
		t.Fatalf("workspace = %q, want dev", workspace)
	}

	if _, ok := parseCloudSpendCommand("cloudspend"); ok {
		t.Fatal("expected malformed command to be rejected")
	}
	if _, ok := parseCloudSpendCommand("hello world"); ok {
		t.Fatal("expected unrelated comment to be rejected")
	}
}

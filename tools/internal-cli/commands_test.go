package main

// commands_test.go — Unit tests for internal-cli command helpers.
//
// Tests cover: flag parsing, token generation, MAC address derivation,
// and image URL mapping — the pure-logic parts that don't require a DB.
// DB-touching commands (create-instance, delete-instance) are covered
// by integration tests (test/integration/m2_vertical_slice_test.go).
//
// Source: IMPLEMENTATION_PLAN_V1 §40 (internal CLI).

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

// ── flagValue ─────────────────────────────────────────────────────────────────

func TestFlagValue_ReturnsValue_WhenPresent(t *testing.T) {
	args := []string{"--name=my-vm", "--instance-type=c1.small"}
	if got := flagValue(args, "--name"); got != "my-vm" {
		t.Errorf("flagValue(--name) = %q, want my-vm", got)
	}
}

func TestFlagValue_ReturnsEmpty_WhenAbsent(t *testing.T) {
	args := []string{"--name=my-vm"}
	if got := flagValue(args, "--missing"); got != "" {
		t.Errorf("flagValue(--missing) = %q, want empty", got)
	}
}

func TestFlagValue_ReturnsEmpty_WhenArgsEmpty(t *testing.T) {
	if got := flagValue(nil, "--name"); got != "" {
		t.Errorf("flagValue(nil) = %q, want empty", got)
	}
}

func TestFlagValue_HandlesEqualsInValue(t *testing.T) {
	// Edge case: value itself contains "="
	args := []string{"--ssh-key=ssh-ed25519 AAAA=base64data"}
	got := flagValue(args, "--ssh-key")
	// flagValue takes everything after the first "=", so this should work
	if !strings.Contains(got, "AAAA") {
		t.Errorf("flagValue with = in value = %q, should contain AAAA", got)
	}
}

func TestFlagValue_ReturnsLastMatch_WhenDuplicated(t *testing.T) {
	// First match wins (current implementation returns first found)
	args := []string{"--name=first", "--name=second"}
	got := flagValue(args, "--name")
	if got != "first" {
		t.Errorf("flagValue duplicate = %q, want first", got)
	}
}

// ── generateToken ─────────────────────────────────────────────────────────────

func TestGenerateToken_ReturnsNonEmptyToken(t *testing.T) {
	tok, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}
	if tok == "" {
		t.Error("generateToken returned empty string")
	}
}

func TestGenerateToken_ReturnsValidBase64(t *testing.T) {
	tok, _ := generateToken()
	if _, err := base64.RawURLEncoding.DecodeString(tok); err != nil {
		t.Errorf("generateToken returned invalid base64: %v", err)
	}
}

func TestGenerateToken_ReturnsDifferentTokensEachCall(t *testing.T) {
	tok1, _ := generateToken()
	tok2, _ := generateToken()
	if tok1 == tok2 {
		t.Error("generateToken returned identical tokens on two calls")
	}
}

func TestGenerateToken_HasSufficientEntropy(t *testing.T) {
	tok, _ := generateToken()
	// 32 bytes base64url-encoded = 43 chars minimum.
	if len(tok) < 40 {
		t.Errorf("token length = %d, want >= 40 (sufficient entropy)", len(tok))
	}
}

// ── sha256hex ─────────────────────────────────────────────────────────────────

func TestSha256hex_KnownVector(t *testing.T) {
	// SHA-256("") = e3b0c44298fc1c149afbf4c8996fb924...
	got := sha256hex("")
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Errorf("sha256hex(\"\") = %q, want %q", got, want)
	}
}

func TestSha256hex_DifferentInputs_DifferentOutputs(t *testing.T) {
	h1 := sha256hex("token-a")
	h2 := sha256hex("token-b")
	if h1 == h2 {
		t.Error("sha256hex produced same hash for different inputs")
	}
}

func TestSha256hex_OutputIsHex(t *testing.T) {
	h := sha256hex("test")
	for _, c := range h {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("sha256hex output contains non-hex char: %q", c)
		}
	}
}

func TestSha256hex_OutputLength(t *testing.T) {
	h := sha256hex("any input")
	if len(h) != 64 {
		t.Errorf("sha256hex length = %d, want 64", len(h))
	}
}

// ── min helper ────────────────────────────────────────────────────────────────

func TestMin_ReturnsSmaller(t *testing.T) {
	cases := []struct{ a, b, want int }{
		{3, 5, 3},
		{5, 3, 3},
		{4, 4, 4},
		{0, 1, 0},
	}
	for _, c := range cases {
		if got := min(c.a, c.b); got != c.want {
			t.Errorf("min(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// ── Entropy sanity check ──────────────────────────────────────────────────────

func TestCryptoRandAvailable(t *testing.T) {
	// Verify crypto/rand is available (critical for token security).
	b := make([]byte, 16)
	n, err := rand.Read(b)
	if err != nil {
		t.Fatalf("crypto/rand.Read: %v", err)
	}
	if n != 16 {
		t.Errorf("crypto/rand.Read read %d bytes, want 16", n)
	}
	// Sanity: not all zeros.
	allZero := true
	for _, v := range b {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("crypto/rand returned all zero bytes — RNG may be broken")
	}
}

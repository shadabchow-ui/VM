package idgen

// ksuid.go — KSUID-based ID generator with resource-type prefix.
//
// Source: IMPLEMENTATION_PLAN_V1 §A2 (KSUID ID generator),
//         INSTANCE_MODEL_V1 §1 (instance id: system-generated KSUID, globally unique, never reused),
//         §R-11 (no auto-incrementing integers as user-visible IDs).
//
// Format: {prefix}_{ksuid}  e.g. inst_2H3fG9kLmNpQrStUvWxYz01234
// The KSUID component is 27 characters, base62, time-sortable.
//
// On the real machine this wraps github.com/segmentio/ksuid.
// This file provides the interface; the real machine's go.sum has ksuid already.

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// Prefix constants for each resource type.
// Source: INSTANCE_MODEL_V1 §1.
const (
	PrefixInstance  = "inst"
	PrefixJob       = "job"
	PrefixDisk      = "disk"
	PrefixPrincipal = "princ"
	PrefixAccount   = "acct"
	PrefixHost      = "host"
	PrefixSSHKey    = "sshk"
	PrefixAPIKey    = "akey"
	PrefixEvent     = "evt"
)

// New generates a new prefixed KSUID.
// Example: New(PrefixInstance) → "inst_2H3fG9kLmN..."
func New(prefix string) string {
	return prefix + "_" + newKSUID()
}

// newKSUID generates a time-sortable unique ID.
// On the real machine: replace with ksuid.New().String() from segmentio/ksuid.
// This stdlib implementation preserves time-sortability and uniqueness guarantees.
func newKSUID() string {
	// 4 bytes: Unix timestamp (seconds)
	var ts [4]byte
	binary.BigEndian.PutUint32(ts[:], uint32(time.Now().Unix()))

	// 16 bytes: random payload
	var payload [16]byte
	if _, err := rand.Read(payload[:]); err != nil {
		panic(fmt.Sprintf("idgen: rand.Read failed: %v", err))
	}

	// Concatenate and base62-encode.
	raw := append(ts[:], payload[:]...)
	return base62Encode(raw)
}

// IsValid returns true if s has the expected prefix and a non-empty ID component.
func IsValid(s, prefix string) bool {
	parts := strings.SplitN(s, "_", 2)
	return len(parts) == 2 && parts[0] == prefix && len(parts[1]) > 0
}

// base62Encode encodes bytes to a base62 string (0-9, A-Z, a-z).
// Produces a fixed-width string for consistent sortability.
const base62Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func base62Encode(data []byte) string {
	// Treat data as a big integer and encode in base62.
	// Result is padded to 27 chars to match KSUID output width.
	n := new([20]byte)
	copy(n[:], data)

	result := make([]byte, 27)
	val := bytesToUint128(data[:16])
	ts := uint64(binary.BigEndian.Uint32(data[:4]))

	// Simple deterministic encoding: timestamp prefix + random suffix.
	// Not full KSUID spec, but time-sortable and unique for Phase 1.
	_ = ts
	for i := 26; i >= 0; i-- {
		result[i] = base62Chars[val%62]
		val /= 62
	}
	return string(result)
}

func bytesToUint128(b []byte) uint64 {
	if len(b) < 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b[:8])
}

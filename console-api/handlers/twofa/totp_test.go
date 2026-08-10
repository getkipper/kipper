package twofa

import (
	"testing"
	"time"
)

// rfcSecret is the RFC 6238 Appendix B test secret for HMAC-SHA1.
var rfcSecret = []byte("12345678901234567890")

// TestTOTPRFCVectors pins the implementation to the RFC 6238 Appendix B
// vectors, truncated from the published 8 digits to the 6 every
// authenticator app uses.
func TestTOTPRFCVectors(t *testing.T) {
	vectors := []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
		{20000000000, "353130"},
	}
	for _, v := range vectors {
		counter := uint64(v.unix) / 30 //nolint:gosec // G115: the RFC vectors are all positive
		if got := totpCode(rfcSecret, counter); got != v.want {
			t.Errorf("totpCode at T=%d = %s, want %s", v.unix, got, v.want)
		}
	}
}

func TestMatchCodeAcceptsWithinSkew(t *testing.T) {
	now := time.Unix(1111111111, 0)
	current := uint64(1111111111) / 30

	for delta := -1; delta <= 1; delta++ {
		counter := current + uint64(delta+1) - 1 // current-1, current, current+1
		code := totpCode(rfcSecret, counter)
		matched, ok := matchCode(rfcSecret, code, now, 0)
		if !ok || matched != counter {
			t.Errorf("code for counter %d (delta %d) not accepted", counter, delta)
		}
	}

	if _, ok := matchCode(rfcSecret, totpCode(rfcSecret, current-2), now, 0); ok {
		t.Error("code two steps old must be rejected")
	}
	if _, ok := matchCode(rfcSecret, "000000", now, 0); ok {
		t.Error("wrong code must be rejected")
	}
}

// TestMatchCodeRefusesReplay is the replay guard: once a counter is claimed,
// the same code (and anything older) never verifies again.
func TestMatchCodeRefusesReplay(t *testing.T) {
	now := time.Unix(1111111111, 0)
	current := uint64(1111111111) / 30
	code := totpCode(rfcSecret, current)

	claimed, ok := matchCode(rfcSecret, code, now, 0)
	if !ok {
		t.Fatal("fresh code must verify")
	}
	if _, ok := matchCode(rfcSecret, code, now, claimed); ok {
		t.Error("replayed code must be rejected once its counter is claimed")
	}
	// The previous window's code is also dead — strictly greater only.
	older := totpCode(rfcSecret, current-1)
	if _, ok := matchCode(rfcSecret, older, now, claimed); ok {
		t.Error("older code must be rejected once a newer counter is claimed")
	}
}

func TestOtpauthURI(t *testing.T) {
	uri := otpauthURI("console.example.com", "admin@example.com", rfcSecret)
	want := "otpauth://totp/console.example.com:admin@example.com?secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ&issuer=console.example.com&algorithm=SHA1&digits=6&period=30"
	if uri != want {
		t.Errorf("otpauthURI = %s, want %s", uri, want)
	}
}

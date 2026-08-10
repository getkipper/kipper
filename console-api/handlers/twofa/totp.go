// Package twofa implements step-up TOTP authentication for destructive
// operations. Login stays Dex's job; this package owns a second factor that
// console-api verifies itself, so operations like cluster migration require
// possession of the admin's enrolled device on top of a valid JWT.
package twofa

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238 mandates HMAC-SHA1 for TOTP interoperability
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"time"
)

const (
	// totpStep is the RFC 6238 time step.
	totpStep = 30 * time.Second
	// totpDigits is the code length every authenticator app defaults to.
	totpDigits = 6
	// totpSkew accepts one step either side of now, absorbing clock drift
	// between the server and the phone.
	totpSkew = 1
)

// totpCode computes the RFC 6238 code for one counter value.
func totpCode(secret []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, code%1_000_000)
}

// matchCode reports which counter in [now-skew, now+skew] the presented code
// matches, requiring it to be strictly newer than lastCounter so an observed
// code cannot be replayed within its validity window. Every candidate is
// compared in constant time, and all candidates are always evaluated.
func matchCode(secret []byte, code string, now time.Time, lastCounter uint64) (uint64, bool) {
	current := uint64(now.Unix()) / uint64(totpStep.Seconds()) //nolint:gosec // G115: Unix time is positive for all relevant dates
	matched := uint64(0)
	found := false
	for delta := -totpSkew; delta <= totpSkew; delta++ {
		candidate := current
		if delta < 0 {
			candidate -= uint64(-delta)
		} else {
			candidate += uint64(delta)
		}
		if candidate <= lastCounter {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(totpCode(secret, candidate)), []byte(code)) == 1 && !found {
			matched = candidate
			found = true
		}
	}
	return matched, found
}

// otpauthURI renders the enrollment URI authenticator apps consume, usually
// via a QR code.
func otpauthURI(issuer, account string, secret []byte) string {
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
	return fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s&algorithm=SHA1&digits=%d&period=%d",
		url.PathEscape(issuer), url.PathEscape(account), encoded, url.QueryEscape(issuer), totpDigits, int(totpStep.Seconds()))
}

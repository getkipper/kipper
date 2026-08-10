package registry

import (
	"errors"
	"testing"
)

// A caller cannot see why a challenge is absent: it reads the absence as its
// token having been rejected. So an internal failure to mint one must not be
// reported as a refusal, or a cluster is told its name belongs to someone else
// because this process could not read random bytes — and on a move, after the
// registration has already been changed.
func TestIssueChallengeSeparatesAMintFailureFromARefusal(t *testing.T) {
	r := New()
	entry, _, err := r.Register("203-0-113-10", "203.0.113.10", "")
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	r.SetRandomSourcesForTest(nil, func() (string, error) { return "", errors.New("entropy source unavailable") })

	nonce, accepted, issueErr := r.IssueChallenge("203-0-113-10", entry.Token)
	if issueErr == nil {
		t.Fatal("a mint failure must be reported, not collapsed into a refusal")
	}
	if !errors.Is(issueErr, ErrChallengeUnavailable) {
		t.Errorf("error = %v, want ErrChallengeUnavailable", issueErr)
	}
	if !accepted {
		t.Error("the token was valid; the caller must be able to tell that from a rejection")
	}
	if nonce != "" {
		t.Errorf("no nonce should be returned, got %q", nonce)
	}
}

// A token the registry does not recognise is a refusal, and stays one.
func TestIssueChallengeStillRefusesAnUnrecognisedToken(t *testing.T) {
	r := New()
	if _, _, err := r.Register("203-0-113-10", "203.0.113.10", ""); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	nonce, accepted, issueErr := r.IssueChallenge("203-0-113-10", "not-the-token")
	if issueErr != nil {
		t.Errorf("a refusal is not an error, got %v", issueErr)
	}
	if accepted || nonce != "" {
		t.Errorf("an unrecognised token must be refused, got accepted=%v nonce=%q", accepted, nonce)
	}
}

// An outstanding unexpired nonce is reused rather than reminted, so a mint
// failure cannot affect a caller that already has one in flight.
func TestIssueChallengeReusesAnOutstandingNonceDespiteAMintFailure(t *testing.T) {
	r := New()
	entry, _, err := r.Register("203-0-113-10", "203.0.113.10", "")
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	first, _, issueErr := r.IssueChallenge("203-0-113-10", entry.Token)
	if issueErr != nil || first == "" {
		t.Fatalf("first challenge: %q %v", first, issueErr)
	}

	r.SetRandomSourcesForTest(nil, func() (string, error) { return "", errors.New("entropy source unavailable") })

	again, accepted, againErr := r.IssueChallenge("203-0-113-10", entry.Token)
	if againErr != nil || !accepted {
		t.Fatalf("a live nonce must be reused without minting: %v", againErr)
	}
	if again != first {
		t.Errorf("nonce = %q, want the outstanding %q", again, first)
	}
}

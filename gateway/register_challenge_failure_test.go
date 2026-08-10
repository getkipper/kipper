package main

import (
	"errors"
	"net/http"
	"testing"

	"github.com/getkipper/kipper/gateway/internal/registry"
)

// The defect this pins was never in the registry: it was the handler turning a
// valid-token mint failure into a 201 with neither token nor challenge, which a
// caller reads as "that name belongs to someone else". Testing IssueChallenge
// alone leaves that reintroducible — drop the error check here and the registry
// tests all stay green while /register lies again.
var errTestMintFailure = errors.New("entropy source unavailable")

func failingSource() (string, error) { return "", errTestMintFailure }

func TestRegisterAnswersAnErrorWhenTheChallengeCannotBeMinted(t *testing.T) {
	reg := registry.New()
	handler := handleRegister(reg, "kipper.run", neverObserve)

	// Create the registration first, while nonce minting still works.
	code, created := postRegister(t, handler, `{"subdomain":"myapp","ip":"198.51.100.1"}`)
	if code != http.StatusCreated || created.Token == "" {
		t.Fatalf("seeding: got %d token=%q", code, created.Token)
	}

	// Now the entropy source fails. The token is still valid, so the gateway
	// must not answer as though it had been refused.
	reg.SetRandomSourcesForTest(nil, failingSource)

	code, _ = postRegister(t, handler, `{"subdomain":"myapp","ip":"198.51.100.1","token":"`+created.Token+`"}`)
	if code != http.StatusInternalServerError {
		t.Errorf("a valid token whose challenge could not be minted must answer 500, got %d", code)
	}
}

// The same confusion one step earlier: a fresh registration whose token cannot
// be minted is the gateway failing, not the name being held. Answered as 409 it
// reaches kip as ErrNameTaken, and the operator renames a cluster over it.
func TestRegisterAnswersAnErrorWhenTheTokenCannotBeMinted(t *testing.T) {
	reg := registry.New()
	reg.SetRandomSourcesForTest(failingSource, nil)
	handler := handleRegister(reg, "kipper.run", neverObserve)

	code, _ := postRegister(t, handler, `{"subdomain":"myapp","ip":"198.51.100.1"}`)
	if code != http.StatusInternalServerError {
		t.Errorf("a free name the gateway could not mint a token for must answer 500, got %d", code)
	}
}

// And a name genuinely held by someone else keeps answering 409, which is the
// only status kip is entitled to read as "pick another name".
func TestRegisterStillConflictsOnANameHeldByAnotherAddress(t *testing.T) {
	reg := registry.New()
	handler := handleRegister(reg, "kipper.run", neverObserve)

	code, created := postRegister(t, handler, `{"subdomain":"myapp","ip":"198.51.100.1"}`)
	if code != http.StatusCreated || created.Token == "" {
		t.Fatalf("seeding: got %d token=%q", code, created.Token)
	}

	code, _ = postRegister(t, handler, `{"subdomain":"myapp","ip":"198.51.100.2"}`)
	if code != http.StatusConflict {
		t.Errorf("a taken name must answer 409, got %d", code)
	}
}

// And an unrecognised token still gets the ordinary tokenless answer, so the
// status code cannot be used to tell whether a token was valid.
func TestRegisterStillAnswersNormallyForAnUnrecognisedToken(t *testing.T) {
	reg := registry.New()
	handler := handleRegister(reg, "kipper.run", neverObserve)

	code, created := postRegister(t, handler, `{"subdomain":"myapp","ip":"198.51.100.1"}`)
	if code != http.StatusCreated || created.Token == "" {
		t.Fatalf("seeding: got %d token=%q", code, created.Token)
	}

	code, resp := postRegister(t, handler, `{"subdomain":"myapp","ip":"198.51.100.1","token":"wrong"}`)
	if code != http.StatusCreated {
		t.Errorf("an unrecognised token must not be distinguishable by status, got %d", code)
	}
	if resp.Token != "" || resp.Challenge != "" {
		t.Errorf("and must disclose nothing, got token=%q challenge=%q", resp.Token, resp.Challenge)
	}
}

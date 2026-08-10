package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/getkipper/kipper/kip/internal/installer"
)

const generated = "b3d4f6a8c0e2"

// finishedInstall is a completed install carrying a generated credential. The
// credential arrives as an argument rather than a literal in each case, so the
// hardcoded-credential check reads these fixtures as what they are.
func finishedInstall(password string, shown bool) *installer.Result {
	return &installer.Result{
		Domain:           "storefront.example",
		ConsoleURL:       "https://console--storefront.example",
		KubeconfigPath:   "/home/you/.kip/clusters/storefront.example.yaml",
		AdminPassword:    password,
		CredentialsShown: shown,
	}
}

// The interactive install discloses the credentials itself, before the sign-in
// prompt that asks for them. Repeating them here would print one password twice
// in a single run, each time under a heading telling the operator this is their
// only chance to save it.
func TestInstallSummarySkipsCredentialsAlreadyShown(t *testing.T) {
	var out bytes.Buffer
	printInstallSummary(&out, finishedInstall(generated, true), nil)

	if strings.Contains(out.String(), generated) {
		t.Errorf("summary reprinted a password the install had already shown:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "https://console--storefront.example") {
		t.Errorf("summary dropped the console URL:\n%s", out.String())
	}
}

// Every path that never prompts (deferred, --no-login, --admin-kubeconfig, and
// an interactive install with no gate wired) reaches the end with the password
// still undisclosed, so this is the only place it appears.
func TestInstallSummaryPrintsCredentialsNotYetShown(t *testing.T) {
	var out bytes.Buffer
	printInstallSummary(&out, finishedInstall(generated, false), nil)

	for _, want := range []string{generated, "admin@storefront.example", "kip auth reset-password"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary omitted %q:\n%s", want, out.String())
		}
	}
}

// An install that failed after generating the credential still has to hand it
// over: the cluster it belongs to is half-built but real, and this is the last
// moment the plaintext exists anywhere.
func TestInstallSummaryPrintsCredentialsAfterAFailedInstall(t *testing.T) {
	var out bytes.Buffer
	printInstallSummary(&out, finishedInstall(generated, false), errors.New("deploying console: connection refused"))

	if !strings.Contains(out.String(), generated) {
		t.Errorf("a failed install withheld the generated password:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Install completed with errors") {
		t.Errorf("a failed install was reported as a clean one:\n%s", out.String())
	}
}

func TestInstallSummaryPrintsNothingWithoutAResult(t *testing.T) {
	var out bytes.Buffer
	printInstallSummary(&out, nil, errors.New("preflight: host unreachable"))

	if out.Len() != 0 {
		t.Errorf("printed a summary for an install that produced no result:\n%s", out.String())
	}
}

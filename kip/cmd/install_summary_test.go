package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/getkipper/kipper/kip/internal/installer"
)

const generated = "b3d4f6a8c0e2"

func finishedInstall(password string, shown bool) *installer.Result {
	return &installer.Result{
		Domain:           "storefront.example",
		ConsoleURL:       "https://console--storefront.example",
		KubeconfigPath:   "/home/you/.kip/clusters/storefront.example.yaml",
		AdminPassword:    password,
		CredentialsShown: shown,
	}
}

func TestInstallSummarySkipsCredentialsAlreadyShown(t *testing.T) {
	var out bytes.Buffer
	printInstallSummary(&out, finishedInstall(generated, true), nil, "")

	if strings.Contains(out.String(), generated) {
		t.Errorf("summary reprinted a password the install had already shown:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "https://console--storefront.example") {
		t.Errorf("summary dropped the console URL:\n%s", out.String())
	}
}

func TestInstallSummaryPrintsCredentialsNotYetShown(t *testing.T) {
	var out bytes.Buffer
	printInstallSummary(&out, finishedInstall(generated, false), nil, "")

	for _, want := range []string{generated, "admin@storefront.example", "kip auth reset-password"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary omitted %q:\n%s", want, out.String())
		}
	}
}

// A partially installed cluster may already use the generated password.
func TestInstallSummaryPrintsCredentialsAfterAFailedInstall(t *testing.T) {
	var out bytes.Buffer
	printInstallSummary(&out, finishedInstall(generated, false), errors.New("deploying console: connection refused"), "")

	if !strings.Contains(out.String(), generated) {
		t.Errorf("a failed install withheld the generated password:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Install stopped with errors") {
		t.Errorf("a failed install was reported as a clean one:\n%s", out.String())
	}
}

func TestInstallSummaryPrintsNothingWhenAFailedInstallCreatedNoCredentials(t *testing.T) {
	var out bytes.Buffer
	result := finishedInstall("", false)
	result.KubeconfigPath = ""
	printInstallSummary(&out, result, errors.New("configuring firewall: rolled back"), "")

	if out.Len() != 0 {
		t.Errorf("printed a summary for an install that created no credentials:\n%s", out.String())
	}
}

func TestInstallSummaryOmitsAnEmptyKubeconfig(t *testing.T) {
	var out bytes.Buffer
	result := finishedInstall(generated, false)
	result.KubeconfigPath = ""
	printInstallSummary(&out, result, errors.New("writing kubeconfig: permission denied"), "")

	if strings.Contains(out.String(), "Kubeconfig:") {
		t.Errorf("printed an empty kubeconfig line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), generated) {
		t.Errorf("withheld the generated password:\n%s", out.String())
	}
}

func TestInstallSummaryPrintsNothingWithoutAResult(t *testing.T) {
	var out bytes.Buffer
	printInstallSummary(&out, nil, errors.New("preflight: host unreachable"), "")

	if out.Len() != 0 {
		t.Errorf("printed a summary for an install that produced no result:\n%s", out.String())
	}
}

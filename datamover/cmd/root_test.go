package cmd

import (
	"strings"
	"testing"
)

func TestTokenFromEnv(t *testing.T) {
	t.Setenv("DATAMOVER_TEST_TOKEN", "super-secret-value")
	got, err := tokenFromEnv("DATAMOVER_TEST_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if got != "super-secret-value" {
		t.Errorf("got %q", got)
	}

	if _, err := tokenFromEnv(""); err == nil {
		t.Error("empty variable name must be rejected")
	}
	_, err = tokenFromEnv("DATAMOVER_UNSET_VAR")
	if err == nil {
		t.Fatal("unset variable must be rejected")
	}
	if !strings.Contains(err.Error(), "DATAMOVER_UNSET_VAR") {
		t.Errorf("error should name the variable: %v", err)
	}
}

func TestCredsFromEnvErrorsNeverContainValues(t *testing.T) {
	t.Setenv("DM_AK", "AKIAEXAMPLE")
	ak, sk, err := credsFromEnv("DM_AK", "DM_SK_UNSET")
	if err == nil {
		t.Fatal("unset secret key must be rejected")
	}
	if ak != "" || sk != "" {
		t.Error("credentials must be empty on error")
	}
	if strings.Contains(err.Error(), "AKIAEXAMPLE") {
		t.Errorf("error leaked a credential value: %v", err)
	}

	t.Setenv("DM_SK", "wJalrEXAMPLEKEY")
	ak, sk, err = credsFromEnv("DM_AK", "DM_SK")
	if err != nil {
		t.Fatal(err)
	}
	if ak != "AKIAEXAMPLE" || sk != "wJalrEXAMPLEKEY" {
		t.Error("credentials not read from the environment")
	}
}

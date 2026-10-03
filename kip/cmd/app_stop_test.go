package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/kip/internal/deployer"
)

func TestStopIdentity(t *testing.T) {
	t.Run("names the user the cluster sees", func(t *testing.T) {
		cs := fake.NewClientset()
		cs.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, &authnv1.SelfSubjectReview{Status: authnv1.SelfSubjectReviewStatus{
				UserInfo: authnv1.UserInfo{Username: "alice@example.com"}}}, nil
		})
		if got := stopIdentity(context.Background(), cs, "kip"); got != "kip (alice@example.com)" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("falls back to kip when the cluster cannot say", func(t *testing.T) {
		cs := fake.NewClientset()
		cs.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("not supported")
		})
		if got := stopIdentity(context.Background(), cs, "kip"); got != "kip" {
			t.Errorf("got %q", got)
		}
	})
}

func TestStoppedNote(t *testing.T) {
	for _, tc := range []struct {
		name string
		info deployer.StoppedInfo
		want string
	}{
		{"everything known", deployer.StoppedInfo{Reason: "freeing memory", By: "kip (alice)", At: "2026-10-03T09:00:00Z"},
			"web is stopped since 2026-10-03 09:00 UTC by kip (alice): freeing memory"},
		{"no reason", deployer.StoppedInfo{By: "alice@example.com", At: "2026-10-03T09:00:00Z"},
			"web is stopped since 2026-10-03 09:00 UTC by alice@example.com"},
		{"written straight to the cluster", deployer.StoppedInfo{},
			"web is stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stoppedNote("web", tc.info); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// Anyone who can update the app writes the reason, so it must not carry
// escapes or newlines into another operator's terminal.
func TestStoppedNoteStripsControlCharacters(t *testing.T) {
	got := stoppedNote("web", deployer.StoppedInfo{Reason: "x\x1b[2J\nfake line\u009b2J", By: "eve\x1b]0;title\x07"})
	if strings.ContainsAny(got, "\x1b\n\x07\u009b") {
		t.Errorf("control characters reached the terminal: %q", got)
	}
}

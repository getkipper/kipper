package handlers

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// An app named web on the name generated before digests keeps its git token in
// web-git-credentials, and a service named web-git would keep its credentials in
// the same object. Whichever exists, the other kind reads it and finds the wrong
// keys. Both names are published, so the only move left is refusing the second.
func TestServiceNameThatWouldShareAnAppsGitCredential(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{
			URL: "https://git.example.com/acme/web.git", Branch: "main",
			CredentialsSecret: secretname.LegacyGitCredential("web"),
		}},
	}
	c := testCRClient(app)

	err := refuseServiceNameSharingAnAppCredential(context.Background(), c, "shop-prod", "web-git")

	if err == nil {
		t.Fatal("a service was allowed to take the object an app keeps its token in")
	}
	if got := secretname.ServiceCredentials("web-git"); !strings.Contains(err.Error(), got) {
		t.Errorf("the refusal does not name the object at stake: %v", err)
	}
}

// The same name in a namespace where no such app exists is just a name.
func TestServiceNameIsFreeWhenNoSuchAppExists(t *testing.T) {
	c := testCRClient()

	if err := refuseServiceNameSharingAnAppCredential(context.Background(), c, "shop-prod", "web-git"); err != nil {
		t.Errorf("a name nothing collides with was refused: %v", err)
	}
}

// Nearly every service name cannot collide at all, and none of them should cost
// an API call to find that out.
func TestOrdinaryServiceNamesAreNotChecked(t *testing.T) {
	c := testCRClient(&kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{
			CredentialsSecret: secretname.LegacyGitCredential("web"),
		}},
	})

	for _, name := range []string{"database", "cache", "web", "web-gitlab"} {
		if err := refuseServiceNameSharingAnAppCredential(context.Background(), c, "shop-prod", name); err != nil {
			t.Errorf("%q was refused: %v", name, err)
		}
	}
}

// The app existing is not the collision. An app on a digest-named credential has
// nothing at the object the service would take, and no writer can put anything
// there, so the name is free.
func TestServiceNameIsFreeWhenTheAppIsOnADigestCredential(t *testing.T) {
	digest := secretname.GitCredential("web", secretname.GitCredentialDigest("a-token", "git.example.com"))
	c := testCRClient(&kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Git: &kipperv1.AppGitSource{CredentialsSecret: digest}},
	})

	if err := refuseServiceNameSharingAnAppCredential(context.Background(), c, "shop-prod", "web-git"); err != nil {
		t.Errorf("a name nothing collides with was refused: %v", err)
	}
}

func TestServiceNameIsFreeWhenTheAppHasNoGitSource(t *testing.T) {
	c := testCRClient(&kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"}})

	if err := refuseServiceNameSharingAnAppCredential(context.Background(), c, "shop-prod", "web-git"); err != nil {
		t.Errorf("an app with no token was treated as owning the object: %v", err)
	}
}

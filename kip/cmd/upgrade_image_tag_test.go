package cmd

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestUpgradeHasAnImageTagFlag(t *testing.T) {
	f := upgradeCmd.Flags().Lookup("image-tag")
	if f == nil {
		t.Fatal("kip upgrade needs an --image-tag flag")
	}
	if f.DefValue != "" {
		t.Errorf("--image-tag must default to empty so a plain upgrade installs the released images, got %q", f.DefValue)
	}
}

func TestTestImagesNotice(t *testing.T) {
	notice := testImagesNotice("0123456789abcdef0123456789abcdef01234567")
	for _, want := range []string{"0123456789abcdef0123456789abcdef01234567", "kip upgrade", "without --image-tag"} {
		if !strings.Contains(notice, want) {
			t.Errorf("the notice should mention %q, got %q", want, notice)
		}
	}
}

func TestPinComponentImage(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	dep := func(image string) *appsv1.Deployment {
		return &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Image: image, ImagePullPolicy: corev1.PullIfNotPresent}},
		}}}}
	}

	d := dep("ghcr.io/getkipper/kipper-console-api:latest")
	moved := pinComponentImage(d, "console-api", sha)
	if got := d.Spec.Template.Spec.Containers[0].Image; got != "ghcr.io/getkipper/kipper-console-api:"+sha {
		t.Errorf("a tagged upgrade installs the commit's image, got %q", got)
	}
	if moved == "" || d.Spec.Template.Spec.Containers[0].ImagePullPolicy != corev1.PullAlways {
		t.Errorf("the move is reported and the image always pulled, got %q and %q", moved, d.Spec.Template.Spec.Containers[0].ImagePullPolicy)
	}

	d = dep("ghcr.io/getkipper/kipper-console-api:" + sha)
	pinComponentImage(d, "console-api", "")
	if got := d.Spec.Template.Spec.Containers[0].Image; got != "ghcr.io/getkipper/kipper-console-api:latest" {
		t.Errorf("a plain upgrade returns to the released image, got %q", got)
	}

	d = dep("traefik:v3")
	if pinComponentImage(d, "traefik", sha) != "" || d.Spec.Template.Spec.Containers[0].Image != "traefik:v3" {
		t.Error("a component without a pinned image is left alone")
	}
}

// The flag checks are a pure function so no test ever runs the upgrade
// itself, which would load the current cluster from the operator's config.
func TestCheckImageTagFlags(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	if err := checkImageTagFlags("", false); err != nil {
		t.Errorf("a plain upgrade needs no checks, got %v", err)
	}
	if err := checkImageTagFlags(sha, true); err != nil {
		t.Errorf("a commit sha with --skip-system is accepted, got %v", err)
	}
	if err := checkImageTagFlags("latest", true); err == nil || !strings.Contains(err.Error(), "commit sha") {
		t.Errorf("a tag that is not a commit sha is refused, got %v", err)
	}
	// A full upgrade re-applies the authz manifest at :latest after the
	// console rollout, which would mix released and test images.
	if err := checkImageTagFlags(sha, false); err == nil || !strings.Contains(err.Error(), "--skip-system") {
		t.Errorf("--image-tag without --skip-system is refused, got %v", err)
	}
}

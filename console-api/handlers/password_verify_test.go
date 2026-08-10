package handlers

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func seedDexConfig(t *testing.T, client *fake.Clientset) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("correct-horse-battery-staple"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	yaml := "staticPasswords:\n- email: admin@kipper.local\n  hash: " + string(hash) + "\n  username: user\n  userID: 1\n"
	_, err = client.CoreV1().ConfigMaps("dex").Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "dex-config", Namespace: "dex"},
		Data:       map[string]string{"config.yaml": yaml},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVerifyUserPassword_Valid(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedDexConfig(t, client)

	if err := VerifyUserPassword(context.Background(), client, "admin@kipper.local", "correct-horse-battery-staple"); err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

func TestVerifyUserPassword_WrongPassword(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedDexConfig(t, client)

	err := VerifyUserPassword(context.Background(), client, "admin@kipper.local", "wrong-password")
	if !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("expected ErrInvalidPassword, got %v", err)
	}
}

func TestVerifyUserPassword_UnknownEmail(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedDexConfig(t, client)

	err := VerifyUserPassword(context.Background(), client, "other@kipper.local", "anything")
	if !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("expected ErrInvalidPassword, got %v", err)
	}
}

func TestVerifyUserPassword_EmptyInputs(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedDexConfig(t, client)

	if err := VerifyUserPassword(context.Background(), client, "", "x"); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("expected ErrInvalidPassword for empty email, got %v", err)
	}
	if err := VerifyUserPassword(context.Background(), client, "admin@kipper.local", ""); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("expected ErrInvalidPassword for empty password, got %v", err)
	}
}

func TestVerifyUserPassword_MissingConfigMap(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern

	err := VerifyUserPassword(context.Background(), client, "admin@kipper.local", "x")
	if err == nil {
		t.Fatal("expected error when dex-config is missing")
	}
	if errors.Is(err, ErrInvalidPassword) {
		t.Errorf("missing configmap should surface as wrapped error, not ErrInvalidPassword: %v", err)
	}
}

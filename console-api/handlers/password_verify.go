package handlers

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// ErrInvalidPassword is returned when the supplied password does not match
// the stored hash for the given email.
var ErrInvalidPassword = errors.New("invalid password")

// VerifyUserPassword checks a plaintext password against the bcrypt hash
// stored for the given email in the Dex staticPasswords ConfigMap.
// Returns ErrInvalidPassword for a wrong password and a wrapped error for
// anything else (config read failure, user not found, etc.).
func VerifyUserPassword(ctx context.Context, client kubernetes.Interface, email, password string) error {
	if email == "" || password == "" {
		return ErrInvalidPassword
	}

	cm, err := client.CoreV1().ConfigMaps("dex").Get(ctx, "dex-config", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading Dex config: %w", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &config); err != nil {
		return fmt.Errorf("parsing Dex config: %w", err)
	}

	passwords, _ := config["staticPasswords"].([]interface{})
	for _, p := range passwords {
		entry, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if entry["email"] != email {
			continue
		}
		hash, _ := entry["hash"].(string)
		if hash == "" {
			return ErrInvalidPassword
		}
		if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
			return ErrInvalidPassword
		}
		return nil
	}

	return ErrInvalidPassword
}

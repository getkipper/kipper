package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestLoadDeployHistory(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expectLen   int
	}{
		{
			name:        "nil annotations",
			annotations: nil,
			expectLen:   0,
		},
		{
			name:        "no history annotation",
			annotations: map[string]string{"other": "value"},
			expectLen:   0,
		},
		{
			name:        "empty history annotation",
			annotations: map[string]string{historyAnnotation: ""},
			expectLen:   0,
		},
		{
			name:        "invalid JSON",
			annotations: map[string]string{historyAnnotation: "not json"},
			expectLen:   0,
		},
		{
			name: "valid history",
			annotations: map[string]string{
				historyAnnotation: `[{"revision":2,"image":"app:v2","trigger":"webhook","timestamp":"2025-01-01T00:00:00Z"},{"revision":1,"image":"app:v1","trigger":"webhook","timestamp":"2024-12-01T00:00:00Z"}]`,
			},
			expectLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := loadDeployHistory(tt.annotations)
			if len(result) != tt.expectLen {
				t.Errorf("expected %d entries, got %d", tt.expectLen, len(result))
			}
		})
	}
}

func TestLoadDeployHistoryOrder(t *testing.T) {
	annotations := map[string]string{
		historyAnnotation: `[{"revision":3,"image":"app:v3"},{"revision":2,"image":"app:v2"},{"revision":1,"image":"app:v1"}]`,
	}

	history := loadDeployHistory(annotations)
	if len(history) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(history))
	}
	if history[0].Revision != 3 {
		t.Errorf("expected first entry revision 3, got %d", history[0].Revision)
	}
	if history[2].Revision != 1 {
		t.Errorf("expected last entry revision 1, got %d", history[2].Revision)
	}
}

func TestVerifyHMAC(t *testing.T) {
	secret := "webhook-secret-token"
	payload := []byte(`{"image":"app:v2","commit":"abc123"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	validSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	tests := []struct {
		name      string
		payload   []byte
		signature string
		secret    string
		expected  bool
	}{
		{
			name:      "valid signature",
			payload:   payload,
			signature: validSig,
			secret:    secret,
			expected:  true,
		},
		{
			name:      "invalid signature",
			payload:   payload,
			signature: "sha256=0000000000000000000000000000000000000000000000000000000000000000",
			secret:    secret,
			expected:  false,
		},
		{
			name:      "wrong secret",
			payload:   payload,
			signature: validSig,
			secret:    "wrong-secret",
			expected:  false,
		},
		{
			name:      "empty signature",
			payload:   payload,
			signature: "",
			secret:    secret,
			expected:  false,
		},
		{
			name:      "tampered payload",
			payload:   []byte(`{"image":"app:v3"}`),
			signature: validSig,
			secret:    secret,
			expected:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := verifyHMAC(tt.payload, tt.signature, tt.secret)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestRecordPromotionHistory(t *testing.T) {
	t.Run("first entry gets revision 1", func(t *testing.T) {
		annotations := make(map[string]string)
		recordPromotionHistory(annotations, "app:v1", "staging")

		history := loadDeployHistory(annotations)
		if len(history) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(history))
		}
		if history[0].Revision != 1 {
			t.Errorf("expected revision 1, got %d", history[0].Revision)
		}
		if history[0].Image != "app:v1" {
			t.Errorf("expected image 'app:v1', got %q", history[0].Image)
		}
		if history[0].Trigger != "promote:staging" {
			t.Errorf("expected trigger 'promote:staging', got %q", history[0].Trigger)
		}
	})

	t.Run("increments revision from existing history", func(t *testing.T) {
		existing := []deployEntry{
			{Revision: 5, Image: "app:v5", Trigger: "webhook"},
		}
		data, _ := json.Marshal(existing)
		annotations := map[string]string{historyAnnotation: string(data)}

		recordPromotionHistory(annotations, "app:v6", "staging")

		history := loadDeployHistory(annotations)
		if len(history) != 2 {
			t.Fatalf("expected 2 entries, got %d", len(history))
		}
		if history[0].Revision != 6 {
			t.Errorf("expected revision 6, got %d", history[0].Revision)
		}
	})

	t.Run("caps history at 10 entries", func(t *testing.T) {
		existing := make([]deployEntry, 10)
		for i := range existing {
			existing[i] = deployEntry{Revision: 10 - i, Image: "app:v" + string(rune('0'+10-i))}
		}
		data, _ := json.Marshal(existing)
		annotations := map[string]string{historyAnnotation: string(data)}

		recordPromotionHistory(annotations, "app:v11", "staging")

		history := loadDeployHistory(annotations)
		if len(history) != 10 {
			t.Errorf("expected 10 entries (capped), got %d", len(history))
		}
		if history[0].Revision != 11 {
			t.Errorf("expected latest revision 11, got %d", history[0].Revision)
		}
	})
}

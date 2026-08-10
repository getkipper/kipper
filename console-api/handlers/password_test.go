package handlers

import "testing"

func TestValidatePassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  string
	}{
		{"valid strong password", "MyP@ssw0rd", ""},
		{"valid with symbols", "Test-123!", ""},
		{"too short", "Ab1!", "password must be at least 8 characters"},
		{"only lowercase", "abcdefgh", "password must contain an uppercase letter"},
		{"only uppercase", "ABCDEFGH", "password must contain a lowercase letter"},
		{"no digits", "Abcdefgh!", "password must contain a number"},
		{"no symbols", "Abcdefg1", "password must contain a symbol"},
		{"all same char", "aaaaaaaaaa", "password must contain an uppercase letter"},
		{"numbers only", "12345678", "password must contain a lowercase letter"},
		{"empty", "", "password must be at least 8 characters"},
		{"seven chars", "Abc1!xx", "password must be at least 8 characters"},
		{"exactly eight valid", "Abcdef1!", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePassword(tt.password)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("validatePassword(%q) unexpected error: %v", tt.password, err)
				}
			} else {
				if err == nil {
					t.Errorf("validatePassword(%q) expected error %q, got nil", tt.password, tt.wantErr)
				} else if err.Error() != tt.wantErr {
					t.Errorf("validatePassword(%q) = %q, want %q", tt.password, err.Error(), tt.wantErr)
				}
			}
		})
	}
}

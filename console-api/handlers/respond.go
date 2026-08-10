package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"unicode"
)

func respondJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if data != nil {
		_ = json.NewEncoder(w).Encode(data)
	}
}

func respondError(w http.ResponseWriter, status int, msg string) {
	respondJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	defer func() { _ = r.Body.Close() }()
	return json.NewDecoder(r.Body).Decode(v)
}

func validatePassword(pw string) error {
	if len(pw) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	var hasLower, hasUpper, hasDigit, hasSymbol bool
	for _, r := range pw {
		switch {
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsDigit(r):
			hasDigit = true
		case !unicode.IsLetter(r) && !unicode.IsDigit(r):
			hasSymbol = true
		}
	}
	if !hasLower {
		return fmt.Errorf("password must contain a lowercase letter")
	}
	if !hasUpper {
		return fmt.Errorf("password must contain an uppercase letter")
	}
	if !hasDigit {
		return fmt.Errorf("password must contain a number")
	}
	if !hasSymbol {
		return fmt.Errorf("password must contain a symbol")
	}
	return nil
}

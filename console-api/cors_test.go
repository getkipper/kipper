package main

import "testing"

// The API's CORS policy uses a wildcard origin, which is only safe without
// credentialed requests. This guards against a future edit that turns on
// credentials while leaving the wildcard in place.
func TestCorsOptionsAreSafe(t *testing.T) {
	if !corsOptionsAreSafe(corsOptions()) {
		t.Fatal("CORS policy pairs a wildcard origin with credentials — any site could make credentialed requests")
	}
}

func TestCorsOptionsAreSafe_RejectsWildcardWithCredentials(t *testing.T) {
	unsafe := corsOptions()
	unsafe.AllowCredentials = true
	if corsOptionsAreSafe(unsafe) {
		t.Fatal("wildcard origin + credentials must be flagged unsafe")
	}
}

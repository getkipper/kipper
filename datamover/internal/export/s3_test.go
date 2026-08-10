package export

import "testing"

func TestNewS3SourceEndpoint(t *testing.T) {
	if _, err := NewS3Source("://bad", "b", "ak", "sk"); err == nil {
		t.Error("invalid endpoint must be rejected")
	}
	if _, err := NewS3Source("no-scheme-host", "b", "ak", "sk"); err == nil {
		t.Error("endpoint without host must be rejected")
	}
	src, err := NewS3Source("https://s3.eu-west-1.example.com", "assets", "ak", "sk")
	if err != nil {
		t.Fatalf("valid endpoint rejected: %v", err)
	}
	if src.bucket != "assets" {
		t.Errorf("bucket = %q, want assets", src.bucket)
	}
}

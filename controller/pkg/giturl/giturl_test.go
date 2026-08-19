package giturl

import "testing"

func TestCanonicalAuthority(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"full https url", "https://github.com/org/repo.git", "github.com", false},
		{"bare host assumed https", "github.com", "github.com", false},
		{"host is lowercased", "https://GitHub.Com/org/repo.git", "github.com", false},
		{"trailing dot fqdn is normalized", "https://github.com./org/repo.git", "github.com", false},
		{"default port is dropped", "https://github.com:443/org/repo", "github.com", false},
		{"non-default port kept", "https://gitlab.example.com:8443/org/repo", "gitlab.example.com:8443", false},
		{"trailing path ignored", "https://gitlab.example.com/", "gitlab.example.com", false},
		{"ipv6 host bracketed", "https://[2001:db8::1]/org/repo", "[2001:db8::1]", false},
		{"ipv6 host with port", "https://[2001:db8::1]:8443/org/repo", "[2001:db8::1]:8443", false},

		{"empty", "", "", true},
		{"http rejected", "http://github.com/org/repo", "", true},
		{"ssh scheme rejected", "ssh://git@github.com/org/repo", "", true},
		{"userinfo rejected", "https://user@github.com/org/repo", "", true},
		{"userinfo with password rejected", "https://user:pass@github.com/org/repo", "", true},
		{"fragment rejected", "https://evil.example/#github.com", "", true},
		{"whitespace rejected", "https://git hub.com/org/repo", "", true},
		{"newline rejected", "https://github.com/org/repo\n", "", true},
		{"no host rejected", "https:///org/repo", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CanonicalAuthority(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("CanonicalAuthority(%q) = %q, want error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("CanonicalAuthority(%q) unexpected error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("CanonicalAuthority(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// The authority of a clone URL and of a credential's server must compare equal
// exactly when they are the same host, so host-binding neither over-blocks nor
// under-blocks.
func TestCanonicalAuthority_MatchesAcrossForms(t *testing.T) {
	url, err := CanonicalAuthority("https://github.com/org/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	server, err := CanonicalAuthority("github.com")
	if err != nil {
		t.Fatal(err)
	}
	if url != server {
		t.Errorf("clone-url authority %q != server authority %q", url, server)
	}
}

// Canonical returns a URL rebuilt over the canonical authority, so git derives
// exactly that authority for its credential request and a mixed-case host or an
// explicit :443 is not falsely denied at clone time.
func TestCanonical_RewritesURLToCanonicalAuthority(t *testing.T) {
	tests := []struct {
		in            string
		wantAuthority string
		wantURL       string
	}{
		{"https://GitHub.Com/org/repo.git", "github.com", "https://github.com/org/repo.git"},
		{"https://github.com:443/org/repo.git", "github.com", "https://github.com/org/repo.git"},
		{"https://gitlab.example.com:8443/org/repo.git", "gitlab.example.com:8443", "https://gitlab.example.com:8443/org/repo.git"},
		{"https://github.com/org/repo.git?ref=main", "github.com", "https://github.com/org/repo.git?ref=main"},
		{"https://[2001:DB8::1]:443/org/repo", "[2001:db8::1]", "https://[2001:db8::1]/org/repo"},
	}
	for _, tt := range tests {
		authority, gotURL, err := Canonical(tt.in)
		if err != nil {
			t.Fatalf("Canonical(%q): %v", tt.in, err)
		}
		if authority != tt.wantAuthority {
			t.Errorf("Canonical(%q) authority = %q, want %q", tt.in, authority, tt.wantAuthority)
		}
		if gotURL != tt.wantURL {
			t.Errorf("Canonical(%q) url = %q, want %q", tt.in, gotURL, tt.wantURL)
		}
	}
}

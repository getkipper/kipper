package mail

import "testing"

func TestStripHTML(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple tags",
			input: "<h1>Hello</h1><p>World</p>",
			want:  "HelloWorld",
		},
		{
			name:  "nested tags",
			input: "<div><span>text</span></div>",
			want:  "text",
		},
		{
			name:  "no tags",
			input: "plain text",
			want:  "plain text",
		},
		{
			name:  "empty",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripHTML(tt.input)
			if got != tt.want {
				t.Errorf("StripHTML() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractEmail(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Kipper <noreply@example.com>", "noreply@example.com"},
		{"noreply@example.com", "noreply@example.com"},
		{"<admin@test.com>", "admin@test.com"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := extractEmail(tt.input)
			if got != tt.want {
				t.Errorf("extractEmail(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

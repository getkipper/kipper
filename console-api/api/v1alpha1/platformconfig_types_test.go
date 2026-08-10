package v1alpha1

import (
	"encoding/json"
	"testing"
)

func TestComponentOverrideEnabledPointerSemantics(t *testing.T) {
	yes, no := true, false

	tests := []struct {
		name        string
		input       string
		wantNil     bool
		wantEnabled bool
	}{
		{
			name:        "absent enabled field is nil (use profile default)",
			input:       `{"name":"prometheus"}`,
			wantNil:     true,
			wantEnabled: false,
		},
		{
			name:        "explicit true",
			input:       `{"name":"prometheus","enabled":true}`,
			wantNil:     false,
			wantEnabled: true,
		},
		{
			name:        "explicit false survives marshalling",
			input:       `{"name":"prometheus","enabled":false}`,
			wantNil:     false,
			wantEnabled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got ComponentOverride
			if err := json.Unmarshal([]byte(tt.input), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if tt.wantNil {
				if got.Enabled != nil {
					t.Fatalf("expected Enabled to be nil, got %v", *got.Enabled)
				}
				return
			}
			if got.Enabled == nil {
				t.Fatalf("expected Enabled to be set, got nil")
			}
			if *got.Enabled != tt.wantEnabled {
				t.Fatalf("expected Enabled=%v, got %v", tt.wantEnabled, *got.Enabled)
			}
		})
	}

	// Round-trip: an explicit false must not become "absent" after marshalling.
	in := ComponentOverride{Name: "loki", Enabled: &no}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) == `{"name":"loki"}` {
		t.Fatalf("explicit false was dropped during marshalling, got %s", string(b))
	}

	// And explicit true round-trips too.
	in = ComponentOverride{Name: "prometheus", Enabled: &yes}
	if _, err := json.Marshal(in); err != nil {
		t.Fatalf("marshal explicit true: %v", err)
	}
}

func TestPlatformConfigSpecProfileRoundTrip(t *testing.T) {
	for _, profile := range []string{"nano", "small", "medium", "large", "xlarge"} {
		t.Run(profile, func(t *testing.T) {
			spec := PlatformConfigSpec{Profile: profile}
			b, err := json.Marshal(spec)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got PlatformConfigSpec
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Profile != profile {
				t.Fatalf("profile mismatch: want %q, got %q", profile, got.Profile)
			}
		})
	}
}

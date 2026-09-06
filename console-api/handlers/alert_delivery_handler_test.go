package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The console asks this so it can tell an operator that the bell is filling in
// silence. On the cluster this comes from, nobody knew until a database had
// been crash-looping for three and a half days.

func TestAlertDeliveryHandler(t *testing.T) {
	tests := []struct {
		name        string
		objects     []runtimeObject
		wantRoute   string
		wantNowhere bool
	}{
		{
			name:        "nothing configured, which the console must say out loud",
			objects:     nil,
			wantRoute:   "nowhere",
			wantNowhere: true,
		},
		{
			name:        "a webhook carries them",
			objects:     []runtimeObject{slackSecret("https://hooks.example.com/abc")},
			wantRoute:   "slack",
			wantNowhere: false,
		},
		{
			name:        "smtp carries them when no webhook is set",
			objects:     []runtimeObject{smtpSecret(t, "smtp.example.com")},
			wantRoute:   "email",
			wantNowhere: false,
		},
	}

	withAdmin(t)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := &AlertDelivery{Client: newFakeClient(tc.objects...)}
			rec := httptest.NewRecorder()
			h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings/alert-delivery", nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}

			var got alertDeliveryResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decoding response: %v", err)
			}
			if got.Route != tc.wantRoute {
				t.Errorf("route = %q, want %q", got.Route, tc.wantRoute)
			}
			if got.GoingNowhere != tc.wantNowhere {
				t.Errorf("going_nowhere = %v, want %v", got.GoingNowhere, tc.wantNowhere)
			}
		})
	}
}

// The response must not leak the webhook or the SMTP password: an operator
// without permission to read those secrets can still be told whether a channel
// exists.
func TestAlertDeliveryHandler_RevealsNoCredential(t *testing.T) {
	client := newFakeClient(slackSecret("https://hooks.example.com/T00/B00/XXXXsecretXXXX"))
	h := &AlertDelivery{Client: client}
	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/v1/settings/alert-delivery", nil))

	if body := rec.Body.String(); strings.Contains(body, "XXXXsecretXXXX") || strings.Contains(body, "hooks.example.com") {
		t.Errorf("response carries the webhook: %s", body)
	}
}

// When alerts go nowhere the console has to give the right advice, and "add a
// Slack webhook or configure SMTP" is wrong advice for a cluster that already
// has SMTP and simply has nobody to send to.
func TestAlertDeliveryHandlerSaysWhyAlertsGoNowhere(t *testing.T) {
	restore := adminRecipients
	defer func() { SetAdminRecipients(restore) }()

	tests := []struct {
		name       string
		objects    []runtimeObject
		admins     []string
		wantReason string
	}{
		{
			name:       "no channel at all",
			objects:    nil,
			admins:     []string{"ops@example.com"},
			wantReason: "no_channel",
		},
		{
			name:       "smtp is configured but no admin has an address",
			objects:    []runtimeObject{smtpSecret(t, "smtp.example.com")},
			admins:     nil,
			wantReason: "no_recipients",
		},
		{
			name:       "delivery works, so there is nothing to explain",
			objects:    []runtimeObject{slackSecret("https://hooks.example.com/abc")},
			admins:     nil,
			wantReason: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			SetAdminRecipients(func() []string { return tc.admins })
			h := &AlertDelivery{Client: newFakeClient(tc.objects...)}

			rec := httptest.NewRecorder()
			h.Get(rec, httptest.NewRequest(http.MethodGet, "/settings/alert-delivery", nil))

			var got alertDeliveryResponse
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

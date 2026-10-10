package resourcebounds

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

func q(s string) resource.Quantity { return resource.MustParse(s) }

func user(s string) Quantity      { return Quantity{Value: q(s), Source: User} }
func automatic(s string) Quantity { return Quantity{Value: q(s), Source: Automatic} }
func held(s string) Quantity      { return Quantity{Value: q(s), Source: Held} }

var unset = Quantity{}

func pair(req, lim string) *Pair { return &Pair{Request: q(req), Limit: q(lim)} }

var memoryRange = AutoRange{Floor: q("128Mi"), Ceiling: q("8Gi")}

func TestResolve(t *testing.T) {
	cases := []struct {
		name        string
		request     Quantity
		limit       Quantity
		recommended *Pair
		fallback    Pair
		wantMode    Mode
		want        Pair
	}{
		{
			name:    "bounds: a quiet recommendation stops at the floor",
			request: user("512Mi"), limit: user("2Gi"),
			recommended: pair("256Mi", "256Mi"), fallback: *pair("512Mi", "2Gi"),
			wantMode: ModeBounded, want: *pair("512Mi", "2Gi"),
		},
		{
			name:    "bounds: a busy recommendation moves the request only",
			request: user("512Mi"), limit: user("2Gi"),
			recommended: pair("768Mi", "768Mi"), fallback: *pair("512Mi", "2Gi"),
			wantMode: ModeBounded, want: *pair("768Mi", "2Gi"),
		},
		{
			name:    "bounds: the request never passes the ceiling",
			request: user("512Mi"), limit: user("2Gi"),
			recommended: pair("4Gi", "4Gi"), fallback: *pair("512Mi", "2Gi"),
			wantMode: ModeBounded, want: *pair("2Gi", "2Gi"),
		},
		{
			name:    "bounds: without a recommendation the live request is kept inside them",
			request: user("512Mi"), limit: user("2Gi"),
			fallback: *pair("1Gi", "1Gi"),
			wantMode: ModeBounded, want: *pair("1Gi", "2Gi"),
		},
		{
			name:    "bounds may sit outside Kipper's own range",
			request: user("64Mi"), limit: user("16Gi"),
			recommended: pair("32Mi", "32Mi"), fallback: *pair("64Mi", "16Gi"),
			wantMode: ModeBounded, want: *pair("64Mi", "16Gi"),
		},
		{
			name:    "equal request and limit is a fixed size",
			request: user("1Gi"), limit: user("1Gi"),
			recommended: pair("256Mi", "256Mi"), fallback: *pair("1Gi", "1Gi"),
			wantMode: ModeFixed, want: *pair("1Gi", "1Gi"),
		},
		{
			name:    "a request above its limit is fixed at the limit",
			request: user("2Gi"), limit: user("1Gi"),
			recommended: pair("256Mi", "256Mi"), fallback: *pair("1Gi", "1Gi"),
			wantMode: ModeFixed, want: *pair("1Gi", "1Gi"),
		},
		{
			name:    "only a request is a fixed size",
			request: user("512Mi"), limit: unset,
			recommended: pair("256Mi", "256Mi"), fallback: *pair("512Mi", "512Mi"),
			wantMode: ModeFixed, want: *pair("512Mi", "512Mi"),
		},
		{
			name:    "only a limit is a fixed size",
			request: unset, limit: user("2Gi"),
			recommended: pair("256Mi", "256Mi"), fallback: *pair("2Gi", "2Gi"),
			wantMode: ModeFixed, want: *pair("2Gi", "2Gi"),
		},
		{
			name:    "a user request beside an automatic limit is a fixed size",
			request: user("512Mi"), limit: automatic("256Mi"),
			recommended: pair("256Mi", "256Mi"), fallback: *pair("256Mi", "256Mi"),
			wantMode: ModeFixed, want: *pair("512Mi", "512Mi"),
		},
		{
			name:    "automatic follows the recommendation",
			request: automatic("256Mi"), limit: automatic("256Mi"),
			recommended: pair("512Mi", "512Mi"), fallback: *pair("256Mi", "256Mi"),
			wantMode: ModeAutomatic, want: *pair("512Mi", "512Mi"),
		},
		{
			name:    "automatic stops at Kipper's floor",
			request: unset, limit: unset,
			recommended: pair("64Mi", "64Mi"), fallback: *pair("128Mi", "128Mi"),
			wantMode: ModeAutomatic, want: *pair("128Mi", "128Mi"),
		},
		{
			name:    "automatic stops at Kipper's ceiling",
			request: unset, limit: unset,
			recommended: pair("16Gi", "16Gi"), fallback: *pair("128Mi", "128Mi"),
			wantMode: ModeAutomatic, want: *pair("8Gi", "8Gi"),
		},
		{
			name:    "automatic without a recommendation keeps the fallback",
			request: automatic("256Mi"), limit: automatic("256Mi"),
			fallback: *pair("256Mi", "256Mi"),
			wantMode: ModeAutomatic, want: *pair("256Mi", "256Mi"),
		},
		{
			name:    "automatic never sets a limit below its request",
			request: unset, limit: unset,
			recommended: pair("512Mi", "256Mi"), fallback: *pair("128Mi", "128Mi"),
			wantMode: ModeAutomatic, want: *pair("512Mi", "512Mi"),
		},
		{
			name:    "held values stay exactly as they are",
			request: held("256Mi"), limit: held("1Gi"),
			recommended: pair("128Mi", "128Mi"), fallback: *pair("200Mi", "200Mi"),
			wantMode: ModeHeld, want: *pair("256Mi", "1Gi"),
		},
		{
			name:    "one held value holds the pair",
			request: held("256Mi"), limit: automatic("512Mi"),
			recommended: pair("128Mi", "128Mi"), fallback: *pair("256Mi", "512Mi"),
			wantMode: ModeHeld, want: *pair("256Mi", "512Mi"),
		},
		{
			name:    "a held value alone is mirrored like any one-sided value",
			request: unset, limit: held("1Gi"),
			recommended: pair("128Mi", "128Mi"), fallback: *pair("1Gi", "1Gi"),
			wantMode: ModeHeld, want: *pair("1Gi", "1Gi"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, mode := Resolve(tc.request, tc.limit, tc.recommended, tc.fallback, memoryRange)
			if mode != tc.wantMode {
				t.Fatalf("mode = %v, want %v", mode, tc.wantMode)
			}
			if got.Request.Cmp(tc.want.Request) != 0 || got.Limit.Cmp(tc.want.Limit) != 0 {
				t.Fatalf("got %s / %s, want %s / %s",
					got.Request.String(), got.Limit.String(), tc.want.Request.String(), tc.want.Limit.String())
			}
		})
	}
}

func TestResolveWithoutACeiling(t *testing.T) {
	cpuRange := AutoRange{Floor: q("100m")}
	got, mode := Resolve(unset, unset, pair("4", "4"), *pair("100m", "100m"), cpuRange)
	if mode != ModeAutomatic || got.Request.Cmp(q("4")) != 0 || got.Limit.Cmp(q("4")) != 0 {
		t.Fatalf("got %s / %s (%v), want 4 / 4 automatic", got.Request.String(), got.Limit.String(), mode)
	}
}

func TestAutomaticLimitKeepsItsOwnFloor(t *testing.T) {
	cpuRange := AutoRange{Floor: q("100m"), LimitFloor: q("500m")}
	rec := Pair{Request: q("150m"), Limit: q("150m")}
	got, mode := Resolve(Quantity{}, Quantity{}, &rec, Pair{Request: q("100m"), Limit: q("1")}, cpuRange)
	if mode != ModeAutomatic {
		t.Fatalf("mode = %v, want automatic", mode)
	}
	if got.Request.Cmp(q("150m")) != 0 || got.Limit.Cmp(q("500m")) != 0 {
		t.Fatalf("Resolve = %s/%s, want 150m/500m", got.Request.String(), got.Limit.String())
	}
}

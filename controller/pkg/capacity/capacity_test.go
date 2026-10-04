package capacity

import (
	"errors"
	"strings"
	"testing"
)

func i32(v int32) *int32 { return &v }

func TestValidate(t *testing.T) {
	tests := []struct {
		name     string
		policy   *Policy
		replicas *int32
		wantErr  string
	}{
		{name: "no block", policy: nil, replicas: i32(7)},
		{name: "enabled, cpu only, min omitted", policy: &Policy{Enabled: true, MaxReplicas: i32(5), CPUTarget: i32(70)}},
		{name: "enabled, memory target 0 is unused", policy: &Policy{Enabled: true, MinReplicas: i32(1), MaxReplicas: i32(5), CPUTarget: i32(70), MemoryTarget: i32(0)}},
		{name: "enabled, utilisation above 100", policy: &Policy{Enabled: true, MaxReplicas: i32(5), CPUTarget: i32(250)}},
		{name: "enabled, explicit min 0", policy: &Policy{Enabled: true, MinReplicas: i32(0), MaxReplicas: i32(5), CPUTarget: i32(70)}, wantErr: "minReplicas must be at least 1"},
		{name: "negative min", policy: &Policy{Enabled: false, MinReplicas: i32(-1), MaxReplicas: i32(5)}, wantErr: "minReplicas must be at least 1"},
		{name: "enabled, no max", policy: &Policy{Enabled: true, CPUTarget: i32(70)}, wantErr: "maxReplicas is required"},
		{name: "explicit max 0", policy: &Policy{Enabled: false, MaxReplicas: i32(0)}, wantErr: "maxReplicas must be at least 1"},
		{name: "min above max", policy: &Policy{Enabled: true, MinReplicas: i32(6), MaxReplicas: i32(5), CPUTarget: i32(70)}, wantErr: "minReplicas (6) must not exceed maxReplicas (5)"},
		{name: "disabled, min above max", policy: &Policy{MinReplicas: i32(6), MaxReplicas: i32(5)}, wantErr: "minReplicas (6) must not exceed maxReplicas (5)"},
		{name: "enabled, no target", policy: &Policy{Enabled: true, MaxReplicas: i32(5)}, wantErr: "set cpuTarget or memoryTarget"},
		{name: "enabled, both targets 0", policy: &Policy{Enabled: true, MaxReplicas: i32(5), CPUTarget: i32(0), MemoryTarget: i32(0)}, wantErr: "set cpuTarget or memoryTarget"},
		{name: "negative target", policy: &Policy{Enabled: true, MaxReplicas: i32(5), CPUTarget: i32(-5)}, wantErr: "cpuTarget must not be negative"},
		{name: "disabled opt-out without max", policy: &Policy{}, replicas: i32(9)},
		{name: "disabled, min without max", policy: &Policy{MinReplicas: i32(3)}, replicas: i32(3), wantErr: "set maxReplicas with minReplicas, or leave both out to remove the bounds"},
		{name: "disabled, the CRD's default min of 1 without max", policy: &Policy{MinReplicas: i32(1)}, replicas: i32(9)},
		{name: "disabled, unused negative target", policy: &Policy{MaxReplicas: i32(5), CPUTarget: i32(-1)}, replicas: i32(3)},
		{name: "replicas within bounds", policy: &Policy{MinReplicas: i32(2), MaxReplicas: i32(5)}, replicas: i32(3)},
		{name: "replicas below min", policy: &Policy{MinReplicas: i32(2), MaxReplicas: i32(5)}, replicas: i32(1), wantErr: "replicas (1) must be between 2 and 5"},
		{name: "replicas above max, policy on", policy: &Policy{Enabled: true, MaxReplicas: i32(5), CPUTarget: i32(70)}, replicas: i32(8), wantErr: "replicas (8) must be between 1 and 5"},
		{name: "replicas 0 with bounds", policy: &Policy{MaxReplicas: i32(5)}, replicas: i32(0), wantErr: "replicas (0) must be between 1 and 5"},
		{name: "replicas omitted is resolved by the caller", policy: &Policy{MinReplicas: i32(2), MaxReplicas: i32(5)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.policy, tt.replicas)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestBounds(t *testing.T) {
	if _, _, ok := (&Policy{}).Bounds(); ok {
		t.Error("a block without maxReplicas has no bounds")
	}
	lo, hi, ok := (&Policy{MaxReplicas: i32(5)}).Bounds()
	if !ok || lo != 1 || hi != 5 {
		t.Errorf("Bounds() = %d, %d, %v; want 1, 5, true", lo, hi, ok)
	}
	var nilPolicy *Policy
	if _, _, ok := nilPolicy.Bounds(); ok {
		t.Error("a missing block has no bounds")
	}
}

func TestUsable(t *testing.T) {
	if !(&Policy{Enabled: true, MaxReplicas: i32(3), CPUTarget: i32(70)}).Usable() {
		t.Error("a valid enabled policy is usable")
	}
	if (&Policy{Enabled: true, MaxReplicas: i32(3)}).Usable() {
		t.Error("an enabled policy without a target is not usable")
	}
	if (&Policy{MaxReplicas: i32(3), CPUTarget: i32(70)}).Usable() {
		t.Error("a disabled policy is not usable")
	}
}

func TestIntoBounds(t *testing.T) {
	p := &Policy{MinReplicas: i32(2), MaxReplicas: i32(5)}
	for _, tc := range []struct {
		in, want int32
		moved    bool
	}{{1, 2, true}, {3, 3, false}, {9, 5, true}, {0, 2, true}} {
		got, moved := p.IntoBounds(tc.in)
		if got != tc.want || moved != tc.moved {
			t.Errorf("IntoBounds(%d) = %d, %v; want %d, %v", tc.in, got, moved, tc.want, tc.moved)
		}
	}
	if got, moved := (&Policy{}).IntoBounds(9); got != 9 || moved {
		t.Errorf("a block without bounds keeps the count, got %d, %v", got, moved)
	}
}

func TestPlanSwitchOff(t *testing.T) {
	on := &Policy{Enabled: true, MinReplicas: i32(2), MaxReplicas: i32(6), CPUTarget: i32(70)}
	live := func(n int32) func() (int32, error) {
		return func() (int32, error) { return n, nil }
	}
	unreadable := func() (int32, error) { return 0, errors.New("forbidden") }
	mustNotRead := func() (int32, error) {
		t.Fatal("the running count must not be read")
		return 0, nil
	}

	tests := []struct {
		name     string
		policy   *Policy
		stored   int32
		stopped  bool
		readLive func() (int32, error)
		want     SwitchOff
		writes   bool
		clamped  bool
	}{
		{name: "no block", policy: nil, stored: 3, readLive: mustNotRead, want: SwitchOff{AlreadyOff: true, Replicas: 3}},
		{name: "already off", policy: &Policy{MaxReplicas: i32(6)}, stored: 3, readLive: mustNotRead, want: SwitchOff{AlreadyOff: true, Replicas: 3}},
		{name: "stopped keeps the stored count", policy: on, stored: 3, stopped: true, readLive: mustNotRead, want: SwitchOff{Stopped: true, Replicas: 3}},
		{name: "unreadable count keeps the stored count", policy: on, stored: 3, readLive: unreadable, want: SwitchOff{CountUnknown: true, Replicas: 3}},
		{name: "running count kept", policy: on, stored: 3, readLive: live(4), want: SwitchOff{Live: 4, Replicas: 4}, writes: true},
		{name: "running count above max clamped", policy: on, stored: 3, readLive: live(9), want: SwitchOff{Live: 9, Replicas: 6}, writes: true, clamped: true},
		{name: "running count below min clamped", policy: on, stored: 3, readLive: live(1), want: SwitchOff{Live: 1, Replicas: 2}, writes: true, clamped: true},
		{name: "invalid bounds keep the running count", policy: &Policy{Enabled: true, MinReplicas: i32(7), MaxReplicas: i32(3), CPUTarget: i32(70)}, stored: 3, readLive: live(5), want: SwitchOff{Live: 5, Replicas: 5, InvalidBounds: true}, writes: true},
		{name: "unusable policy without target still hands over", policy: &Policy{Enabled: true, MaxReplicas: i32(6)}, stored: 3, readLive: live(4), want: SwitchOff{Live: 4, Replicas: 4}, writes: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PlanSwitchOff(tt.policy, tt.stored, tt.stopped, tt.readLive)
			if got != tt.want {
				t.Errorf("PlanSwitchOff() = %+v, want %+v", got, tt.want)
			}
			if got.WritesReplicas() != tt.writes {
				t.Errorf("WritesReplicas() = %v, want %v", got.WritesReplicas(), tt.writes)
			}
			if got.Clamped() != tt.clamped {
				t.Errorf("Clamped() = %v, want %v", got.Clamped(), tt.clamped)
			}
		})
	}
}

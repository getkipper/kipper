package handlers

import (
	"testing"

	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
)

func sp(s string) *string { return &s }

func TestValidateResourceQuantities(t *testing.T) {
	tests := []struct {
		name    string
		req     resourcesRequest
		wantErr bool
	}{
		{"all empty", resourcesRequest{}, false},
		{"valid values", resourcesRequest{CPURequest: sp("100m"), CPULimit: sp("1"), MemoryRequest: sp("128Mi"), MemoryLimit: sp("256Mi")}, false},
		{"partial valid", resourcesRequest{MemoryLimit: sp("512Mi")}, false},
		{"garbage cpu", resourcesRequest{CPURequest: sp("2 cores")}, true},
		{"garbage memory", resourcesRequest{MemoryLimit: sp("lots")}, true},
		{"bad unit", resourcesRequest{MemoryRequest: sp("64MB")}, true},
		{"negative cpu", resourcesRequest{CPURequest: sp("-100m")}, true},
		{"negative memory", resourcesRequest{MemoryLimit: sp("-1Mi")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateResourceQuantities(tt.req)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateResourceQuantities(%+v) error = %v, wantErr %v", tt.req, err, tt.wantErr)
			}
		})
	}
}

func TestPairEdit(t *testing.T) {
	tests := []struct {
		name     string
		req, lim *string
		want     *resourcebounds.PairEdit
	}{
		{"neither sent leaves the resource alone", nil, nil, nil},
		{"both empty clears it", sp(""), sp(""), &resourcebounds.PairEdit{Clear: true}},
		{"a lone limit is a fixed size", nil, sp("2Gi"), &resourcebounds.PairEdit{Request: "2Gi", Limit: "2Gi"}},
		{"a lone request is a fixed size", sp("512Mi"), nil, &resourcebounds.PairEdit{Request: "512Mi", Limit: "512Mi"}},
		{"both set are bounds", sp("512Mi"), sp("2Gi"), &resourcebounds.PairEdit{Request: "512Mi", Limit: "2Gi"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pairEdit(tt.req, tt.lim)
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Fatalf("pairEdit = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestValidateRequestWithinLimit(t *testing.T) {
	if err := validateRequestWithinLimit(resourcesRequest{MemoryRequest: sp("2Gi"), MemoryLimit: sp("1Gi")}); err == nil {
		t.Error("a memory request above its limit was accepted")
	}
	if err := validateRequestWithinLimit(resourcesRequest{CPURequest: sp("250m"), CPULimit: sp("1")}); err != nil {
		t.Errorf("a request below its limit was refused: %v", err)
	}
	if err := validateRequestWithinLimit(resourcesRequest{MemoryLimit: sp("1Gi")}); err != nil {
		t.Errorf("a lone limit was refused: %v", err)
	}
}

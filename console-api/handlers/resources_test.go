package handlers

import "testing"

func TestValidateResourceQuantities(t *testing.T) {
	tests := []struct {
		name    string
		req     resourcesRequest
		wantErr bool
	}{
		{"all empty", resourcesRequest{}, false},
		{"valid values", resourcesRequest{CPURequest: "100m", CPULimit: "1", MemoryRequest: "128Mi", MemoryLimit: "256Mi"}, false},
		{"partial valid", resourcesRequest{MemoryLimit: "512Mi"}, false},
		{"garbage cpu", resourcesRequest{CPURequest: "2 cores"}, true},
		{"garbage memory", resourcesRequest{MemoryLimit: "lots"}, true},
		{"bad unit", resourcesRequest{MemoryRequest: "64MB"}, true},
		{"negative cpu", resourcesRequest{CPURequest: "-100m"}, true},
		{"negative memory", resourcesRequest{MemoryLimit: "-1Mi"}, true},
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

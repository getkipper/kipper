package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/deployer"
)

func TestResourceFlagEdits(t *testing.T) {
	cases := []struct {
		name  string
		flags resourceFlags
		want  *deployer.ResourceEdits
	}{
		{"nothing asked", resourceFlags{}, nil},
		{"bounds", resourceFlags{memoryRequest: "512Mi", memoryLimit: "2Gi"},
			&deployer.ResourceEdits{Memory: &deployer.PairEdit{Request: "512Mi", Limit: "2Gi"}}},
		{"a fixed size", resourceFlags{memory: "1Gi"},
			&deployer.ResourceEdits{Memory: &deployer.PairEdit{Request: "1Gi", Limit: "1Gi"}}},
		{"a lone limit is a fixed size", resourceFlags{cpuLimit: "1"},
			&deployer.ResourceEdits{CPU: &deployer.PairEdit{Request: "1", Limit: "1"}}},
		{"back to automatic", resourceFlags{tuning: "auto"},
			&deployer.ResourceEdits{CPU: &deployer.PairEdit{Clear: true}, Memory: &deployer.PairEdit{Clear: true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.flags.edits()
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestResourceFlagEditsRefuseContradictions(t *testing.T) {
	for name, flags := range map[string]resourceFlags{
		"request above limit":       {memoryRequest: "4Gi", memoryLimit: "2Gi"},
		"fixed and bounds together": {memory: "1Gi", memoryLimit: "2Gi"},
		"auto with a value":         {tuning: "auto", cpu: "1"},
		"an unknown tuning":         {tuning: "manual"},
		"an unreadable quantity":    {memoryLimit: "lots"},
		"a negative quantity":       {cpuRequest: "-1"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := flags.edits()
			assert.Error(t, err)
		})
	}
}

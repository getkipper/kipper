package installer

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParsePinnedHosts(t *testing.T) {
	body := `127.0.0.1 localhost
::1 localhost
127.0.0.1 dex.old.example.com dex.new.example.com # kipper-dex-pin
10.0.0.1 other
`
	assert.Equal(t, []string{"dex.old.example.com", "dex.new.example.com"}, parsePinnedHosts(body))
	assert.Nil(t, parsePinnedHosts("127.0.0.1 localhost\n"), "no pin line yields nothing")
	assert.Nil(t, parsePinnedHosts("127.0.0.1 # kipper-dex-pin\n"), "a marker with no hosts yields nothing")
}

func TestUnionHosts(t *testing.T) {
	assert.Equal(t, []string{"a.com", "b.com"}, unionHosts([]string{"a.com"}, []string{"b.com"}))
	assert.Equal(t, []string{"a.com", "b.com"}, unionHosts([]string{"a.com", "b.com"}, []string{"a.com"}))
	assert.Equal(t, []string{"a.com"}, unionHosts([]string{"a.com"}, nil))
	assert.Empty(t, unionHosts(nil, nil))
}

func TestRenderAdminSubjectPatch(t *testing.T) {
	// Staging keeps both admins; every subject is oidc:-prefixed and sorted.
	p := renderAdminSubjectPatch([]string{"admin@new.example.com", "admin@old.example.com"})
	assert.Equal(t, `{"subjects":[{"apiGroup":"rbac.authorization.k8s.io","kind":"User","name":"oidc:admin@new.example.com"},{"apiGroup":"rbac.authorization.k8s.io","kind":"User","name":"oidc:admin@old.example.com"}]}`, p)

	// Contraction to one admin.
	p = renderAdminSubjectPatch([]string{"admin@new.example.com"})
	assert.Contains(t, p, "oidc:admin@new.example.com")
	assert.NotContains(t, p, "old")

	// Dedup.
	p = renderAdminSubjectPatch([]string{"a@x.com", "a@x.com"})
	assert.Equal(t, 1, countSubjects(p))
}

func countSubjects(patch string) int {
	n := 0
	for i := 0; i+1 < len(patch); i++ {
		if patch[i:i+2] == `{"` && i > 12 { // skip the leading {"subjects"
			n++
		}
	}
	return n
}

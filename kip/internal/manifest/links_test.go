package manifest

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func namespaceOwners(known map[string][2]string) func(string) (string, string, bool, error) {
	return func(ns string) (string, string, bool, error) {
		owner, ok := known[ns]
		return owner[0], owner[1], ok, nil
	}
}

func TestExportLinks(t *testing.T) {
	live := []interface{}{
		map[string]interface{}{"app": "api", "namespace": "shop-test"},
		map[string]interface{}{"app": "docuseal", "namespace": "docuseal-test"},
		map[string]interface{}{"app": "ledger", "namespace": "finance-prod"},
		map[string]interface{}{"app": "ghost", "namespace": "unlabelled"},
	}
	m := &Manifest{}
	got, err := exportLinks(live, "shop-test", "test", namespaceOwners(map[string][2]string{
		"docuseal-test": {"docuseal", "test"},
		"finance-prod":  {"finance", "prod"},
	}), "web", m)

	require.NoError(t, err)
	assert.Equal(t, []LinkSpec{
		{App: "api"},
		{App: "docuseal", Project: "docuseal"},
		{App: "ledger", Project: "finance", Environment: "prod"},
	}, got)
	require.Len(t, m.Notes, 1)
	assert.Contains(t, m.Notes[0], `app "web"`)
	assert.Contains(t, m.Notes[0], "ghost")
}

func TestExportLinksWithoutAnEnvironmentLabel(t *testing.T) {
	live := []interface{}{
		map[string]interface{}{"app": "api", "namespace": "finance"},
		map[string]interface{}{"app": "ledger", "namespace": "finance-staging"},
	}
	m := &Manifest{}
	got, err := exportLinks(live, "shop-test", "test", namespaceOwners(map[string][2]string{
		"finance":         {"finance", ""},
		"finance-staging": {"finance", ""},
	}), "web", m)

	require.NoError(t, err)
	assert.Equal(t, []LinkSpec{{App: "api", Project: "finance", Environment: "default"}}, got)
	require.Len(t, m.Notes, 1)
	assert.Contains(t, m.Notes[0], "finance-staging")
}

func TestExportLinksFailsWhenANamespaceCannotBeRead(t *testing.T) {
	live := []interface{}{map[string]interface{}{"app": "api", "namespace": "finance-prod"}}
	_, err := exportLinks(live, "shop-test", "test", func(string) (string, string, bool, error) {
		return "", "", false, fmt.Errorf("namespaces is forbidden")
	}, "web", &Manifest{})

	assert.ErrorContains(t, err, "finance-prod")
}

func TestConvertLinks(t *testing.T) {
	m := &Manifest{Project: "shop", Apps: map[string]AppSpec{"web": {
		Image: "web:1", Port: 8080,
		Links: []LinkSpec{{App: "api"}, {App: "docuseal", Project: "docuseal"}, {App: "ledger", Project: "finance", Environment: "prod"}},
	}}}
	resolve := func(project, environment string) string {
		if environment == "" {
			environment = "test"
		}
		return fmt.Sprintf("%s-%s", project, environment)
	}

	res := Convert(m, "shop-test", resolve)

	require.Len(t, res, 1)
	assert.Equal(t, []interface{}{
		map[string]interface{}{"app": "api", "namespace": "shop-test"},
		map[string]interface{}{"app": "docuseal", "namespace": "docuseal-test"},
		map[string]interface{}{"app": "ledger", "namespace": "finance-prod"},
	}, res[0].Object.Object["spec"].(map[string]interface{})["links"])
}

func TestValidateLinks(t *testing.T) {
	base := func(links ...LinkSpec) *Manifest {
		return &Manifest{Project: "shop", Apps: map[string]AppSpec{"web": {Image: "web:1", Port: 8080, Links: links}}}
	}
	assert.NoError(t, Validate(base(LinkSpec{App: "api"}, LinkSpec{App: "docuseal", Project: "docuseal", Environment: "prod"})))
	assert.ErrorContains(t, Validate(base(LinkSpec{})), "links")
	assert.ErrorContains(t, Validate(base(LinkSpec{App: "api", Environment: "prod"})), "project")
	assert.ErrorContains(t, Validate(base(LinkSpec{App: "Bad_Name"})), "links")

	many := make([]LinkSpec, 65)
	for i := range many {
		many[i] = LinkSpec{App: fmt.Sprintf("app%d", i)}
	}
	assert.ErrorContains(t, Validate(base(many...)), "64")
}

func TestDiffSpecReportsEachRemovedLinkAsAClear(t *testing.T) {
	live := map[string]interface{}{"links": []interface{}{
		map[string]interface{}{"app": "api", "namespace": "shop-test"},
		map[string]interface{}{"app": "ledger", "namespace": "finance-staging"},
	}}
	desired := map[string]interface{}{"links": []interface{}{
		map[string]interface{}{"app": "api", "namespace": "shop-test"},
	}}

	clears := Clears(DiffSpec(live, desired, nil, nil))

	require.Len(t, clears, 1)
	assert.Equal(t, "links", clears[0].Path)
	assert.Contains(t, clears[0].Live, "finance-staging/ledger")
}

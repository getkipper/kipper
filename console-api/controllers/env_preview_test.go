package controllers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/envtemplate"
)

const previewPassword = "hunter2:p@ss/word"

// previewFixture is an app bound to a postgres service, holding its own
// Secrets, with whatever spec.env the case needs.
func previewFixture(t *testing.T, env map[string]string) *EnvPreview {
	t.Helper()
	controller := true

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "docuseal", Namespace: "shop-prod", UID: "app-uid"},
		Spec: kipperv1.AppSpec{
			Image: "docuseal/docuseal:2.0", Port: 3000,
			Env:             env,
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_"}},
		},
	}
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-prod", UID: "svc-uid"},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-credentials", Namespace: "shop-prod",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: "db", UID: "svc-uid", Controller: &controller,
			}},
		},
		Data: map[string][]byte{
			"USERNAME": []byte("kipper"),
			"PASSWORD": []byte(previewPassword),
			"HOST":     []byte("db.shop-prod.svc.cluster.local"),
			"PORT":     []byte("5432"),
			"NAME":     []byte("docuseal"),
		},
	}
	// The workload's own Secrets, written through the Secrets tab.
	writer := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "app-docuseal-secrets", Namespace: "shop-prod"},
		Data:       map[string][]byte{"SECRET_KEY_BASE": []byte("rails-secret-key-base")},
	}

	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(app, svc, creds, writer).WithStatusSubresource(app).Build()

	preview, err := BuildEnvPreview(context.Background(), c, app)
	require.NoError(t, err)
	return preview
}

func previewVar(t *testing.T, p *EnvPreview, key string) EnvPreviewVariable {
	t.Helper()
	for _, v := range p.Variables {
		if v.Key == key {
			return v
		}
	}
	t.Fatalf("no variable %q in the preview", key)
	return EnvPreviewVariable{}
}

// TestBuildEnvPreview_MasksTheCredentialAndShowsTheRest is what the preview is
// for. Masking everything a binding contributes would leave
// ••••://••••:••••@•••• and tell an operator less than the template already
// does, so the components that are not credentials resolve visibly.
func TestBuildEnvPreview_MasksTheCredentialAndShowsTheRest(t *testing.T) {
	preview := previewFixture(t, map[string]string{
		"DATABASE_URL": "postgres://${DB_USERNAME}:${DB_PASSWORD:urlencode}@${DB_HOST}:${DB_PORT}/${DB_NAME}",
	})

	v := previewVar(t, preview, "DATABASE_URL")
	assert.True(t, v.IsTemplate)
	assert.NotContains(t, v.Resolved, previewPassword, "the preview must not carry the credential")
	assert.NotContains(t, v.Resolved, "hunter2", "nor any part of it")
	assert.Equal(t,
		"postgres://kipper:"+envtemplate.Mask+"@db.shop-prod.svc.cluster.local:5432/docuseal",
		v.Resolved)

	byName := map[string]EnvPreviewReference{}
	for _, ref := range v.References {
		byName[ref.Name] = ref
	}
	assert.True(t, byName["DB_PASSWORD"].Secret, "the password is reported as masked")
	assert.False(t, byName["DB_HOST"].Secret, "the host is not a credential")
	assert.False(t, byName["DB_USERNAME"].Secret, "nor is the username Kipper sets")
	assert.Equal(t, "binding", byName["DB_PASSWORD"].Origin)
	assert.Equal(t, "db", byName["DB_PASSWORD"].Source, "the service that answered is named")
}

// TestBuildEnvPreview_MasksEverythingInTheWorkloadsOwnSecrets: Kipper never
// decides what an operator put in the Secrets tab, so all of it is a secret.
func TestBuildEnvPreview_MasksEverythingInTheWorkloadsOwnSecrets(t *testing.T) {
	preview := previewFixture(t, map[string]string{
		"DERIVED": "prefix-${SECRET_KEY_BASE}-suffix",
	})

	v := previewVar(t, preview, "DERIVED")
	assert.Equal(t, "prefix-"+envtemplate.Mask+"-suffix", v.Resolved)
	assert.NotContains(t, v.Resolved, "rails-secret-key-base")
	assert.True(t, v.References[0].Secret)
	assert.Equal(t, "secrets", v.References[0].Origin)
}

// TestBuildEnvPreview_MaskingSurvivesAnEmbeddedValue is D13's own argument: a
// preview that hid credentials by searching the resolved text would find
// nothing here, because the value no longer resembles what it was built from.
func TestBuildEnvPreview_MaskingSurvivesAnEmbeddedValue(t *testing.T) {
	preview := previewFixture(t, map[string]string{
		"BASE64_KEY": "${DB_PASSWORD}",
		"REPEATED":   "${DB_PASSWORD}${DB_PASSWORD}",
		"ENCODED":    "${DB_PASSWORD:urlencode}",
	})

	for _, key := range []string{"BASE64_KEY", "REPEATED", "ENCODED"} {
		v := previewVar(t, preview, key)
		assert.NotContains(t, v.Resolved, previewPassword, "%s carries the credential", key)
		assert.NotContains(t, v.Resolved, "hunter2", "%s carries part of the credential", key)
		assert.NotContains(t, v.Resolved, "%40", "%s carries the encoded credential", key)
		assert.Contains(t, v.Resolved, envtemplate.Mask, "%s has no mask in it", key)
	}
}

// TestBuildEnvPreview_AvailableNamesCarryNoValues keeps the list of what an
// operator may reference from becoming a second way to read a credential.
func TestBuildEnvPreview_AvailableNamesCarryNoValues(t *testing.T) {
	preview := previewFixture(t, map[string]string{"LOG_LEVEL": "debug"})

	names := map[string]EnvPreviewName{}
	for _, n := range preview.Available {
		names[n.Name] = n
	}
	require.Contains(t, names, "DB_PASSWORD", "a name an operator may reference is offered")
	assert.True(t, names["DB_PASSWORD"].Secret, "and marked as one that will be masked")
	assert.Equal(t, "db", names["DB_PASSWORD"].Source)

	// The struct has no value field at all, so this asserts against the wire
	// form rather than the type: a field added later would show up here.
	assert.NotContains(t, marshalPreview(t, preview), previewPassword)
	assert.NotContains(t, marshalPreview(t, preview), "rails-secret-key-base")
}

// TestBuildEnvPreview_ReportsWhatWentWrong covers the three things the
// EnvResolved condition reports, per variable so the console can show each
// against the row that causes it.
func TestBuildEnvPreview_ReportsWhatWentWrong(t *testing.T) {
	preview := previewFixture(t, map[string]string{
		"TYPO":       "postgres://${DB_HSOT}/app",
		"INNER":      "${DB_HOST}",
		"TRANSITIVE": "http://${INNER}/health",
		"DB_HOST":    "i-am-overridden",
	})

	typo := previewVar(t, preview, "TYPO")
	require.Len(t, typo.References, 1)
	assert.False(t, typo.References[0].Resolved, "nothing defines DB_HSOT")
	assert.Contains(t, typo.Resolved, "${DB_HSOT}", "an unknown name reaches the process as written")

	transitive := previewVar(t, preview, "TRANSITIVE")
	require.Len(t, transitive.References, 1)
	assert.True(t, transitive.References[0].Transitive,
		"INNER is itself a template, and the render is a single pass")

	shadowed := previewVar(t, preview, "DB_HOST")
	assert.Equal(t, "db", shadowed.ShadowedBy,
		"the binding sets DB_HOST too and wins, so the pod never sees this one")
}

// TestBuildEnvPreview_ReportsShellStyleReferences: $(NAME) is Kubernetes' own
// syntax and Kipper resolves none of it. Neither does the kubelet, because
// spec.env reaches the pod through envFrom, where nothing is expanded — so the
// value arrives as written and the console says so.
func TestBuildEnvPreview_ReportsShellStyleReferences(t *testing.T) {
	preview := previewFixture(t, map[string]string{
		"WRONG_SYNTAX": "postgres://$(DB_HOST)/app",
	})

	v := previewVar(t, preview, "WRONG_SYNTAX")
	assert.Equal(t, []string{"DB_HOST"}, v.ShellStyle)
	assert.False(t, v.IsTemplate, "it references nothing Kipper resolves")
	assert.Empty(t, v.Resolved, "so there is nothing to preview beyond the template itself")
}

// TestBuildEnvPreview_SnippetsCarryTheEncoder is D4: wave 4 ships starter
// snippets that embed a credential in a URL, so an unencoded one would be
// Kipper shipping the bug it warns about. An operator's password routinely
// holds @ or :.
func TestBuildEnvPreview_SnippetsCarryTheEncoder(t *testing.T) {
	preview := previewFixture(t, map[string]string{})

	require.Len(t, preview.Snippets, 1)
	s := preview.Snippets[0]
	assert.Equal(t, "db", s.Service)
	assert.Equal(t, "postgres", s.Type)
	assert.Equal(t, "DATABASE_URL", s.Key)
	assert.Contains(t, s.Value, "${DB_PASSWORD:urlencode}",
		"a credential component in a URL must carry the encoder")
	assert.Contains(t, s.Value, "${DB_HOST}")

	// The snippet has to be something the resolver actually resolves, or it is
	// a suggestion that produces a literal.
	resolved, unresolved := envtemplate.Resolve(s.Value, func(name string) (string, bool) {
		return map[string]string{
			"DB_USERNAME": "kipper", "DB_PASSWORD": previewPassword,
			"DB_HOST": "db.svc", "DB_PORT": "5432", "DB_NAME": "docuseal",
		}[name], strings.HasPrefix(name, "DB_")
	})
	assert.Empty(t, unresolved, "every name a snippet uses is one the binding injects")
	assert.Contains(t, resolved, "hunter2%3Ap%40ss%2Fword", "the password is encoded for a URL component")
}

// TestBuildEnvPreview_RedisSnippetCarriesNoCredential follows the credentials
// change: redis starts with no --requirepass, so its binding carries no
// password and a URL holding one fails to connect.
func TestBuildEnvPreview_RedisSnippetCarriesNoCredential(t *testing.T) {
	key, value, ok := bindingSnippet("redis", "REDIS_")
	require.True(t, ok)
	assert.Equal(t, "REDIS_URL", key)
	assert.Equal(t, "redis://${REDIS_HOST}:${REDIS_PORT}", value)
	assert.NotContains(t, value, "PASSWORD")
}

// marshalPreview renders the preview as the handler sends it, so an assertion
// about what leaves the process reads the wire form rather than the struct.
func marshalPreview(t *testing.T, p *EnvPreview) string {
	t.Helper()
	b, err := json.Marshal(p)
	require.NoError(t, err)
	return string(b)
}

// TestBuildEnvPreview_MasksABindingKeyItDoesNotRecognise is the fail-closed
// rule, and the case that motivated it.
//
// A credentials Secret is Service-owned rather than immutable, and editing its
// data invalidates no ownership check, so a key Kipper never wrote can reach
// the table as an ordinary binding value. The rule used to name the two
// credentials and show everything else, which meant an operator's own
// API_TOKEN came back in full.
func TestBuildEnvPreview_MasksABindingKeyItDoesNotRecognise(t *testing.T) {
	controller := true
	//nolint:gosec // the fixture has to look like a real token: the point is that
	// a credential under an unrecognised key is masked rather than returned.
	const token = "ghp_operator_added_this_by_hand"

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "docuseal", Namespace: "shop-prod", UID: "app-uid"},
		Spec: kipperv1.AppSpec{
			Image: "docuseal/docuseal:2.0", Port: 3000,
			Env:             map[string]string{"FORWARDED": "${DB_API_TOKEN}"},
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_"}},
		},
	}
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-prod", UID: "svc-uid"},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-credentials", Namespace: "shop-prod",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: "db", UID: "svc-uid", Controller: &controller,
			}},
		},
		Data: map[string][]byte{
			"HOST":      []byte("db.shop-prod.svc.cluster.local"),
			"API_TOKEN": []byte(token),
		},
	}

	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(app, svc, creds).WithStatusSubresource(app).Build()
	preview, err := BuildEnvPreview(context.Background(), c, app)
	require.NoError(t, err)

	v := previewVar(t, preview, "FORWARDED")
	assert.Equal(t, envtemplate.Mask, v.Resolved, "a key the rule does not recognise is masked, not shown")
	assert.NotContains(t, marshalPreview(t, preview), token)
	assert.True(t, v.References[0].Secret)

	// The address beside it is still shown, or the preview would be useless.
	names := map[string]EnvPreviewName{}
	for _, n := range preview.Available {
		names[n.Name] = n
	}
	assert.False(t, names["DB_HOST"].Secret, "an address is not a credential")
}

// TestBuildEnvPreview_NamesARefusedBinding: a refused binding leaves its
// variables out of the table, so a reference to one reads as unresolved. That
// is what the next render will make of it, and not what a pod already running
// on the last good generation receives — the reconciler deliberately holds it
// there rather than publishing an environment with a hole in it. Naming the
// binding is the difference between a broken template and a broken binding.
func TestBuildEnvPreview_NamesARefusedBinding(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "docuseal", Namespace: "shop-prod", UID: "app-uid"},
		Spec: kipperv1.AppSpec{
			Image: "docuseal/docuseal:2.0", Port: 3000,
			Env:             map[string]string{"DATABASE_URL": "postgres://${DB_HOST}/app"},
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_"}},
		},
	}
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-prod", UID: "svc-uid"},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	// Owned by nothing, so the binding may not read it.
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "shop-prod"},
		Data:       map[string][]byte{"HOST": []byte("db.shop-prod.svc.cluster.local")},
	}

	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(app, svc, creds).WithStatusSubresource(app).Build()
	preview, err := BuildEnvPreview(context.Background(), c, app)
	require.NoError(t, err)

	require.NotEmpty(t, preview.Refused, "a refused binding must be reported, not silently dropped")
	assert.Contains(t, preview.Refused[0], "db")

	v := previewVar(t, preview, "DATABASE_URL")
	assert.False(t, v.References[0].Resolved, "its variables are not in the table")
}

// TestBuildEnvPreview_AnEmptyResolutionIsStillAResult: an empty value is a real
// environment value, so a template that resolves to one has to be
// distinguishable from a value that was never a template. Testing the resolved
// string for content collapses the two, and the console then shows nothing at
// all for a variable that resolved perfectly well.
func TestBuildEnvPreview_AnEmptyResolutionIsStillAResult(t *testing.T) {
	preview := previewFixture(t, map[string]string{
		"EMPTY":          "",
		"COPY":           "${EMPTY}",
		"NOT_A_TEMPLATE": "plain",
	})

	copied := previewVar(t, preview, "COPY")
	assert.True(t, copied.IsTemplate, "it references a name, so it is a template")
	assert.Equal(t, "", copied.Resolved)
	require.Len(t, copied.References, 1)
	assert.True(t, copied.References[0].Resolved, "EMPTY is defined; it is just empty")

	plain := previewVar(t, preview, "NOT_A_TEMPLATE")
	assert.False(t, plain.IsTemplate)

	// The field is always on the wire, so presence cannot be read as a signal.
	assert.Contains(t, marshalPreview(t, preview), `"resolved":""`)
}

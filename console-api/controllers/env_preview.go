package controllers

import (
	"context"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/envtemplate"
)

// EnvPreviewVariable is one spec.env entry as the preview reports it.
type EnvPreviewVariable struct {
	Key string `json:"key"`
	// Template is the value as written on the CR, references and all.
	Template string `json:"template"`
	// Resolved is what the next render will produce, with every secret-derived
	// substitution masked. Read it together with IsTemplate rather than by
	// testing it for content: a template resolving to the empty string is a
	// real result, and dropping the field there would make it indistinguishable
	// from a value that was never a template.
	Resolved   string `json:"resolved"`
	IsTemplate bool   `json:"isTemplate"`
	// References is every name this value refers to, in order of appearance.
	References []EnvPreviewReference `json:"references,omitempty"`
	// ShellStyle names the $(NAME) references in this value. Kipper resolves
	// none of them and neither does the kubelet, so they reach the process as
	// written; the console says so rather than leaving it to be discovered.
	ShellStyle []string `json:"shellStyle,omitempty"`
	// ShadowedBy names the source that sets this key too and wins, so the pod
	// never sees what was written here. Empty when nothing overrides it.
	ShadowedBy string `json:"shadowedBy,omitempty"`
}

// EnvPreviewReference is one ${NAME} inside a value, and what answered it.
type EnvPreviewReference struct {
	Name string `json:"name"`
	// Origin is which kind of source answered: spec.env, secrets, binding,
	// link or runtime. Empty when nothing did.
	Origin string `json:"origin,omitempty"`
	// Source names the Secret or Service it came from.
	Source   string `json:"source,omitempty"`
	Resolved bool   `json:"resolved"`
	// Secret says the value was masked rather than shown.
	Secret bool `json:"secret"`
	// Transitive marks a reference to another spec.env entry that is itself a
	// template. Resolution is a single pass, so the inner reference survives
	// into the resolved value rather than resolving.
	Transitive bool `json:"transitive"`
}

// EnvPreviewName is one variable in scope, offered as something to reference.
//
// Names only. A value never appears here: the per-variable preview above is
// where a resolved value is shown, and it is masked.
type EnvPreviewName struct {
	Name   string `json:"name"`
	Origin string `json:"origin"`
	Source string `json:"source,omitempty"`
	Secret bool   `json:"secret"`
}

// EnvPreviewSnippet is a starter template for one of the workload's bindings.
type EnvPreviewSnippet struct {
	Service string `json:"service"`
	Type    string `json:"type"`
	Key     string `json:"key"`
	Value   string `json:"value"`
}

// EnvPreview is everything the console's Env tab shows beyond the raw map.
type EnvPreview struct {
	Variables []EnvPreviewVariable `json:"variables"`
	Available []EnvPreviewName     `json:"available"`
	Snippets  []EnvPreviewSnippet  `json:"snippets,omitempty"`
	// Refused names the bindings whose credentials this workload may not read.
	// Their variables are absent from everything above, so a reference to one
	// reads as unresolved — which is what the next render will make of it, and
	// not what a pod already running on the last good generation receives.
	// Saying so is the difference between a broken template and a broken
	// binding, and they are fixed in different places.
	Refused []string `json:"refused,omitempty"`
}

// BuildEnvPreview resolves an App's spec.env against the sources its
// environment is built from, and reports the result with every secret-derived
// value masked.
//
// **This is what the next render will produce, not what a running pod holds.**
// The two differ, and the difference is not an edge case: a pod reads one
// immutable generation Secret, and the reconciler deliberately leaves it there
// when a binding stops being readable rather than publishing an environment
// with a hole in it. So a refused binding shows here as an unresolved
// reference while the pod goes on serving the value it was given. Refused
// names it, so the console can say which of the two is happening.
//
// Reading the published generation instead would answer the other question and
// lose the one the preview exists for: that Secret holds the resolved values
// already flattened, with no record of which source each came from, so nothing
// could be masked without inspecting the text — which is the approach D13 rules
// out. The generation is where the restart banner gets its answer.
//
// It reads. The controller's own path derives the per-binding Secrets and the
// link policy before building this table, and neither happens here.
//
// The table comes from the same builder pod construction uses, so the preview
// cannot disagree with the pod about precedence, prefixes or which bindings are
// injectable. Answering from a second implementation is how a preview ends up
// showing a value the process never receives.
//
// The caller must gate this on the deployer role. Env GET is viewer-readable
// while env mutation is deployer-only, so an unmasked-by-default preview
// handed to a viewer would widen who can read a credential (D13).
func BuildEnvPreview(ctx context.Context, c client.Client, app *kipperv1.App) (*EnvPreview, error) {
	links, _, err := ResolveLinks(ctx, c, app)
	if err != nil {
		return nil, fmt.Errorf("resolving links: %w", err)
	}

	// No rendered snapshot: this pass renders nothing, so every binding source
	// is read back from its Secret in the cluster.
	sources, refused, err := appEnvSources(ctx, c, app, links, nil)
	if err != nil {
		return nil, fmt.Errorf("building environment sources: %w", err)
	}

	table, err := effectiveEnv(ctx, c, app.Namespace, sources)
	if err != nil {
		return nil, fmt.Errorf("building effective environment: %w", err)
	}

	preview := &EnvPreview{
		Variables: previewVariables(app.Spec.Env, table),
		Available: previewNames(table),
		Snippets:  previewSnippets(ctx, c, app),
		Refused:   refused,
	}
	return preview, nil
}

// previewVariables resolves each spec.env entry and describes what happened.
func previewVariables(env map[string]string, table map[string]envEntry) []EnvPreviewVariable {
	out := make([]EnvPreviewVariable, 0, len(env))
	for _, key := range sortedKeys(env) {
		value := env[key]
		names := envtemplate.Names(value)

		v := EnvPreviewVariable{
			Key:        key,
			Template:   value,
			IsTemplate: len(names) > 0,
			ShellStyle: envtemplate.ShellStyleRefs(value),
		}

		// The table holds the winner for every name, so an entry from anywhere
		// but spec.env means this key is written here and overridden in the pod.
		if entry, ok := table[key]; ok && entry.origin != originSpecEnv {
			v.ShadowedBy = entry.source
		}

		for _, name := range names {
			entry, found := table[name]
			ref := EnvPreviewReference{
				Name:     name,
				Resolved: found,
				Secret:   found && entrySecret(entry),
			}
			if found {
				ref.Origin = string(entry.origin)
				ref.Source = entry.source
				// A reference to another template in this same spec.env does
				// not resolve through: the render is a single pass, so the
				// inner reference survives into the value.
				ref.Transitive = entry.origin == originSpecEnv && len(envtemplate.Names(entry.value)) > 0
			}
			v.References = append(v.References, ref)
		}

		if v.IsTemplate {
			v.Resolved, _ = envtemplate.ResolveMasked(value, func(name string) (envtemplate.Value, bool) {
				entry, ok := table[name]
				if !ok {
					return envtemplate.Value{}, false
				}
				return envtemplate.Value{Text: entry.value, Secret: entrySecret(entry)}, true
			})
		}

		out = append(out, v)
	}
	return out
}

// previewNames lists every variable in scope, so the editor can offer them
// rather than leaving an operator to guess at the prefix a binding used.
func previewNames(table map[string]envEntry) []EnvPreviewName {
	out := make([]EnvPreviewName, 0, len(table))
	for _, name := range sortedTableKeys(table) {
		entry := table[name]
		out = append(out, EnvPreviewName{
			Name:   name,
			Origin: string(entry.origin),
			Source: entry.source,
			Secret: entrySecret(entry),
		})
	}
	return out
}

// entrySecret says whether a value should be masked rather than shown.
//
// The decision is made from where the value came from, never from what it looks
// like. D13 rules the second one out and the reason is concrete:
// BASE64_KEY=${SECRET_KEY} produces a value that no longer resembles the
// credential it was built from, so searching the resolved text finds nothing to
// hide.
//
// Everything in the workload's own Secrets is a secret: that is what the
// Secrets tab is for, and Kipper never decides what an operator put there.
//
// A binding is not masked wholesale, because it carries an address and an
// identity beside the credential and hiding those would leave a preview reading
// ••••://••••:••••@••••, which tells an operator less than the template already
// did. Which of its keys are safe to show is kipperv1.IsSensitiveCredentialKey's
// question rather than this one's, and it answers with an allowlist so a key
// nobody anticipated is masked rather than shown.
//
// The key is the one the source itself uses. Reading the prefixed name instead
// would make the decision depend on what an operator called the binding.
func entrySecret(entry envEntry) bool {
	switch entry.origin {
	case originWriterSecret:
		return true
	case originBinding:
		return kipperv1.IsSensitiveCredentialKey(entry.key)
	}
	return false
}

// previewSnippets offers a starter template per binding, composing the URL the
// service type's clients expect.
//
// Every credential component carries :urlencode. Wave 4 shipping snippets that
// embed a credential in a URL is exactly why the modifier exists (D4): an
// operator's own password routinely holds @ or :, and a snippet without the
// encoder would be Kipper shipping the bug it warns about.
func previewSnippets(ctx context.Context, c client.Client, app *kipperv1.App) []EnvPreviewSnippet {
	var out []EnvPreviewSnippet
	for _, b := range app.Spec.ServiceBindings {
		svcType, known, err := bindingServiceType(ctx, c, b.Name, app.Namespace)
		if err != nil || !known {
			continue
		}
		prefix := b.Prefix
		if prefix == "" {
			prefix = kipperv1.DefaultBindingPrefix(svcType)
		}
		key, value, ok := bindingSnippet(svcType, prefix)
		if !ok {
			continue
		}
		out = append(out, EnvPreviewSnippet{Service: b.Name, Type: svcType, Key: key, Value: value})
	}
	return out
}

// bindingSnippet is the connection string a bound service type's clients read,
// built from the variables that binding injects.
func bindingSnippet(svcType, p string) (key, value string, ok bool) {
	switch svcType {
	case "postgres":
		return "DATABASE_URL", fmt.Sprintf(
			"postgresql://${%sUSERNAME}:${%sPASSWORD:urlencode}@${%sHOST}:${%sPORT}/${%sNAME}", p, p, p, p, p), true
	case "mysql":
		return "DATABASE_URL", fmt.Sprintf(
			"mysql://${%sUSERNAME}:${%sPASSWORD:urlencode}@${%sHOST}:${%sPORT}/${%sNAME}", p, p, p, p, p), true
	case "mongodb":
		return "MONGODB_URI", fmt.Sprintf(
			"mongodb://${%sUSERNAME}:${%sPASSWORD:urlencode}@${%sHOST}:${%sPORT}/${%sNAME}?authSource=admin", p, p, p, p, p), true
	case "rabbitmq":
		return "AMQP_URL", fmt.Sprintf(
			"amqp://${%sUSERNAME}:${%sPASSWORD:urlencode}@${%sHOST}:${%sPORT}/${%sVHOST:urlencode}", p, p, p, p, p), true
	case "redis":
		// No credential in it. Redis starts with no --requirepass, so the
		// binding carries none, and a URL holding one fails to connect: redis
		// answers AUTH with an error when no password is set.
		return "REDIS_URL", fmt.Sprintf("redis://${%sHOST}:${%sPORT}", p, p), true
	case "opensearch":
		return "OPENSEARCH_URL", fmt.Sprintf("http://${%sHOST}:${%sPORT}", p, p), true
	}
	return "", "", false
}

// sortedTableKeys orders the table for a stable answer, so the console renders
// the same list twice running.
func sortedTableKeys(table map[string]envEntry) []string {
	keys := make([]string, 0, len(table))
	for k := range table {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

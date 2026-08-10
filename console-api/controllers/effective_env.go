package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/envtemplate"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// envOrigin says where a variable in a workload's environment came from.
//
// It is carried per entry rather than worked out from the value, because
// anything that decides by looking at the text is defeated by
// BASE64_KEY=${SECRET_KEY}: the value no longer resembles the credential it
// was built from.
type envOrigin string

const (
	// originSpecEnv is the workload's own spec.env, the lowest tier and the
	// only one whose values are templates.
	originSpecEnv envOrigin = "spec.env"
	// originWriterSecret is the <kind>-<name>-secrets Secret, written by the
	// Secrets tab and the CLI rather than derived from the CR.
	originWriterSecret envOrigin = "secrets"
	// originBinding is a service binding's credentials.
	originBinding envOrigin = "binding"
	// originLink is the address of an app this one links to.
	originLink envOrigin = "link"
	// originRuntime is a variable the platform sets on the container itself.
	originRuntime envOrigin = "runtime"
)

// envSource is one place a workload's environment comes from, in the order the
// kubelet applies them: every envFrom source in the order it appears on the
// container, then the container's own env, which beats all of them.
//
// Pod construction and template resolution both read this list, and that is the
// whole point of it. The two used to answer the same questions separately —
// which bindings may be injected, what prefix an empty one gets, which Secret a
// binding maps to — and a resolver that answered any of them differently would
// resolve ${DB_PASSWORD} from a credential the pod is refused.
type envSource struct {
	origin envOrigin

	// secret is the Secret the pod reads this source from through envFrom.
	// Empty for a source the controller sets on the container directly.
	secret string

	// prefix is prepended to every key this source contributes.
	prefix string

	// values is the content when the controller holds it rather than reading it
	// back: the spec.env this pass is about to render, and the variables the
	// platform sets itself. held says which, because an empty spec.env is real
	// content — falling back to the Secret there would feed the last render's
	// output back in as this render's input.
	//
	// For spec.env these are the raw templates rather than what lands in the
	// Secret. Resolution reads them as they were written, which is what makes
	// A=${B} with B=${A} terminate instead of deepening on every pass.
	values []corev1.EnvVar
	held   bool

	// direct marks a source the pod carries as container.Env. Kubernetes gives
	// those precedence over every envFrom source whatever the order here.
	direct bool

	// service is the Service a binding source draws from.
	service string
}

// name is what an operator should be told this source is, for a message that
// says which of them set a variable.
func (s envSource) name() string {
	switch {
	case s.service != "":
		return s.service
	case s.secret != "":
		return s.secret
	default:
		return string(s.origin)
	}
}

// envEntry is one variable the pod will see, and where it came from.
type envEntry struct {
	value  string
	origin envOrigin
	source string
	// key is the name the source itself uses, before the binding prefix. The
	// preview decides what to mask from it: a binding contributes PASSWORD
	// alongside HOST, PORT and USERNAME, and masking the lot would leave
	// nothing worth previewing. Reading the prefixed name instead would make
	// that decision depend on what the operator called the binding.
	key string
}

// appEnvSources is everything an App's container reads, in the order it reads
// it. Links come last because the pod carries them as container.Env, which
// beats every envFrom source.
//
// The refused list names the bindings whose credentials this workload may not
// read; the caller decides what to do about them.
func appEnvSources(ctx context.Context, c client.Client, app *kipperv1.App, links []ResolvedLink, rendered renderedBindings) ([]envSource, []string, error) {
	sources := []envSource{
		{
			origin: originSpecEnv,
			secret: secretname.Env(secretname.KindApp, app.Name),
			values: envVarsOf(app.Spec.Env),
			held:   true,
		},
		{
			origin: originWriterSecret,
			secret: secretname.Secrets(secretname.KindApp, app.Name),
		},
	}

	bindings, refused, err := bindingEnvSources(ctx, c, app, secretname.KindApp, app.Spec.ServiceBindings, rendered)
	if err != nil {
		return nil, nil, err
	}
	sources = append(sources, bindings...)

	if vars := linkEnvVars(links); len(vars) > 0 {
		sources = append(sources, envSource{origin: originLink, values: vars, held: true, direct: true})
	}
	return sources, refused, nil
}

// functionMode is the pod shape a Function's environment is being built for.
// The shapes differ in the variables the platform sets on the container, which
// beat every envFrom source, so a table has to say which shape it describes.
type functionMode int

const (
	// functionShared is what both shapes agree on. The env Secret is rendered
	// once and read by both, so a template can only resolve against a variable
	// that means the same thing in either — ${KIPPER_MODE} would otherwise be
	// baked to "batch" and read by the serving pod.
	functionShared functionMode = iota
	// functionServing is the long-running HTTP Deployment.
	functionServing
	// functionBatch is a cron or test run, which the runtime dispatches on.
	functionBatch
)

// functionEnvSources is everything a Function's pods read that has to be
// resolved against the cluster, in order. It is resolved once per pass and
// shared by every pod shape; withFunctionRuntime adds what differs between them.
//
// A binding this Function may not read is refused, exactly as an App's is. The
// caller decides what to do about it, and both kinds now stop the pass: a
// workload rendered without credentials it declares starts and fails on its
// first connection, which is further from the cause than not starting at all.
func functionEnvSources(ctx context.Context, c client.Client, fn *kipperv1.Function, rendered renderedBindings) ([]envSource, []string, error) {
	sources := []envSource{
		{
			origin: originSpecEnv,
			secret: secretname.Env(secretname.KindFunction, fn.Name),
			values: envVarsOf(fn.Spec.Env),
			held:   true,
		},
		{
			origin: originWriterSecret,
			secret: secretname.Secrets(secretname.KindFunction, fn.Name),
		},
	}

	bindings, refused, err := bindingEnvSources(ctx, c, fn, secretname.KindFunction, fn.Spec.ServiceBindings, rendered)
	if err != nil {
		return nil, nil, err
	}
	sources = append(sources, bindings...)
	return sources, refused, nil
}

// withFunctionRuntime returns these sources as one pod shape sees them, adding
// the variables the platform sets on that shape's container.
//
// It reads nothing. The Function used to resolve its whole source list once per
// pod shape, and every one of those calls read the Service again — so a Service
// that disappeared between the reconcile's refusal gate and the Deployment
// render changed bindingIsDerived's answer, moved the binding to a Secret name
// nothing had rendered, and the Deployment was written without it. Passing the
// gate and then failing open in the same pass is worse than either. The cluster
// is asked once now, and every shape is derived from that one answer.
func withFunctionRuntime(fn *kipperv1.Function, sources []envSource, mode functionMode, trigger string) []envSource {
	vars := functionRuntimeVars(fn, mode, trigger)
	if len(vars) == 0 {
		return sources
	}
	// Copied rather than appended in place: the caller's slice is shared by
	// every shape, and appending to it twice would have the second write over
	// the first's runtime source.
	out := make([]envSource, 0, len(sources)+1)
	out = append(out, sources...)
	return append(out, envSource{origin: originRuntime, values: vars, held: true, direct: true})
}

// functionRuntimeVars is what the platform tells the function runtime about the
// pod it is running in.
//
// A batch pod is told which mode it is in and what triggered it, and the
// serving pod is told neither, so those two are absent from the shared shape:
// a value resolved against them would be written into a Secret both shapes
// read.
func functionRuntimeVars(fn *kipperv1.Function, mode functionMode, trigger string) []corev1.EnvVar {
	var vars []corev1.EnvVar
	if mode == functionBatch {
		vars = append(vars,
			corev1.EnvVar{Name: "KIPPER_MODE", Value: "batch"},
			corev1.EnvVar{Name: "KIPPER_TRIGGER", Value: trigger},
		)
	}
	// Inline code is mounted at a path the runtime has to be told about,
	// whichever shape is running it.
	if fn.Spec.Source != nil && fn.Spec.Source.Code != "" {
		_, path := runtimeHandler(fn.Spec.Runtime)
		vars = append(vars, corev1.EnvVar{Name: "KIPPER_FUNCTION_PATH", Value: path})
	}
	return vars
}

// jobEnvSources is everything a Job's container reads.
//
// A Job has no service bindings and no links, so its own spec.env is the whole
// table. A reference to a credential therefore stays literal, which is the
// truth about a Job today rather than a limit of the resolver.
func jobEnvSources(job *kipperv1.Job) []envSource {
	return []envSource{{
		origin: originSpecEnv,
		secret: secretname.Env(secretname.KindJob, job.Name),
		values: envVarsOf(job.Spec.Env),
		held:   true,
	}}
}

// bindingEnvSources returns a source for every binding whose credentials this
// workload may read, in the order the bindings are declared, and the name of
// each binding refused.
//
// Provenance is the gate rather than the name (see injectableBindingSecret).
// The App and Function reconcilers used to answer that and the empty-prefix
// default separately, which is exactly the drift the shared table exists to
// stop.
func bindingEnvSources(ctx context.Context, c client.Client, owner client.Object, kind secretname.Kind, bindings []kipperv1.ServiceBinding, rendered renderedBindings) ([]envSource, []string, error) {
	var sources []envSource
	var refused []string

	for _, b := range bindings {
		// One read of the Service answers both questions it decides: which
		// Secret this binding injects, and what prefix an unset one gets. A read
		// that fails stops the pass rather than falling back, because the
		// fallback would name a different Secret than the one the render
		// produced under the same binding.
		svcType, typeKnown, err := bindingServiceType(ctx, c, b.Name, owner.GetNamespace())
		if err != nil {
			return nil, nil, err
		}
		secret := bindingSecretName(b, svcType, typeKnown, kind, owner.GetName())

		// What this pass rendered is both newer and better attested than what a
		// read would return: producing it required the Service's own
		// controller-owner UID to match, which is the gate's test and then some.
		// Reading it back would answer from the informer cache, which is a pass
		// behind its own write.
		values, held := rendered.envVars(secret)
		if !held {
			ok, err := injectableBindingSecret(ctx, c, b, kind, owner)
			if err != nil {
				return nil, nil, err
			}
			if !ok {
				refused = append(refused, fmt.Sprintf("%s (no usable %s Secret)", b.Name, secret))
				continue
			}
		}

		prefix := b.Prefix
		if prefix == "" && typeKnown {
			prefix = kipperv1.DefaultBindingPrefix(svcType)
		}
		sources = append(sources, envSource{
			origin:  originBinding,
			secret:  secret,
			prefix:  prefix,
			service: b.Name,
			values:  values,
			held:    held,
		})
	}
	return sources, refused, nil
}

// bindingServiceType is the type of the Service a binding names, or "" when it
// cannot be read. It decides both the Secret the binding injects and, when the
// binding sets no prefix of its own, the prefix its variables carry — the same
// contract the bind handler and the console preview (InjectedEnvNames) honour,
// so all of them agree on the names a binding injects.
// bindingServiceType is the type of the Service a binding names. found is false
// when there is no such Service; a read that fails for any other reason is
// returned as an error.
//
// The three answers are distinct on purpose. The type decides both the Secret a
// binding injects and the prefix its variables carry, so guessing when the read
// failed means a transient error silently changes which Secret the pod is told
// to read — and the render, which resolved the same question from a Service it
// read successfully, would then disagree with it.
func bindingServiceType(ctx context.Context, c client.Client, serviceName, namespace string) (svcType string, found bool, err error) {
	var svc kipperv1.Service
	if getErr := c.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: namespace}, &svc); getErr != nil {
		if errors.IsNotFound(getErr) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading service %q: %w", serviceName, getErr)
	}
	return svc.Spec.Type, true, nil
}

// envFrom is the container's envFrom for these sources, in order.
//
// Every reference is optional, so a Secret that does not exist leaves its
// variables unset instead of holding the pod at ContainerCreating.
func envFrom(generation string) []corev1.EnvFromSource {
	// Not optional, unlike the several sources this replaced. Each of those
	// carried part of the environment, so a missing one started the pod with a
	// quiet gap in it. This one carries all of it, so a pod that cannot read it
	// has no published environment at all and must not start.
	optional := false
	return []corev1.EnvFromSource{{
		SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: generation},
			Optional:             &optional,
		},
	}}
}

// generationOnContainer returns the env generation a running container names,
// or "" when it names none — a workload that has not been published yet, or one
// still on the several mutable Secrets this replaced.
func generationOnContainer(containers []corev1.Container, kind secretname.Kind, workload string) string {
	if len(containers) == 0 {
		return ""
	}
	prefix := secretname.EnvGenerationPrefix(kind, workload)
	for _, ef := range containers[0].EnvFrom {
		if ef.SecretRef != nil && strings.HasPrefix(ef.SecretRef.Name, prefix) {
			return ef.SecretRef.Name
		}
	}
	return ""
}

// directEnv is the container's own env for these sources, in order.
func directEnv(sources []envSource) []corev1.EnvVar {
	var out []corev1.EnvVar
	for _, s := range sources {
		if !s.direct {
			continue
		}
		out = append(out, s.values...)
	}
	return out
}

// effectiveEnv flattens the sources into the variables the pod will see, a
// later source winning a name an earlier one also sets.
//
// The envFrom sources are applied in their own order and the direct ones after
// all of them, which is the kubelet's rule rather than this list's. Following
// the list instead would work only while every direct source happens to sit at
// the end of it, and the day one did not the table would report a value the pod
// overrides.
//
// A Secret that does not exist contributes nothing, matching the optional
// envFrom that reads it: the pod starts without those variables, so the table
// must not claim them either.
func effectiveEnv(ctx context.Context, c client.Client, namespace string, sources []envSource) (map[string]envEntry, error) {
	return walkEnvSources(ctx, c, namespace, sources, nil, true)
}

// publishedEnv is what one generation Secret holds: every envFrom source
// flattened as the kubelet would flatten it, with spec.env contributing the
// values the render produced rather than the templates it read.
//
// The resolution table cannot be published as it stands, and the reason is the
// point of the split. spec.env is held raw in that table because it is what
// renderEnv resolves against, and holding it raw is what stops one render
// becoming the next render's input. The pod has never read those templates; it
// reads what the render wrote afterwards.
//
// The direct sources are left out because they travel on the pod template
// itself, so they are already published atomically with the generation name.
func publishedEnv(ctx context.Context, c client.Client, namespace string, sources []envSource, resolved map[string]string) (map[string]string, error) {
	table, err := walkEnvSources(ctx, c, namespace, sources, resolved, false)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(table))
	for k, e := range table {
		out[k] = e.value
	}
	return out, nil
}

// envDigest names a published environment by its content.
//
// Keys are sorted and every field is length-prefixed, so no pair of maps can
// serialise to the same bytes: without the lengths, {"A":"1B", "":"2"} and
// {"A":"1", "B":"2"} are the same stream, and two environments sharing a name
// would mean a pod reading one while its template promised the other.
func envDigest(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		// A hash.Hash never reports a write error, per its own contract.
		_, _ = fmt.Fprintf(h, "%d:%s%d:%s", len(k), k, len(env[k]), env[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// walkEnvSources applies the sources in the order the pod's environment is
// built. specEnv, when non-nil, replaces what the spec.env source contributes,
// and includeDirect says whether the container's own env is part of the answer.
func walkEnvSources(ctx context.Context, c client.Client, namespace string, sources []envSource,
	specEnv map[string]string, includeDirect bool) (map[string]envEntry, error) {
	table := map[string]envEntry{}

	apply := func(s envSource, key, value string) {
		table[s.prefix+key] = envEntry{value: value, origin: s.origin, source: s.name(), key: key}
	}

	for _, s := range sources {
		if s.direct {
			continue
		}
		if s.held {
			if specEnv != nil && s.origin == originSpecEnv {
				// Published rather than resolved against: the render's output
				// takes the place of the templates it read.
				for k, v := range specEnv {
					apply(s, k, v)
				}
				continue
			}
			for _, v := range s.values {
				apply(s, v.Name, v.Value)
			}
			continue
		}

		var secret corev1.Secret
		err := c.Get(ctx, types.NamespacedName{Name: s.secret, Namespace: namespace}, &secret)
		if errors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading secret %q: %w", s.secret, err)
		}
		for k, v := range secret.Data {
			apply(s, k, string(v))
		}
	}

	if !includeDirect {
		return table, nil
	}
	for _, s := range sources {
		if !s.direct {
			continue
		}
		for _, v := range s.values {
			apply(s, v.Name, v.Value)
		}
	}
	return table, nil
}

// envDiagnostics is what a render learned about a workload's templates, for the
// EnvResolved condition.
type envDiagnostics struct {
	// unresolved names were referenced and nothing in the pod's environment
	// defines them, so they reach the process as written.
	unresolved []string
	// shadowed keys are set in spec.env and overridden by a higher-precedence
	// source, so the pod never sees what was written.
	shadowed []string
	// transitive references name another spec.env entry that is itself a
	// template. Resolution is a single pass, so the inner reference survives
	// into the resolved value.
	transitive []string
}

func (d envDiagnostics) empty() bool {
	return len(d.unresolved) == 0 && len(d.shadowed) == 0 && len(d.transitive) == 0
}

// message reads as an operator should hear it, one clause per kind of problem.
func (d envDiagnostics) message() string {
	var parts []string
	if len(d.unresolved) > 0 {
		parts = append(parts, "nothing in this workload's environment defines "+
			joinWithinConditionMessage(d.unresolved)+", so the reference reaches the process as written")
	}
	if len(d.shadowed) > 0 {
		parts = append(parts, "set in env and overridden in the pod: "+joinWithinConditionMessage(d.shadowed))
	}
	if len(d.transitive) > 0 {
		parts = append(parts, "resolved once rather than repeatedly: "+joinWithinConditionMessage(d.transitive))
	}
	return joinWithinConditionMessage(parts)
}

// renderEnv resolves a workload's spec.env against the environment its pod will
// see, and reports what an operator would want to know about the result.
//
// Only spec.env values are templates. Secret and credential values are resolved
// from and never resolved in, so a password that happens to contain ${...} is
// injected as it is.
func renderEnv(env map[string]string, table map[string]envEntry) (map[string]string, envDiagnostics) {
	resolved, unresolved := envtemplate.ResolveAll(env, func(name string) (string, bool) {
		entry, ok := table[name]
		return entry.value, ok
	})

	diag := envDiagnostics{unresolved: unresolved}
	for _, key := range sortedKeys(env) {
		// The table holds the winner for every name, so an entry from anywhere
		// but spec.env means this key is set and then overridden.
		if entry, ok := table[key]; ok && entry.origin != originSpecEnv {
			diag.shadowed = append(diag.shadowed, fmt.Sprintf("%s (by %s)", key, entry.source))
		}
		for _, ref := range envtemplate.Names(env[key]) {
			// Only a reference to another template in the same spec.env is
			// worth reporting. A Secret value containing ${...} is literal text
			// nothing was ever going to resolve.
			entry, ok := table[ref]
			if !ok || entry.origin != originSpecEnv || len(envtemplate.Names(entry.value)) == 0 {
				continue
			}
			diag.transitive = append(diag.transitive,
				fmt.Sprintf("%s references %s, which is itself a template", key, ref))
		}
	}
	return resolved, diag
}

// applyEnvResolvedCondition records what the env render made of the workload's
// templates. In-memory only: the caller's updateStatus persists it, so this
// never costs a status write of its own.
//
// A workload with no environment carries no condition, so removing the last
// variable clears a warning about it rather than leaving one behind.
func applyEnvResolvedCondition(conditions *[]metav1.Condition, generation int64, envCount int, diag envDiagnostics) {
	if envCount == 0 {
		apimeta.RemoveStatusCondition(conditions, kipperv1.ConditionEnvResolved)
		return
	}

	cond := metav1.Condition{
		Type:               kipperv1.ConditionEnvResolved,
		Status:             metav1.ConditionTrue,
		Reason:             "Resolved",
		Message:            "every reference in this workload's environment resolved",
		ObservedGeneration: generation,
	}
	if !diag.empty() {
		cond.Status = metav1.ConditionFalse
		cond.Message = diag.message()
		switch {
		case len(diag.unresolved) > 0:
			cond.Reason = "UnresolvedReferences"
		case len(diag.transitive) > 0:
			cond.Reason = "TransitiveReferences"
		default:
			cond.Reason = "ShadowedKeys"
		}
	}
	apimeta.SetStatusCondition(conditions, cond)
}

// envVarsOf turns a spec.env map into ordered variables, so a table built from
// it twice is the same table and a condition does not flap between passes.
func envVarsOf(env map[string]string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(env))
	for _, k := range sortedKeys(env) {
		out = append(out, corev1.EnvVar{Name: k, Value: env[k]})
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

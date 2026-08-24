// Package secretname holds the names of the Secrets a workload's controller
// derives for it, because more than one module has to agree about them.
//
// Secret names are namespace-global while workload names are unique only within
// a kind, so an App, a Function and a Job may all be called "api" in one
// namespace. Naming their Secrets after the workload alone gives all three the
// same object: two controllers author it in turn and a third reads whatever
// happens to be there. That is why the kind is part of every name here.
//
// It matters more than it looks. These Secrets carry resolved service
// credentials, so a workload reading the wrong one reads another workload's
// database password, and `writerSecretAmbiguous` existed in the App controller
// only to detect this collision rather than prevent it.
//
// The CLI names the same Secrets to read, write and delete them, and it is a
// separate module from the reconcilers that create them. One of them spelling a
// name differently means writing a Secret the workload never reads, so the rule
// lives here rather than in each of them.
package secretname

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Kind identifies the workload a derived Secret belongs to.
type Kind string

const (
	// KindApp is a long-running App workload.
	KindApp Kind = "app"
	// KindFunction is a Function workload, in any of its HTTP, cron or test modes.
	KindFunction Kind = "function"
	// KindJob is a Job workload, one-off or scheduled.
	KindJob Kind = "job"
)

// Env is the Secret holding a workload's environment variables, rendered by its
// controller from the CR's spec.env.
func Env(kind Kind, workload string) string {
	return string(kind) + "-" + workload + "-env"
}

// EnvGeneration is the Secret holding one published generation of a workload's
// complete environment, named by a digest of its own content.
//
// The environment is published as one immutable object so that a pod reads one
// generation or another and never a mix of two. The name carries the digest
// because the pod template names it: a change to any value produces a different
// name, so moving a workload to a new environment is a pod-template update,
// which is atomic, rather than several Secret writes, which are not.
func EnvGeneration(kind Kind, workload, digest string) string {
	return EnvGenerationPrefix(kind, workload) + digest
}

// EnvGenerationPrefix is what every generation of one workload's environment
// shares, so a controller can recognise the generation a running container
// names without knowing which digest it is.
func EnvGenerationPrefix(kind Kind, workload string) string {
	return string(kind) + "-" + workload + "-env-"
}

// Secrets is the Secret holding a workload's own secret values, written through
// the Secrets tab and the CLI rather than derived from the CR.
func Secrets(kind Kind, workload string) string {
	return string(kind) + "-" + workload + "-secrets"
}

// Binding is the Secret holding one workload's credentials for one service,
// derived from the service's shared credentials with the binding's own logical
// namespace applied. Used when a binding pins a database or vhost of its own.
func Binding(service string, kind Kind, workload string) string {
	return service + "-" + string(kind) + "-" + workload + "-credentials"
}

// ServiceCredentials is the Secret holding a service's shared credentials. It
// belongs to the Service CR rather than to any workload, so no kind applies:
// every binding without its own logical namespace reads this one directly.
func ServiceCredentials(service string) string {
	return service + "-credentials"
}

// GitCredential is the Secret holding the token an App clones its source with,
// named by a digest of the token and the host it is for.
//
// One generation per write, like EnvGeneration and for the same reason: the
// token and the URL are a pair, and they used to be written as a value into one
// fixed name while the URL went to the CR. Two writes could then interleave
// into a pair nobody asked for, and undoing a half-written change meant
// restoring a value rather than dropping an object. Naming the pair makes the
// App's own update the commit point, so a failed write leaves an object nothing
// references instead of a credential that has to be put back.
//
// The digest also makes a repeated write idempotent: the same token for the
// same host is the same object, so rotating twice does not churn.
func GitCredential(app, digest string) string {
	return GitCredentialPrefix(app) + digest
}

// GitCredentialPrefix is what every generation of one App's git credential
// shares, so a reader can tell the App's own credential from a shared one, and
// a sweep can find the generations the App no longer names.
func GitCredentialPrefix(app string) string {
	return app + "-git-credentials-"
}

// LegacyGitCredential is the single fixed name git credentials used before they
// were written one generation per attempt. Clusters installed before that keep
// referencing it until their next rotation, so every reader still accepts it.
func LegacyGitCredential(app string) string {
	return app + "-git-credentials"
}

// GitCredentialDigest identifies one token-and-host pair. Both go in, because
// the same token used for a different host is a different pair, and a name that
// did not say so would let one be mistaken for the other.
//
// The digest reaches the Secret's name and so `spec.git.credentialsSecret`,
// where anyone who can read the App can see it while the host is public in
// spec.git.url. A token with real entropy is unaffected; one a person chose is
// guessable offline at hashing speed. Keying this, with an HMAC over a secret
// every writer can already read, would close that and keep the convergence the
// name depends on.
func GitCredentialDigest(token, authority string) string {
	sum := sha256.Sum256([]byte(authority + "\x00" + token))
	return hex.EncodeToString(sum[:])[:digestLength]
}

// digestLength is how much of the hash reaches a name. It is named because two
// places depend on it agreeing: the writer that generates a name and the
// classifier that recognises one.
const digestLength = 16

// IsGitCredentialOf reports whether a Secret name is an App's own git
// credential rather than a shared one or a stranger's object.
//
// The name alone decides this, because it is what `spec.git.credentialsSecret`
// carries and what a reader has before it fetches anything. Both the legacy
// fixed name and any generation of it count, so a cluster that has not rotated
// since generations arrived keeps working.
func IsGitCredentialOf(app, secret string) bool {
	if secret == LegacyGitCredential(app) {
		return true
	}
	rest, found := strings.CutPrefix(secret, GitCredentialPrefix(app))
	// A generation carries a digest and nothing else. Without this an app named
	// "web" would claim "web-git-credentials-<anything>", including the
	// credential of an app called "web-git-credentials-x".
	return found && isDigest(rest)
}

func isDigest(s string) bool {
	// Exactly what GitCredentialDigest produces. Any other length is a name this
	// package did not generate, and treating one as a generated credential would
	// hand it the lifetime rules that go with the class.
	if len(s) != digestLength {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// AppSharingServiceCredentialName is the app whose git credential Secret would be
// the same object as this service's credentials, and whether there is one.
//
// The two schemes meet: an App named web stored its token at
// web-git-credentials while it was on the name generated before digests, and a
// Service named web-git stores its credentials at exactly that name. Whichever
// object exists, the other kind reads it, and the reader finds a Secret whose
// keys are not the ones it expects.
//
// Both names are published, so neither scheme can move. What a caller can do is
// refuse to create the second object, which is why this answers a question about
// a name rather than doing anything about it.
func AppSharingServiceCredentialName(service string) (string, bool) {
	app, found := strings.CutSuffix(service, "-git")
	if !found || app == "" {
		return "", false
	}
	// Compared rather than assumed: the suffix arithmetic above is a shortcut,
	// and this is the thing that actually has to be true.
	if ServiceCredentials(service) != LegacyGitCredential(app) {
		return "", false
	}
	return app, true
}

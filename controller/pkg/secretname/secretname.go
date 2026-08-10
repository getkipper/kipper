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

// Package servicecatalog holds the facts about a stateful service type that
// more than one component has to agree on.
//
// Two writers create a service's credentials Secret: the console reconciler
// from its own catalog, and `kip service add` from the CLI's. A workload bound
// to the service cannot tell which one ran, so a fact the two spell differently
// is a fact the bound workload sees at random. That has already happened here —
// the CLI dropped USERNAME and PASSWORD for opensearch while the reconciler
// wrote them for every type — which is the argument for one definition rather
// than a matching pair.
package servicecatalog

// ConditionCredentialsReady is the status condition a service carries while its
// credentials Secret cannot be used, under one of two reasons that no retry
// clears: SecretNotOwned, where the object belongs to something else, and
// DataWithoutCredentials, where there is a volume and no password or username
// for it. Its message names the remedy.
//
// The name lives here because three components read it off the object: the
// reconciler that writes it, the console that shows it, and the CLI. An operator
// who cannot see this condition cannot act on it, so a component spelling the
// name differently is a service that silently reports nothing.
const ConditionCredentialsReady = "CredentialsReady"

// ConditionCleanupComplete is the status condition a service carries while its
// deletion cannot finish. The finalizer holds a deleting service until its
// share links are revoked, everything bound to it is unbound and, where that was
// asked for, its data is destroyed. Any of those can fail in a way no retry
// clears, and the service then sits there deleting for good.
//
// It is written for the same reason as the condition above: the reason lives in
// the controller's log otherwise, which an operator watching a service refuse to
// go has no way to read.
const ConditionCleanupComplete = "CleanupComplete"

// ConditionNameFree is the status condition a service carries when an object of
// its name belongs to something else: a StatefulSet or a cluster address that is
// not Kipper's, or is another owner's. No retry frees a name somebody holds, so
// it is one of the states an operator has to clear, and the service has to be
// called something else.
const ConditionNameFree = "NameFree"

// HasAuth reports which service types Kipper configures with credentials.
// Only those types should receive generated passwords; the remaining configured
// services and unknown types use unauthenticated defaults.
func HasAuth(serviceType string) bool {
	switch serviceType {
	case "postgres", "mysql", "mongodb", "rabbitmq", "minio":
		return true
	}
	return false
}

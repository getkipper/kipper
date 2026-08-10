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

// HasAuth reports whether the server this service type runs asks a connecting
// client for a credential.
//
// Three types answer false, each for its own reason: redis starts with no
// --requirepass, opensearch with DISABLE_SECURITY_PLUGIN=true, and mailhog has
// no authentication in the image at all. A credentials Secret for any of them
// carries HOST and PORT alone.
//
// Writing a password anyway is worse than leaving it out. It reaches every
// bound workload, ${REDIS_PASSWORD} resolves against it, and redis answers AUTH
// with an error when no password is set — so a connection string built from it
// fails, and names the wrong cause when it does.
//
// An unknown type answers false. The catalogs fall back to a plain image with
// no credentials configured, so claiming otherwise would mint a password
// nothing reads.
func HasAuth(serviceType string) bool {
	switch serviceType {
	case "postgres", "mysql", "mongodb", "rabbitmq", "minio":
		return true
	}
	return false
}

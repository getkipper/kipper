package v1alpha1

import "strings"

// DefaultBindingPrefix returns the conventional env var prefix for a service
// type when the user does not set one explicitly. Keep this aligned with the
// service catalog in the ServiceReconciler — the prefix combined with the
// credential keys produces the env var names a binding will inject.
func DefaultBindingPrefix(serviceType string) string {
	switch serviceType {
	case "postgres", "mysql", "mongodb":
		return "DB_"
	case "redis":
		return "REDIS_"
	case "rabbitmq":
		return "AMQP_"
	case "opensearch":
		return "OPENSEARCH_"
	case "minio":
		return "S3_"
	case "mailhog":
		return "MAIL_"
	default:
		return strings.ToUpper(serviceType) + "_"
	}
}

// CredentialKeys returns the keys present in a service's credentials Secret
// for a given service type. The ServiceReconciler creates a Secret with
// HOST/PORT for every service, USERNAME and PASSWORD for any service with
// authentication, and a type-specific logical-namespace key on top: NAME
// (default database) for postgres/mysql/mongodb, VHOST for rabbitmq.
// minio is the exception: as an S3 service it carries ENDPOINT/ACCESS_KEY/
// SECRET_KEY instead of the host/port/user/pass baseline. EnvFrom + prefix
// turns these into the env var names a bound app or function sees.
//
// This is the declared shape, and it is read as a contract rather than as
// documentation: the migration path refuses to carry a service whose Secret is
// missing a key named here, so a type listed with more keys than it has cannot
// be migrated at all.
func CredentialKeys(serviceType string) []string {
	switch serviceType {
	case "postgres", "mysql", "mongodb":
		return []string{"HOST", "PORT", "USERNAME", "PASSWORD", "NAME"}
	case "rabbitmq":
		return []string{"HOST", "PORT", "USERNAME", "PASSWORD", "VHOST"}
	case "minio":
		// S3 service: a single endpoint URL plus an access key / secret
		// key, not the host/port/user/pass baseline. EnvFrom + prefix
		// turns these into e.g. S3_ENDPOINT / S3_ACCESS_KEY /
		// S3_SECRET_KEY — the names S3 clients actually read.
		return []string{"ENDPOINT", "ACCESS_KEY", "SECRET_KEY"}
	case "mailhog", "redis", "opensearch":
		// Servers that start with authentication off, so their
		// credentials Secret carries an address and nothing else.
		// redis runs with no --requirepass, opensearch with
		// DISABLE_SECURITY_PLUGIN=true, and the mailhog image has no
		// authentication at all. See servicecatalog.HasAuth, which both
		// Secret writers derive this from.
		return []string{"HOST", "PORT"}
	default:
		// An unknown type falls back to a plain image with nothing
		// configured, so it has no credentials either.
		return []string{"HOST", "PORT"}
	}
}

// IsSensitiveCredentialKey reports whether a credentials Secret key holds a
// secret value that must be masked in previews and never echoed back.
//
// The rule is an allowlist of the keys that demonstrably carry no secret, so
// anything else is treated as one. Naming the credentials instead reads more
// directly and is wrong in the direction that matters: a key this does not
// recognise is shown in full. The Secret is Service-owned rather than
// immutable, and editing its data invalidates nothing, so an operator adding
// API_TOKEN to it would have had that token echoed back by every preview that
// asks this question.
//
// Kipper writes HOST, PORT, USERNAME, ENDPOINT and ACCESS_KEY as addresses and
// identities, NAME and VHOST as the logical namespace a binding chose, and
// management as a URL with no credential in it. Everything else is a secret
// until it is listed here.
func IsSensitiveCredentialKey(key string) bool {
	switch key {
	case "HOST", "PORT", "USERNAME", "NAME", "VHOST", "ENDPOINT", "ACCESS_KEY", "management":
		return false
	}
	return true
}

// HasBrowseableUI reports whether a service type ships a web UI the
// service reconciler should expose via an Ingress + forwardAuth.
// Kept here next to the other service-type metadata so handlers can
// answer "does this service have a UI URL?" without importing the
// controllers package.
//
// CredentialDefaults returns the type-specific keys (and their default
// values) that the credentials Secret carries on top of the
// HOST/PORT/USERNAME/PASSWORD baseline. Used by the controller both
// when creating a fresh Secret and when reconciling an existing one
// into the current shape.
func CredentialDefaults(serviceType string) map[string]string {
	switch serviceType {
	case "postgres", "mysql", "mongodb":
		return map[string]string{"NAME": "app"}
	case "rabbitmq":
		return map[string]string{"VHOST": "/"}
	}
	return nil
}

// HasLogicalNamespace reports whether a service type can carve out a named
// space of its own per binding — a database for postgres, mysql and mongodb, a
// vhost for rabbitmq. Types without one (redis, minio, opensearch) always share
// the service's credentials, so a binding on them never derives a Secret.
func HasLogicalNamespace(serviceType string) bool {
	switch serviceType {
	case "postgres", "mysql", "mongodb", "rabbitmq":
		return true
	}
	return false
}

// LogicalNamespaceKey returns the credentials key carrying a binding's logical
// namespace. The derived per-binding Secret overrides this one key and inherits
// every other value from the service's shared credentials, so the bound
// container resolves its own database while sharing the service's password.
func LogicalNamespaceKey(serviceType string) string {
	if serviceType == "rabbitmq" {
		return "VHOST"
	}
	return "NAME"
}

// InjectedEnvNames returns the env var names a binding will inject for a
// service of the given type, using the supplied prefix. An empty prefix
// falls back to DefaultBindingPrefix. This is the canonical answer to
// "what will I see in process.env when I bind this service?".
func InjectedEnvNames(serviceType, prefix string) []string {
	if prefix == "" {
		prefix = DefaultBindingPrefix(serviceType)
	}
	keys := CredentialKeys(serviceType)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = prefix + k
	}
	return out
}

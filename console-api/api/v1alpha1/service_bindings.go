package v1alpha1

import "strings"

// DefaultBindingPrefix returns the default environment variable prefix for a
// service type. Unknown types use the uppercase type name followed by an underscore.
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

// CredentialKeys returns the expected keys in a service's credentials Secret,
// including connection details and logical namespace keys. Unknown types use
// HOST and PORT. The returned slice is independent of subsequent calls.
func CredentialKeys(serviceType string) []string {
	switch serviceType {
	case "postgres", "mysql", "mongodb":
		return []string{"HOST", "PORT", "USERNAME", "PASSWORD", "NAME"}
	case "rabbitmq":
		return []string{"HOST", "PORT", "USERNAME", "PASSWORD", "VHOST"}
	case "minio":
		// S3 clients use an endpoint URL and access keys.
		return []string{"ENDPOINT", "ACCESS_KEY", "SECRET_KEY"}
	case "mailhog", "redis", "opensearch":
		// These services run with authentication disabled by default.
		return []string{"HOST", "PORT"}
	default:
		// An unknown type falls back to a plain image with nothing
		// configured, so it has no credentials either.
		return []string{"HOST", "PORT"}
	}
}

// IsSensitiveCredentialKey reports whether a credentials Secret key must be
// masked in previews. Only known address, identity, and namespace keys are
// exposed. Unknown keys are masked so credentials added by an operator stay private.
func IsSensitiveCredentialKey(key string) bool {
	switch key {
	case "HOST", "PORT", "USERNAME", "NAME", "VHOST", "ENDPOINT", "ACCESS_KEY", "management":
		return false
	}
	return true
}

// CredentialDefaults returns default logical namespace values for a service's
// credentials Secret. It returns nil for types without a logical namespace.
func CredentialDefaults(serviceType string) map[string]string {
	switch serviceType {
	case "postgres", "mysql", "mongodb":
		return map[string]string{"NAME": "app"}
	case "rabbitmq":
		return map[string]string{"VHOST": "/"}
	}
	return nil
}

// HasLogicalNamespace reports whether bindings can select a database or vhost
// within a service. Other types share the service's credentials directly.
func HasLogicalNamespace(serviceType string) bool {
	switch serviceType {
	case "postgres", "mysql", "mongodb", "rabbitmq":
		return true
	}
	return false
}

// LogicalNamespaceKey returns VHOST for rabbitmq and NAME for every other type.
// Callers should first check HasLogicalNamespace to ensure the key applies.
func LogicalNamespaceKey(serviceType string) string {
	if serviceType == "rabbitmq" {
		return "VHOST"
	}
	return "NAME"
}

// InjectedEnvNames returns the expected environment variable names for a binding
// in CredentialKeys order. An empty prefix uses DefaultBindingPrefix.
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

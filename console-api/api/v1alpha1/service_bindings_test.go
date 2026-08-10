package v1alpha1

import (
	"reflect"
	"testing"
)

func TestDefaultBindingPrefix(t *testing.T) {
	cases := map[string]string{
		"postgres":   "DB_",
		"mysql":      "DB_",
		"mongodb":    "DB_",
		"redis":      "REDIS_",
		"rabbitmq":   "AMQP_",
		"opensearch": "OPENSEARCH_",
		"minio":      "S3_",
		"mailhog":    "MAIL_",
		"kafka":      "KAFKA_", // unknown type → uppercased + _
	}
	for in, want := range cases {
		if got := DefaultBindingPrefix(in); got != want {
			t.Errorf("DefaultBindingPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInjectedEnvNames(t *testing.T) {
	t.Run("postgres with default prefix", func(t *testing.T) {
		got := InjectedEnvNames("postgres", "")
		want := []string{"DB_HOST", "DB_PORT", "DB_USERNAME", "DB_PASSWORD", "DB_NAME"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// redis starts with no --requirepass, so the binding carries an address and
	// nothing else. It used to advertise a generated CACHE_PASSWORD that the
	// server refuses, and a client sending it fails to connect.
	t.Run("redis injects an address alone", func(t *testing.T) {
		got := InjectedEnvNames("redis", "CACHE_")
		want := []string{"CACHE_HOST", "CACHE_PORT"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// opensearch runs with DISABLE_SECURITY_PLUGIN=true.
	t.Run("opensearch injects an address alone", func(t *testing.T) {
		got := InjectedEnvNames("opensearch", "")
		want := []string{"OPENSEARCH_HOST", "OPENSEARCH_PORT"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("rabbitmq injects VHOST not NAME", func(t *testing.T) {
		got := InjectedEnvNames("rabbitmq", "")
		want := []string{"AMQP_HOST", "AMQP_PORT", "AMQP_USERNAME", "AMQP_PASSWORD", "AMQP_VHOST"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// The docs have always said a MailHog binding injects host and port with no
	// auth; the code disagreed and this test pinned the code.
	t.Run("mailhog injects an address alone", func(t *testing.T) {
		got := InjectedEnvNames("mailhog", "")
		want := []string{"MAIL_HOST", "MAIL_PORT"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("minio injects S3 endpoint + access key + secret key", func(t *testing.T) {
		got := InjectedEnvNames("minio", "")
		want := []string{"S3_ENDPOINT", "S3_ACCESS_KEY", "S3_SECRET_KEY"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// An unknown type runs a plain image with nothing configured, so claiming a
	// password for it would be claiming one nothing generated.
	t.Run("unknown type falls through to an address alone", func(t *testing.T) {
		got := InjectedEnvNames("kafka", "")
		want := []string{"KAFKA_HOST", "KAFKA_PORT"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

func TestCredentialDefaults(t *testing.T) {
	cases := map[string]map[string]string{
		"postgres": {"NAME": "app"},
		"mysql":    {"NAME": "app"},
		"mongodb":  {"NAME": "app"},
		"rabbitmq": {"VHOST": "/"},
		"minio":    nil,
		"redis":    nil,
		"kafka":    nil,
	}
	for svcType, want := range cases {
		if got := CredentialDefaults(svcType); !reflect.DeepEqual(got, want) {
			t.Errorf("CredentialDefaults(%q) = %v, want %v", svcType, got, want)
		}
	}
}

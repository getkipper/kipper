package migrationjob

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// PostgresSpec configures the postgres dump+restore job. Source and
// target are identified by the credentials secrets in their respective
// namespaces; the secret must contain HOST/PORT/USERNAME/PASSWORD/NAME
// keys (the shape Kipper's service reconciler produces).
type PostgresSpec struct {
	// TargetNamespace is where the Job runs. The target service must
	// already exist there.
	TargetNamespace string

	// TargetService is the name of the destination service CR. Used as
	// the EnvFrom secret name (`{service}-credentials`) and for Job
	// labelling.
	TargetService string

	// SourceNamespace is the source environment's namespace, e.g.
	// "demo-test". The source service is assumed to share TargetService's
	// name (env-copy preserves service names).
	SourceNamespace string

	// Image is the postgres image to use as the migration client. Should
	// match or exceed both source and target server versions; the safest
	// pick is the target service's own image so the dump format is
	// guaranteed compatible.
	Image string
}

// BuildPostgres constructs a Spec for a postgres dump+restore Job. The
// command pipes pg_dump from source to psql in target with --clean
// --if-exists so re-runs overwrite the previous restore cleanly.
func BuildPostgres(p PostgresSpec) Spec {
	sourceMirror := fmt.Sprintf("%s-from-%s-credentials", p.TargetService, sanitiseNamespace(p.SourceNamespace))
	targetSecret := secretname.ServiceCredentials(p.TargetService)

	// Single sh -c so we get a clean pipe between the two commands and a
	// single non-zero exit if either side fails. We surface the exit
	// status of pg_dump (left side of the pipe) via `set -o pipefail`,
	// which alpine sh supports.
	command := []string{
		"sh", "-c",
		`set -e
set -o pipefail
echo "==> dumping ${SRC_USERNAME}@${SRC_HOST}:${SRC_PORT}/${SRC_NAME}"
echo "==> restoring into ${DST_USERNAME}@${DST_HOST}:${DST_PORT}/${DST_NAME}"
PGPASSWORD="${SRC_PASSWORD}" pg_dump \
    -h "${SRC_HOST}" -p "${SRC_PORT}" -U "${SRC_USERNAME}" -d "${SRC_NAME}" \
    --clean --if-exists --no-owner --no-privileges \
| PGPASSWORD="${DST_PASSWORD}" psql \
    -h "${DST_HOST}" -p "${DST_PORT}" -U "${DST_USERNAME}" -d "${DST_NAME}" \
    --set ON_ERROR_STOP=1 -v ON_ERROR_STOP=1
echo "==> migration complete"
`,
	}

	return Spec{
		Namespace: p.TargetNamespace,
		JobName:   SuggestJobName("migrate-" + p.TargetService),
		Image:     p.Image,
		Command:   command,
		EnvFrom: []corev1.EnvFromSource{
			{
				Prefix: "SRC_",
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: sourceMirror},
				},
			},
			{
				Prefix: "DST_",
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: targetSecret},
				},
			},
		},
		Mirrors: []MirrorSpec{
			{
				SourceNamespace: p.SourceNamespace,
				SourceName:      secretname.ServiceCredentials(p.TargetService),
				TargetName:      sourceMirror,
			},
		},
		Labels: map[string]string{
			"kipper.run/service":      p.TargetService,
			"kipper.run/service-type": "postgres",
		},
	}
}

// sanitiseNamespace turns a namespace string into something safe to use
// inside a secret name (DNS-1123 subdomain rules are already met by any
// valid namespace name; this is just defensive shortening).
func sanitiseNamespace(ns string) string {
	if len(ns) > 40 {
		return ns[:40]
	}
	return ns
}

package migration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// migrateServices exports Service CRs and database data from the source
// cluster and transfers them to the target.
func (h *Handler) migrateServices(ctx context.Context, session *Session, token *Token, namespace string) error {
	var serviceList kipperv1.ServiceList
	if err := h.CRClient.List(ctx, &serviceList, crclient.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing services: %w", err)
	}

	if len(serviceList.Items) == 0 {
		return nil
	}

	for _, svc := range serviceList.Items {
		if session.IsCancelled() {
			return fmt.Errorf("migration cancelled")
		}

		// Step: Create Service CR on target
		stepName := fmt.Sprintf("Creating service %s/%s on target", namespace, svc.Name)
		session.AddStep(Step{
			Name:   stepName,
			Phase:  "data",
			Status: StepRunning,
		})

		specJSON, _ := json.Marshal(svc.Spec)
		var specMap map[string]interface{}
		_ = json.Unmarshal(specJSON, &specMap)

		// The credentials travel with the CR rather than in the bulk Secret
		// phase, so the target can own them from the moment they land and its
		// engine initialises against the password this cluster's data was
		// written with. A service whose credentials are missing here cannot be
		// migrated at all: the target would mint its own and the restored data
		// would not know it.
		credentials, err := h.serviceCredentialsPayload(ctx, namespace, svc.Name, svc.Spec.Type)
		if err != nil {
			session.UpdateStep(stepName, func(s *Step) {
				s.Status = StepFailed
				s.Error = err.Error()
			})
			return err
		}

		if err := h.sendToTarget(token, fmt.Sprintf("/api/v1/migrate-target/%s/resource", session.ID), map[string]interface{}{
			"kind":        "Service",
			"name":        svc.Name,
			"namespace":   namespace,
			"spec":        specMap,
			"credentials": credentials,
		}); err != nil {
			session.UpdateStep(stepName, func(s *Step) {
				s.Status = StepFailed
				s.Error = err.Error()
			})
			return fmt.Errorf("creating service %s on target: %w", svc.Name, err)
		}

		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepCompleted
			now := time.Now()
			s.CompletedAt = &now
		})

		// Wait for the StatefulSet to be ready on the target
		stepName = fmt.Sprintf("Waiting for %s to be ready on target", svc.Name)
		session.AddStep(Step{
			Name:   stepName,
			Phase:  "data",
			Status: StepRunning,
		})

		// A fresh box pulls the service image and provisions storage before
		// the StatefulSet turns ready, so the budget is generous — and a
		// timeout fails the run here with the real cause instead of letting
		// the data import die on a pod that never came up.
		svcReady := false
		svcErr := fmt.Errorf("statefulsets in %s did not become ready", namespace)
		for attempt := 0; attempt < 150; attempt++ {
			resp, err := h.callTarget(token, "GET", fmt.Sprintf("/api/v1/migrate-target/%s/status?namespace=%s", session.ID, namespace), nil)
			if err != nil {
				svcErr = err
			} else if ready, ok := resp["statefulsets_ready"].(bool); ok && ready {
				svcReady = true
				break
			}
			time.Sleep(2 * time.Second)
		}
		if !svcReady {
			session.UpdateStep(stepName, func(s *Step) {
				s.Status = StepFailed
				s.Error = svcErr.Error()
			})
			return fmt.Errorf("waiting for %s on target: %w", svc.Name, svcErr)
		}

		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepCompleted
			now := time.Now()
			s.CompletedAt = &now
		})

		// Export and import data for stateful services. Engines without a
		// dump path move their PVC bytes through the chunked transfer with
		// the statefulset stopped on both clusters.
		if hasExportableData(svc.Spec.Type) {
			if err := h.migrateDatabaseData(ctx, session, token, namespace, &svc); err != nil {
				return err
			}
		} else if needsManualDataTransfer(svc.Spec.Type) {
			if err := h.transferServicePVCData(ctx, session, token, namespace, &svc); err != nil {
				return err
			}
		}
	}

	return nil
}

// serviceCredentialsPayload reads a service's shared credentials for the
// handover that carries its Service CR. Values are base64 encoded, as they are
// on the bulk Secret endpoint, so a non-UTF-8 byte survives the JSON hop.
func (h *Handler) serviceCredentialsPayload(ctx context.Context, namespace, service, serviceType string) (*transferredCredentials, error) {
	secret, err := h.Client.CoreV1().Secrets(namespace).Get(ctx, secretname.ServiceCredentials(service), metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the credentials of %s: %w", service, err)
	}
	// A partial credential is worse than none: it would replace the target's
	// complete set on a replay, and stand a new engine up with no password at
	// all on a fresh install. A service whose credentials are missing keys its
	// own pod reads by name is already broken here, so this refuses to carry
	// the breakage rather than discovering it on the far side.
	var missing []string
	for _, key := range kipperv1.CredentialKeys(serviceType) {
		if len(secret.Data[key]) == 0 {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the credentials of %s are missing %s, so the target could not run it: repair the secret on this cluster first",
			service, strings.Join(missing, ", "))
	}
	data := make(map[string]string, len(secret.Data))
	for k, v := range secret.Data {
		data[k] = base64.StdEncoding.EncodeToString(v)
	}
	return &transferredCredentials{Labels: secret.Labels, Data: data}, nil
}

const maxAutoTransferBytes = 500 * 1024 * 1024 // 500MB

func (h *Handler) migrateDatabaseData(ctx context.Context, session *Session, token *Token, namespace string, svc *kipperv1.Service) error {
	stepName := fmt.Sprintf("Exporting %s database (%s)", svc.Spec.Type, svc.Name)
	session.AddStep(Step{
		Name:   stepName,
		Phase:  "data",
		Status: StepRunning,
	})

	// Estimate database size before dumping
	estimatedSize, sizeErr := h.estimateDatabaseSize(ctx, namespace, svc.Name, svc.Spec.Type)
	if sizeErr == nil && estimatedSize > maxAutoTransferBytes {
		sizeMB := estimatedSize / (1024 * 1024)
		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepSkipped
			s.Detail = fmt.Sprintf("Database is ~%d MB: too large for automatic transfer", sizeMB)
			s.ManualSteps = buildManualMigrationSteps(svc.Spec.Type, svc.Name, namespace)
		})
		return nil
	}

	// Get credentials for dump command
	creds, err := h.Client.CoreV1().Secrets(namespace).Get(ctx, secretname.ServiceCredentials(svc.Name), metav1.GetOptions{})
	if err != nil {
		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepFailed
			s.Error = "credentials not found"
		})
		return fmt.Errorf("getting credentials for %s: %w", svc.Name, err)
	}

	username := string(creds.Data["USERNAME"])

	// Postgres moves database by database: a pg_dumpall stream replayed
	// into a single database restores roles and cross-database content
	// wrongly, and a failed \connect silently dumps the remainder into
	// whatever database was connected last.
	if svc.Spec.Type == "postgres" {
		return h.migratePostgres(ctx, session, token, namespace, svc, username, stepName)
	}

	// Build dump command
	dumpCmd, containerName := buildDumpCommand(svc.Spec.Type, svc.Name)
	if dumpCmd == nil {
		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepSkipped
			s.Detail = fmt.Sprintf("dump not supported for %s", svc.Spec.Type)
		})
		return nil
	}

	podName, err := h.servicePod(ctx, namespace, svc.Name)
	if err != nil {
		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepFailed
			s.Error = "no running pod found"
		})
		return err
	}

	importStep := fmt.Sprintf("Importing %s database on target (%s)", svc.Spec.Type, svc.Name)
	session.AddStep(Step{
		Name:   importStep,
		Phase:  "data",
		Status: StepRunning,
	})

	// The dump streams from the source exec straight into the target's
	// import exec; export and import run as one transfer.
	sent, err := h.streamExecToTarget(ctx, token,
		fmt.Sprintf("/api/v1/migrate-target/%s/db-import", session.ID),
		url.Values{
			"service":   {svc.Name},
			"namespace": {namespace},
			"type":      {svc.Spec.Type},
		},
		namespace, podName, containerName, dumpCmd)
	if err != nil {
		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepFailed
			s.Error = fmt.Sprintf("dump failed: %v", err)
		})
		session.UpdateStep(importStep, func(s *Step) {
			s.Status = StepFailed
			s.Error = err.Error()
		})
		return fmt.Errorf("transferring %s database: %w", svc.Name, err)
	}

	now := time.Now()
	session.UpdateStep(stepName, func(s *Step) {
		s.Status = StepCompleted
		s.BytesDone = sent
		s.Detail = fmt.Sprintf("%s exported (%d bytes)", svc.Spec.Type, sent)
		s.CompletedAt = &now
	})
	session.UpdateStep(importStep, func(s *Step) {
		s.Status = StepCompleted
		s.BytesDone = sent
		s.CompletedAt = &now
	})

	return nil
}

// maxDBImportBytes caps a streamed dump on the receiving side. The source
// refuses databases whose estimated size exceeds maxAutoTransferBytes, so
// this only needs headroom for estimates that undershoot the actual dump.
const maxDBImportBytes = 768 * 1024 * 1024

// ReceiveDBImportHandler receives a streamed database dump and pipes it into
// the target cluster's service pod. Metadata arrives in query parameters and
// the request body is the raw dump.
// POST /api/v1/migrate-target/{session}/db-import?service=&namespace=&type=[&database=&tables=]
func (h *Handler) ReceiveDBImportHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	service := q.Get("service")
	namespace := q.Get("namespace")
	dbType := q.Get("type")
	if service == "" || namespace == "" || dbType == "" {
		respondError(w, http.StatusBadRequest, "service, namespace and type query parameters are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dataTransferTimeout)
	defer cancel()

	// The scope check runs before the body is touched, so an out-of-scope
	// caller cannot make this cluster ingest a stream at all.
	if !h.namespaceInScope(ctx, chi.URLParam(r, "session"), namespace) {
		respondError(w, http.StatusForbidden, "target project is outside this migration's accepted scope")
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxDBImportBytes)

	if dbType == "postgres" {
		database := q.Get("database")
		tables, err := strconv.ParseInt(q.Get("tables"), 10, 64)
		if err != nil {
			respondError(w, http.StatusBadRequest, "tables query parameter must be the source table count")
			return
		}
		if err := h.importPostgresDatabase(ctx, namespace, service, database, tables, body); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("import failed: %v", err))
			return
		}
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":   "imported",
			"database": database,
		})
		return
	}

	importCmd, containerName := buildImportCommand(dbType, service)
	if importCmd == nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("import not supported for %s", dbType))
		return
	}

	podName, err := h.servicePod(ctx, namespace, service)
	if err != nil {
		respondError(w, http.StatusServiceUnavailable, fmt.Sprintf("no running pod for %s", service))
		return
	}

	// Same first-time-init race as postgres: the MySQL image runs a
	// bootstrap server with --skip-networking during init, so wait for the
	// real server on TCP before feeding it the dump. mysqladmin ping over
	// 127.0.0.1 reports the server reachable without needing valid auth.
	if dbType == "mysql" {
		if err := h.waitDBServerReady(ctx, namespace, podName, service, "mysql",
			[]string{"mysqladmin", "ping", "-h", "127.0.0.1", "--silent"}); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("waiting for %s: %v", service, err))
			return
		}
	}

	if err := h.execInPodTo(ctx, namespace, podName, containerName, importCmd, body, io.Discard); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("import failed: %v", err))
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "imported"})
}

// buildDumpCommand covers the service types whose dump restores symmetric
// through a single stream. Postgres is handled by migratePostgres, which
// moves database by database.
func buildDumpCommand(svcType, serviceName string) ([]string, string) {
	switch svcType {
	case "mysql":
		// Routines and events are off by default in mysqldump and would
		// silently stay behind; triggers is explicit for the same reason.
		return []string{"sh", "-c", "mysqldump -u root -p\"$MYSQL_ROOT_PASSWORD\" --all-databases --single-transaction --routines --events --triggers"}, serviceName
	case "mongodb":
		// The catalog sets MONGO_INITDB_ROOT_USERNAME and _PASSWORD, and the
		// official image starts mongod with --auth when both are present, so an
		// unauthenticated dump is refused. The localhost exception does not
		// apply once a root user exists. Both variables are already in the pod's
		// environment, so the credentials never appear in the command line.
		return []string{"sh", "-c", "mongodump --archive --quiet " +
			`-u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin`}, serviceName
	case "redis":
		// SAVE blocks until the RDB is on disk, so the copied file is the
		// current dataset. BGSAVE returns before the write finishes, and a
		// fixed sleep shipped whatever snapshot happened to be there.
		return []string{"sh", "-c", "redis-cli SAVE >/dev/null && cat /data/dump.rdb"}, serviceName
	case "rabbitmq":
		return []string{"sh", "-c", "rabbitmqctl export_definitions -"}, serviceName
	default:
		return nil, ""
	}
}

func buildImportCommand(svcType, serviceName string) ([]string, string) {
	switch svcType {
	case "mysql":
		return []string{"sh", "-c", "mysql -u root -p\"$MYSQL_ROOT_PASSWORD\""}, serviceName
	case "mongodb":
		return []string{"sh", "-c", "mongorestore --archive --quiet " +
			`-u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin`}, serviceName
	case "redis":
		// DEBUG RELOAD without NOSAVE first saves the (empty) in-memory
		// dataset over the file it is about to load, silently discarding the
		// transfer. NOSAVE loads the written snapshot as-is.
		return []string{"sh", "-c", "cat > /data/dump.rdb && redis-cli DEBUG RELOAD NOSAVE"}, serviceName
	case "rabbitmq":
		// import_definitions takes a path and nothing else: given "-" it looks
		// for a file of that name and fails with "File - does not exist", so the
		// stream is written to disk first. Verified against
		// rabbitmq:3-management-alpine (3.13.7), the tag the catalog pins.
		//
		// The file stays. rabbitmqctl reports the import as asynchronous, so
		// removing it straight afterwards would race whatever is still reading
		// it, and the definitions hold the same password hashes the node's own
		// database in that container already holds.
		return []string{"sh", "-c", "cat > /tmp/definitions.json && rabbitmqctl import_definitions /tmp/definitions.json"}, serviceName
	default:
		return nil, ""
	}
}

// estimateDatabaseSize checks the data directory size in the service pod.
func (h *Handler) estimateDatabaseSize(ctx context.Context, namespace, serviceName, svcType string) (int64, error) {
	pods, err := h.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", serviceName),
	})
	if err != nil || len(pods.Items) == 0 {
		return 0, fmt.Errorf("no pod found")
	}

	dataPath := "/var/lib/postgresql/data"
	switch svcType {
	case "mysql":
		dataPath = "/var/lib/mysql"
	case "mongodb":
		dataPath = "/data/db"
	case "redis":
		dataPath = "/data"
	case "rabbitmq":
		dataPath = "/var/lib/rabbitmq"
	}

	cmd := []string{"sh", "-c", fmt.Sprintf("du -sb %s 2>/dev/null | cut -f1", dataPath)}
	req := h.Client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pods.Items[0].Name).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: serviceName,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(h.RESTConfig, "POST", req.URL())
	if err != nil {
		return 0, err
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return 0, err
	}

	var size int64
	if _, err := fmt.Sscanf(strings.TrimSpace(stdout.String()), "%d", &size); err != nil {
		return 0, err
	}
	return size, nil
}

func buildManualMigrationSteps(svcType, serviceName, namespace string) []string {
	// Extract project from namespace for --project flag
	project := namespace

	switch svcType {
	case "postgres":
		return []string{
			"# Export each database from the source cluster (list them with \\l):",
			fmt.Sprintf("kip exec %s --project %s -- pg_dump -U kipper --clean --if-exists --no-owner --no-privileges app > %s-app.sql", serviceName, project, serviceName),
			"# Copy to target server:",
			fmt.Sprintf("scp %s-app.sql root@<target-server>:/tmp/", serviceName),
			"# Import on target cluster (repeat per database):",
			fmt.Sprintf("kip exec %s --project %s -- psql -U kipper -d app -v ON_ERROR_STOP=1 < /tmp/%s-app.sql", serviceName, project, serviceName),
		}
	case "mysql":
		return []string{
			"# Export from source cluster:",
			fmt.Sprintf("kip exec %s --project %s -- mysqldump -u root --all-databases --single-transaction > %s-dump.sql", serviceName, project, serviceName),
			"# Copy to target server:",
			fmt.Sprintf("scp %s-dump.sql root@<target-server>:/tmp/", serviceName),
			"# Import on target cluster:",
			fmt.Sprintf("kip exec %s --project %s -- mysql -u root < /tmp/%s-dump.sql", serviceName, project, serviceName),
		}
	case "mongodb":
		// The credentials are read inside the pod, so the command runs under
		// sh -c in single quotes: unquoted, the operator's own shell would
		// expand the variables to nothing before kip ever saw them.
		return []string{
			"# Export from source cluster:",
			fmt.Sprintf(`kip exec %s --project %s -- sh -c 'mongodump --archive -u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin' > %s-dump.archive`, serviceName, project, serviceName),
			"# Copy to target server:",
			fmt.Sprintf("scp %s-dump.archive root@<target-server>:/tmp/", serviceName),
			"# Import on target cluster:",
			fmt.Sprintf(`kip exec %s --project %s -- sh -c 'mongorestore --archive -u "$MONGO_INITDB_ROOT_USERNAME" -p "$MONGO_INITDB_ROOT_PASSWORD" --authenticationDatabase admin' < /tmp/%s-dump.archive`, serviceName, project, serviceName),
		}
	default:
		return []string{
			fmt.Sprintf("# Manual data transfer required for %s service %q", svcType, serviceName),
		}
	}
}

func hasExportableData(svcType string) bool {
	switch svcType {
	case "postgres", "mysql", "mongodb", "redis", "rabbitmq":
		return true
	}
	return false
}

func needsManualDataTransfer(svcType string) bool {
	switch svcType {
	case "opensearch", "minio":
		return true
	}
	return false
}

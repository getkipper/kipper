package migration

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/mongotool"
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
			return fmt.Errorf("migration canceled")
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

		// Sending credentials with the CR lets the target initialize the engine
		// with the source password before restoring its data.
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
		session.MarkDataLeftBehind(namespace, svc.Name)
		// Fetch manual steps before UpdateStep acquires the session lock so
		// cancellation can proceed while these requests run.
		manual := h.manualMigrationSteps(ctx, namespace, svc)
		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepSkipped
			s.Detail = fmt.Sprintf("Database is ~%d MB: too large for automatic transfer", sizeMB)
			s.ManualSteps = manual
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

// mysqlClient prefixes a mysql or mysqldump invocation with the root password
// in its environment, which keeps it out of the process's arguments.
const mysqlClient = `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" `

// buildDumpCommand returns a command and container for services whose dumps can
// be restored from a single stream. Unsupported types return nil and an empty
// container name. migratePostgres handles Postgres one database at a time.
func buildDumpCommand(svcType, serviceName string) ([]string, string) {
	switch svcType {
	case "mysql":
		// Routines and events are off by default in mysqldump and would
		// silently stay behind. Triggers are explicit to ensure they are included too.
		return []string{"sh", "-c", mysqlClient + "mysqldump -u root --all-databases --single-transaction --routines --events --triggers"}, serviceName
	case "mongodb":
		// The catalog sets MONGO_INITDB_ROOT_USERNAME and _PASSWORD, and the
		// official image starts mongod with --auth when both are present, so an
		// unauthenticated dump is refused. The localhost exception does not
		// apply once a root user exists.
		return []string{"sh", "-c", mongotool.Script("mongodump --archive --quiet")}, serviceName
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
		return []string{"sh", "-c", mysqlClient + "mysql -u root"}, serviceName
	case "mongodb":
		return []string{"sh", "-c", mongotool.Script("mongorestore --archive --quiet")}, serviceName
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

// estimateDatabaseSize estimates the service's data size in bytes from inside
// its pod. The estimate determines eligibility for automatic transfer and may
// differ from the dump size.
func (h *Handler) estimateDatabaseSize(ctx context.Context, namespace, serviceName, svcType string) (int64, error) {
	pod, err := h.servicePod(ctx, namespace, serviceName)
	if err != nil {
		return 0, err
	}
	return estimateSize(svcType, func(cmd []string) (string, error) {
		return h.execInPod(ctx, namespace, pod, serviceName, cmd, nil)
	})
}

// estimateSize asks the engine for the size of the data it holds, and falls
// back to the size of the data directory when it has no answer. The directory
// also holds binary logs, redo logs and WAL, which no dump carries.
func estimateSize(svcType string, run func(cmd []string) (string, error)) (int64, error) {
	if cmd := logicalSizeCommand(svcType); cmd != nil {
		if out, err := run(cmd); err == nil {
			if size, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil {
				return size, nil
			}
		}
	}
	out, err := run([]string{"sh", "-c", fmt.Sprintf("du -sb %s 2>/dev/null | cut -f1", dataDirectory(svcType))})
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

// logicalSizeCommand returns a query for MySQL table data bytes or Postgres
// non-template database bytes, or nil for an unsupported engine. MySQL reads
// current statistics instead of the ones it caches for up to a day.
func logicalSizeCommand(svcType string) []string {
	switch svcType {
	case "mysql":
		return []string{"sh", "-c", mysqlClient + `mysql -u root -N -B -e "SET SESSION information_schema_stats_expiry = 0; SELECT COALESCE(SUM(data_length), 0) FROM information_schema.tables WHERE table_schema NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')"`}
	case "postgres":
		return []string{"sh", "-c", `psql -U "$POSTGRES_USER" -d postgres -At -c "SELECT COALESCE(SUM(pg_database_size(datname)), 0) FROM pg_database WHERE NOT datistemplate"`}
	default:
		return nil
	}
}

func dataDirectory(svcType string) string {
	switch svcType {
	case "mysql":
		return "/var/lib/mysql"
	case "mongodb":
		return "/data/db"
	case "redis":
		return "/data"
	case "rabbitmq":
		return "/var/lib/rabbitmq"
	default:
		return "/var/lib/postgresql/data"
	}
}

// plainDatabaseName is the database name kip service export and import accept.
var plainDatabaseName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// manualMigrationSteps lists the service's databases for buildManualMigrationSteps.
// If the list is unavailable, Postgres and MySQL steps use the service's default
// database, while MongoDB steps transfer the whole archive.
func (h *Handler) manualMigrationSteps(ctx context.Context, namespace string, svc *kipperv1.Service) []string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var databases []string
	if pod, err := h.servicePod(ctx, namespace, svc.Name); err == nil {
		databases, _ = listDatabases(svc.Spec.Type, func(cmd []string) (string, error) {
			return h.execInPod(ctx, namespace, pod, svc.Name, cmd, nil)
		})
	}
	return buildManualMigrationSteps(svc.Spec.Type, svc.Name, namespace, databases)
}

// listDatabases lists the user databases of a postgres or mysql service.
func listDatabases(svcType string, run func(cmd []string) (string, error)) ([]string, error) {
	var cmd []string
	switch svcType {
	case "postgres":
		cmd = []string{"sh", "-c", `psql -U "$POSTGRES_USER" -d postgres -At -c "SELECT encode(convert_to(datname, 'UTF8'), 'hex') FROM pg_database WHERE NOT datistemplate"`}
	case "mysql":
		cmd = []string{"sh", "-c", mysqlClient + `mysql -u root -N -B -e "SELECT HEX(schema_name) FROM information_schema.schemata WHERE schema_name NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')"`}
	default:
		return nil, nil
	}
	out, err := run(cmd)
	if err != nil {
		return nil, err
	}
	// Each name comes back hex-encoded, because spaces and line breaks are
	// part of an identifier and plain output would lose or split them.
	var databases []string
	for _, line := range strings.Fields(out) {
		name, err := hex.DecodeString(line)
		if err != nil {
			return nil, fmt.Errorf("reading a database name: %w", err)
		}
		databases = append(databases, string(name))
	}
	return databases, nil
}

// buildManualMigrationSteps returns kip export and import commands to run from
// a machine that can reach both clusters. Postgres and MySQL move one database
// at a time. With no database list, they use the service's default database and
// include instructions to repeat the commands for others. MongoDB moves the
// whole archive. Unsupported engines and unsafe names get manual instructions.
func buildManualMigrationSteps(svcType, serviceName, namespace string, databases []string) []string {
	extension := map[string]string{"postgres": ".dump", "mysql": ".sql.gz", "mongodb": ".archive.gz"}[svcType]
	if extension == "" {
		return []string{fmt.Sprintf("# Manual data transfer required for %s service %q", svcType, serviceName)}
	}
	// Migration preserves namespace names, so use the exact namespace to avoid
	// adding the operator's organization prefix through --project.
	scope := "--namespace " + namespace
	steps := []string{
		"# Run these commands from a machine where kip can reach both clusters. Use the cluster names from 'kip cluster list'.",
	}
	// Require a successful export before importing because export rejects an
	// existing dump file, which could contain data from another transfer.
	transfer := func(databaseFlag, file string) {
		steps = append(steps, fmt.Sprintf(
			"kip service export %[1]s %[2]s%[3]s --file %[4]s --cluster <source-cluster> && "+
				// --drop replaces data from a failed import so the operator can retry.
				"kip service import %[1]s %[2]s%[3]s --drop --file %[4]s --cluster <target-cluster>",
			serviceName, scope, databaseFlag, file))
	}
	// The namespace prefix separates dumps from different namespaces. Dots
	// separate the names because namespaces and accepted database names cannot contain them.
	prefix := namespace + "." + serviceName
	if svcType == "mongodb" {
		transfer("", prefix+extension)
		return steps
	}
	if len(databases) == 0 {
		// Omitting --database selects the database named in the service's credentials.
		transfer("", prefix+extension)
		return append(steps, "# Repeat this line for each additional database in the service. Add --database <name> to both commands and use the same --file path in both, with a different path for each database.")
	}
	for _, db := range databases {
		// The steps are pasted into a shell, and a database name is whatever
		// its creator chose, so only a name kip accepts becomes a command.
		if !plainDatabaseName.MatchString(db) {
			steps = append(steps, fmt.Sprintf("# Move the database %q by hand: kip service export accepts only letters, digits, underscores and hyphens.", db))
			continue
		}
		transfer(" --database "+db, prefix+"."+db+extension)
	}
	return steps
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

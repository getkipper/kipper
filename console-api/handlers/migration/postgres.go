package migration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/go-chi/chi/v5"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// validDBName vets database names read from the source cluster before they
// are spliced into SQL on the target. Kipper creates lowercase names, but the
// gate also covers databases users created by hand.
var validDBName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$-]*$`)

// userTableCountQuery counts the tables a restore is expected to reproduce.
const userTableCountQuery = "SELECT count(*) FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog','information_schema')"

// migratePostgres transfers every database in the service, one at a time:
// pg_dump --clean --if-exists per database, restored on the target into a
// database of the same name with ON_ERROR_STOP, then verified by table
// count. exportStep is the already-running step migrateDatabaseData opened.
func (h *Handler) migratePostgres(ctx context.Context, session *Session, token *Token, namespace string, svc *kipperv1.Service, username, exportStep string) error {
	failExport := func(format string, args ...interface{}) error {
		err := fmt.Errorf(format, args...)
		session.UpdateStep(exportStep, func(s *Step) {
			s.Status = StepFailed
			s.Error = err.Error()
		})
		return err
	}

	pod, err := h.servicePod(ctx, namespace, svc.Name)
	if err != nil {
		return failExport("finding %s pod: %w", svc.Name, err)
	}

	databases, err := h.listPostgresDatabases(ctx, namespace, pod, svc.Name, username)
	if err != nil {
		return failExport("listing databases in %s: %w", svc.Name, err)
	}
	if len(databases) == 0 {
		return failExport("service %s reports no databases; refusing to continue with an empty restore", svc.Name)
	}
	for _, db := range databases {
		if !validDBName.MatchString(db) {
			return failExport("database name %q in %s is not migratable automatically", db, svc.Name)
		}
	}

	importStep := fmt.Sprintf("Importing postgres databases on target (%s)", svc.Name)
	session.AddStep(Step{
		Name:       importStep,
		Phase:      "data",
		Status:     StepRunning,
		BytesTotal: int64(len(databases)),
	})

	var totalBytes int64
	for i, db := range databases {
		if session.IsCancelled() {
			return fmt.Errorf("migration cancelled")
		}

		tables, err := h.countPostgresTables(ctx, namespace, pod, svc.Name, username, db)
		if err != nil {
			return failExport("counting tables in %s/%s: %w", svc.Name, db, err)
		}

		session.UpdateStep(exportStep, func(s *Step) {
			s.Detail = fmt.Sprintf("Dumping %s (%d/%d)", db, i+1, len(databases))
		})
		session.UpdateStep(importStep, func(s *Step) {
			s.Detail = fmt.Sprintf("Restoring %s (%d/%d, %d tables)", db, i+1, len(databases), tables)
		})

		// The dump streams from pg_dump straight into the target's restore:
		// no buffering, no base64, so memory stays flat however large the
		// database is.
		sent, err := h.streamExecToTarget(ctx, token,
			fmt.Sprintf("/api/v1/migrate-target/%s/db-import", session.ID),
			url.Values{
				"service":   {svc.Name},
				"namespace": {namespace},
				"type":      {"postgres"},
				"database":  {db},
				"tables":    {strconv.FormatInt(tables, 10)},
			},
			namespace, pod, svc.Name,
			[]string{"pg_dump", "-U", username, "--clean", "--if-exists", "--no-owner", "--no-privileges", db})
		if err != nil {
			session.UpdateStep(importStep, func(s *Step) {
				s.Status = StepFailed
				s.Error = fmt.Sprintf("restoring %s: %v", db, err)
			})
			return fmt.Errorf("restoring %s/%s on target: %w", svc.Name, db, err)
		}

		totalBytes += sent
		session.UpdateStep(importStep, func(s *Step) {
			s.BytesDone = int64(i + 1)
		})
	}

	// A confirmed overwrite promises replacement, so databases that exist
	// only on the target are dropped; without this the result is a merge
	// that quietly keeps the target's old data.
	pruneResp, err := h.callTarget(token, "POST", fmt.Sprintf("/api/v1/migrate-target/%s/db-prune", session.ID), map[string]interface{}{
		"service":   svc.Name,
		"namespace": namespace,
		"databases": databases,
	})
	if err != nil {
		session.UpdateStep(importStep, func(s *Step) {
			s.Status = StepFailed
			s.Error = fmt.Sprintf("removing target-only databases: %v", err)
		})
		return fmt.Errorf("removing target-only databases in %s: %w", svc.Name, err)
	}
	var droppedNote string
	if dropped, ok := pruneResp["dropped"].([]interface{}); ok && len(dropped) > 0 {
		names := make([]string, 0, len(dropped))
		for _, d := range dropped {
			if s, ok := d.(string); ok {
				names = append(names, s)
			}
		}
		droppedNote = fmt.Sprintf("; dropped target-only databases: %s", strings.Join(names, ", "))
	}

	now := time.Now()
	session.UpdateStep(exportStep, func(s *Step) {
		s.Status = StepCompleted
		s.BytesDone = totalBytes
		s.Detail = fmt.Sprintf("%d databases exported (%s)", len(databases), strings.Join(databases, ", "))
		s.CompletedAt = &now
	})
	session.UpdateStep(importStep, func(s *Step) {
		s.Status = StepCompleted
		s.Detail = fmt.Sprintf("%d databases restored and verified by table count%s", len(databases), droppedNote)
		s.CompletedAt = &now
	})

	return nil
}

// ReceiveDBPruneHandler drops the postgres databases of a service that the
// source does not have, completing overwrite semantics after the per-database
// restores. The "postgres" maintenance database is never dropped, and only
// names the automatic path could have created are considered.
// POST /api/v1/migrate-target/{session}/db-prune
func (h *Handler) ReceiveDBPruneHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Service   string   `json:"service"`
		Namespace string   `json:"namespace"`
		Databases []string `json:"databases"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Service == "" || req.Namespace == "" || len(req.Databases) == 0 {
		respondError(w, http.StatusBadRequest, "service, namespace and a non-empty database list are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	if !h.namespaceInScope(ctx, chi.URLParam(r, "session"), req.Namespace) {
		respondError(w, http.StatusForbidden, "target project is outside this migration's accepted scope")
		return
	}

	keep := make(map[string]bool, len(req.Databases)+1)
	for _, db := range req.Databases {
		keep[db] = true
	}
	// The maintenance database must survive whatever the source reports:
	// dropping it would break every later psql session.
	keep["postgres"] = true

	creds, err := h.Client.CoreV1().Secrets(req.Namespace).Get(ctx, secretname.ServiceCredentials(req.Service), metav1.GetOptions{})
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("credentials for %s not found: %v", req.Service, err))
		return
	}
	username := string(creds.Data["USERNAME"])

	pod, err := h.servicePod(ctx, req.Namespace, req.Service)
	if err != nil {
		respondError(w, http.StatusServiceUnavailable, err.Error())
		return
	}

	existing, err := h.listPostgresDatabases(ctx, req.Namespace, pod, req.Service, username)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("listing databases: %v", err))
		return
	}

	var dropped []string
	for _, db := range existing {
		if keep[db] || !validDBName.MatchString(db) {
			continue
		}
		// FORCE terminates lingering sessions; the migrated apps are not
		// running yet during the data phase, so none belong to a workload.
		stmt := fmt.Sprintf("DROP DATABASE %q WITH (FORCE)", db)
		if _, err := h.execInPod(ctx, req.Namespace, pod, req.Service,
			[]string{"psql", "-U", username, "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-c", stmt}, nil); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("dropping %s: %v", db, err))
			return
		}
		dropped = append(dropped, db)
	}

	// The migrated apps read the transferred credentials Secret, but the
	// target's postgres role keeps whatever password it initialised with
	// (a fresh one if the service existed before the Secret arrived, or on
	// a replay). Force the role password to match the Secret so apps can
	// authenticate. psql authenticates locally via peer/trust, so this needs
	// no knowledge of the current password.
	if err := h.syncPostgresPassword(ctx, req.Namespace, pod, req.Service, username, string(creds.Data["PASSWORD"])); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("aligning role password: %v", err))
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "pruned",
		"dropped": dropped,
	})
}

// syncPostgresPassword sets the service role's password to the value in the
// transferred credentials Secret, so the target database and the Secret the
// apps use never disagree. The password is embedded as a SQL string literal
// with single quotes doubled, the standard escape.
func (h *Handler) syncPostgresPassword(ctx context.Context, namespace, pod, service, username, password string) error {
	if password == "" {
		return nil
	}
	// The role name is a SQL identifier and the password a string literal;
	// each gets its own escaping (doubled double-quotes vs doubled single
	// quotes). Go's %q is not SQL-safe for either.
	roleIdent := `"` + strings.ReplaceAll(username, `"`, `""`) + `"`
	passLiteral := "'" + strings.ReplaceAll(password, "'", "''") + "'"
	stmt := fmt.Sprintf("ALTER USER %s WITH PASSWORD %s", roleIdent, passLiteral)
	_, err := h.execInPod(ctx, namespace, pod, service,
		[]string{"psql", "-U", username, "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-c", stmt}, nil)
	return err
}

// waitPostgresReady blocks until the target postgres real server is accepting
// TCP connections. The first-time-init bootstrap server listens on the unix
// socket only, so a forced-TCP check (-h 127.0.0.1) rules it out: pg_isready
// gets no response from the socket-only bootstrap server and reports success
// only once the real server listens on the port. pg_isready needs no
// credentials, so this holds whatever the pod's host-auth policy is.
func (h *Handler) waitPostgresReady(ctx context.Context, namespace, pod, service, username string) error {
	return h.waitDBServerReady(ctx, namespace, pod, service, "postgres",
		[]string{"pg_isready", "-h", "127.0.0.1", "-p", "5432", "-U", username})
}

// waitDBServerReady polls a readiness command until it exits zero, bounding
// the whole wait with its own timeout so a stuck exec cannot outlive it and a
// genuinely broken service fails the migration instead of hanging.
func (h *Handler) waitDBServerReady(ctx context.Context, namespace, pod, service, dbType string, probe []string) error {
	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var lastErr error
	for {
		if _, err := h.execInPod(waitCtx, namespace, pod, service, probe, nil); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-waitCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("%s never accepted a connection: %w", dbType, lastErr)
			}
			return fmt.Errorf("%s never accepted a connection", dbType)
		case <-time.After(2 * time.Second):
		}
	}
}

// importPostgresDatabase restores one database on the target: create it if
// missing, replay the dump with ON_ERROR_STOP so the first error aborts the
// restore, then verify the table count against what the source reported.
// The dump arrives as a stream and feeds psql's stdin directly.
func (h *Handler) importPostgresDatabase(ctx context.Context, namespace, service, db string, expectedTables int64, dump io.Reader) error {
	if !validDBName.MatchString(db) {
		return fmt.Errorf("invalid database name %q", db)
	}

	creds, err := h.Client.CoreV1().Secrets(namespace).Get(ctx, secretname.ServiceCredentials(service), metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("credentials for %s not found on target: %w", service, err)
	}
	username := string(creds.Data["USERNAME"])
	if username == "" {
		return fmt.Errorf("credentials for %s carry no USERNAME", service)
	}

	pod, err := h.servicePod(ctx, namespace, service)
	if err != nil {
		return err
	}

	// A freshly provisioned postgres runs a temporary bootstrap server on the
	// unix socket during first-time init, then shuts it down and starts the
	// real server on TCP. The pod's readiness gate can go green against that
	// temporary server, so wait for the real server here — it is the only one
	// that listens on TCP — before touching any data, or the restore connects
	// to the bootstrap server and is cut off mid-stream.
	if err := h.waitPostgresReady(ctx, namespace, pod, service, username); err != nil {
		return fmt.Errorf("waiting for %s to finish initialising: %w", service, err)
	}

	// CREATE DATABASE cannot run inside the dump's transaction and has no
	// IF NOT EXISTS, so it goes through psql's \gexec: the SELECT emits the
	// statement only when the database is missing.
	ensure := fmt.Sprintf("SELECT 'CREATE DATABASE %q' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '%s')\n\\gexec\n", db, db)
	if _, err := h.execInPod(ctx, namespace, pod, service,
		[]string{"psql", "-U", username, "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-At"},
		strings.NewReader(ensure)); err != nil {
		return fmt.Errorf("creating database %s: %w", db, err)
	}

	if _, err := h.execInPod(ctx, namespace, pod, service,
		[]string{"psql", "-U", username, "-d", db, "-v", "ON_ERROR_STOP=1", "-q"},
		dump); err != nil {
		return fmt.Errorf("restoring database %s: %w", db, err)
	}

	got, err := h.countPostgresTables(ctx, namespace, pod, service, username, db)
	if err != nil {
		return fmt.Errorf("verifying restore of %s: %w", db, err)
	}
	if got != expectedTables {
		return fmt.Errorf("restore of %s produced %d tables, source has %d", db, got, expectedTables)
	}

	return nil
}

// listPostgresDatabases returns every non-template database, including
// "postgres" itself — users can and do create tables there, and pg_dumpall
// used to carry them.
func (h *Handler) listPostgresDatabases(ctx context.Context, namespace, pod, container, username string) ([]string, error) {
	out, err := h.execInPod(ctx, namespace, pod, container,
		[]string{"psql", "-U", username, "-d", "postgres", "-At", "-c",
			"SELECT datname FROM pg_database WHERE NOT datistemplate"}, nil)
	if err != nil {
		return nil, err
	}
	var databases []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			databases = append(databases, line)
		}
	}
	return databases, nil
}

func (h *Handler) countPostgresTables(ctx context.Context, namespace, pod, container, username, db string) (int64, error) {
	out, err := h.execInPod(ctx, namespace, pod, container,
		[]string{"psql", "-U", username, "-d", db, "-At", "-c", userTableCountQuery}, nil)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

// servicePod returns a pod backing the named service.
func (h *Handler) servicePod(ctx context.Context, namespace, serviceName string) (string, error) {
	pods, err := h.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", serviceName),
	})
	if err != nil || len(pods.Items) == 0 {
		return "", fmt.Errorf("no running pod for service %s", serviceName)
	}
	return pods.Items[0].Name, nil
}

// execInPod runs a command in the named pod and returns its stdout. Stderr
// is folded into the returned error. Commands are argv form throughout, so
// no argument ever passes through a shell.
func (h *Handler) execInPod(ctx context.Context, namespace, pod, container string, cmd []string, stdin io.Reader) (string, error) {
	var stdout bytes.Buffer
	if err := h.execInPodTo(ctx, namespace, pod, container, cmd, stdin, &stdout); err != nil {
		return stdout.String(), err
	}
	return stdout.String(), nil
}

// execInPodTo runs a command in the named pod, writing its stdout to the
// given writer. Bulk transfers pass a pipe here so dumps stream instead of
// accumulating in memory.
func (h *Handler) execInPodTo(ctx context.Context, namespace, pod, container string, cmd []string, stdin io.Reader, stdout io.Writer) error {
	req := h.Client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdin:     stdin != nil,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(h.RESTConfig, "POST", req.URL())
	if err != nil {
		return err
	}

	var stderr bytes.Buffer
	opts := remotecommand.StreamOptions{Stdout: stdout, Stderr: &stderr}
	if stdin != nil {
		opts.Stdin = stdin
	}
	if err := exec.StreamWithContext(ctx, opts); err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(stderr.String()), err)
	}
	return nil
}

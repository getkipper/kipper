package migration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/builder"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// runMigration executes the full migration sequence in the background.
func (h *Handler) runMigration(session *Session, token *Token) {
	// The context is the cancellation path: CancelHandler cancels it via the
	// session, which aborts whatever dump, exec, or upload is mid-stream.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session.SetCancel(cancel)

	// Persist the step log periodically so a console-api restart mid-transfer
	// leaves an operator-visible record of how far the run got, instead of
	// only the state at the last terminal Save. The saver is stopped and
	// joined before any terminal Save, so an in-flight periodic write can
	// never land after — and regress — the final status.
	saverDone := make(chan struct{})
	saverStopped := make(chan struct{})
	var stopSaverOnce sync.Once
	stopSaver := func() {
		stopSaverOnce.Do(func() {
			close(saverDone)
			<-saverStopped
		})
	}
	defer stopSaver()
	go func() {
		defer close(saverStopped)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-saverDone:
				return
			case <-ticker.C:
				h.Sessions.Save(session)
			}
		}
	}()

	log := func(msg string, args ...interface{}) {
		fmt.Printf("[migration %s] %s\n", shortID(session.ID), fmt.Sprintf(msg, args...))
	}

	defer func() {
		if r := recover(); r != nil {
			stopSaver()
			session.Finish(SessionFailed, fmt.Sprintf("panic: %v", r))
			log("panic: %v", r)
			h.Sessions.Save(session)
		}
	}()

	log("starting migration of %v to %s", session.Projects, session.TargetCluster)

	if warning := h.autoscaledAppsWarning(ctx, session.Projects); warning != nil {
		session.AddStep(*warning)
	}

	// Classify every app's route host once, before any transfer, so the app
	// secrets (phase 1) and the app specs (phase 3) rewrite coexisting host
	// references consistently. The table spans all migrated projects, so a
	// cross-project reference (one app naming another's host) is covered.
	res, err := h.resolveDomains(ctx, session)
	if err != nil {
		stopSaver()
		session.Finish(SessionFailed, fmt.Sprintf("classifying app domains: %v", err))
		log("failed to classify app domains: %v", err)
		h.Sessions.Save(session)
		return
	}

	for _, project := range session.Projects {
		if session.IsCancelled() {
			log("cancelled")
			h.abortTarget(session, log)
			return
		}
		if err := h.migrateProject(ctx, session, token, project, res); err != nil {
			// A cancel mid-transfer surfaces as a context error from the
			// stream it aborted; the session is already cancelled and must
			// not be rewritten as failed.
			if session.IsCancelled() || ctx.Err() != nil {
				stopSaver()
				log("cancelled mid-transfer: %v", err)
				h.Sessions.Save(session)
				h.abortTarget(session, log)
				return
			}
			stopSaver()
			session.Finish(SessionFailed, err.Error())
			log("failed: %v", err)
			h.Sessions.Save(session)
			h.abortTarget(session, log)
			return
		}
	}

	stopSaver()
	log("all projects migrated, entering verification")
	session.SetStatus(SessionVerifying)
	session.AddStep(Step{
		Name:   "Team accounts (Dex users)",
		Phase:  "verification",
		Status: StepSkipped,
		Detail: "User accounts move separately from apps and data; the target starts with only its bootstrap admin",
		ManualSteps: []string{
			"# Capture the Dex config on the source cluster:",
			"kubectl get cm dex-config -n dex -o yaml > dex-config-snapshot.yaml",
			"# Import it on the target cluster (existing entries there win on conflicts):",
			"kip --cluster <target> user import dex-config-snapshot.yaml --restart-dex",
		},
	})
	session.AddStep(Step{
		Name:   "Waiting for verification",
		Phase:  "verification",
		Status: StepRunning,
		Detail: "Apps are live on temporary URLs. Verify and confirm domain cutover",
	})
	// The verifying state must survive a console-api restart: it carries the
	// saved routes and target secret the cutover still needs.
	h.Sessions.Save(session)
}

// abortTarget asks the target to remove the unadopted secrets this run
// created there. Best-effort: the target may be unreachable in the same
// failure that ended the run, and a retried migration re-sends every secret
// anyway.
func (h *Handler) abortTarget(session *Session, log func(string, ...interface{})) {
	token := &Token{Endpoint: session.TargetAPI, Secret: session.Secret}
	if _, err := h.callTarget(token, "POST", fmt.Sprintf("/api/v1/migrate-target/%s/abort", session.ID), nil); err != nil {
		log("cleaning up transferred secrets on target: %v", err)
	}
}

// autoscaledAppsWarning surfaces apps whose HPA keeps them serving through a
// replica freeze. The scale writers refuse replica edits on these apps, but
// an operator who skipped the freeze — or froze before that guard existed —
// must see in the flow who is still taking writes during the data copy.
func (h *Handler) autoscaledAppsWarning(ctx context.Context, projects []string) *Step {
	var autoscaled []string
	for _, project := range projects {
		namespaces, err := h.getProjectNamespaces(ctx, project)
		if err != nil {
			continue
		}
		for _, ns := range namespaces {
			var appList kipperv1.AppList
			if err := h.CRClient.List(ctx, &appList, crclient.InNamespace(ns)); err != nil {
				continue
			}
			for _, app := range appList.Items {
				if app.Spec.Autoscale != nil && app.Spec.Autoscale.Enabled {
					autoscaled = append(autoscaled, ns+"/"+app.Name)
				}
			}
		}
	}
	if len(autoscaled) == 0 {
		return nil
	}
	return &Step{
		Name:   "Write freeze check",
		Phase:  "structure",
		Status: StepSkipped,
		Detail: fmt.Sprintf("autoscaling keeps %s serving during the data copy; anything written there after its copy stays on this cluster", strings.Join(autoscaled, ", ")),
		ManualSteps: []string{
			"# Autoscaled apps keep running through a replica freeze. To stop their writes:",
			"kip app autoscale <app> --off",
			"kip app scale <app> --replicas 0 --project <project>",
			"# Re-enable autoscaling on the target after the domain cutover.",
		},
	}
}

func (h *Handler) migrateProject(ctx context.Context, session *Session, token *Token, projectName string, res *domainResolution) error {
	// Phase 1: Structure

	// Step 1: Transfer Project CR
	stepName := fmt.Sprintf("Creating project %s on target", projectName)
	session.AddStep(Step{
		Name:   stepName,
		Phase:  "structure",
		Status: StepRunning,
	})

	projectSpec, err := h.exportProjectSpec(ctx, projectName)
	if err != nil {
		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepFailed
			s.Error = err.Error()
		})
		return fmt.Errorf("exporting project %s: %w", projectName, err)
	}

	if err := h.sendToTarget(token, fmt.Sprintf("/api/v1/migrate-target/%s/resource", session.ID), map[string]interface{}{
		"kind": "Project",
		"name": projectName,
		"spec": projectSpec,
	}); err != nil {
		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepFailed
			s.Error = err.Error()
		})
		return fmt.Errorf("creating project on target: %w", err)
	}

	session.UpdateStep(stepName, func(s *Step) {
		s.Status = StepCompleted
		now := time.Now()
		s.CompletedAt = &now
	})

	// Step 2: Wait for namespaces
	stepName = fmt.Sprintf("Waiting for namespaces (%s)", projectName)
	session.AddStep(Step{
		Name:   stepName,
		Phase:  "structure",
		Status: StepRunning,
	})

	namespaces, err := h.getProjectNamespaces(ctx, projectName)
	if err != nil {
		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepFailed
			s.Error = err.Error()
		})
		return fmt.Errorf("getting namespaces for %s: %w", projectName, err)
	}

	// Poll the target until the namespaces exist. Timing out must fail the
	// run here with the real cause: a transfer into a missing namespace
	// would otherwise surface as a misleading scope-gate 403.
	nsReady := false
	var nsErr error
	for attempt := 0; attempt < 60 && !nsReady; attempt++ {
		allReady := true
		for _, ns := range namespaces {
			resp, err := h.callTarget(token, "GET", fmt.Sprintf("/api/v1/migrate-target/%s/status?namespace=%s", session.ID, ns), nil)
			if err != nil {
				nsErr = err
				allReady = false
				break
			}
			if ready, ok := resp["namespace_ready"].(bool); !ok || !ready {
				nsErr = fmt.Errorf("namespace %s has not been provisioned on the target", ns)
				allReady = false
				break
			}
		}
		if allReady {
			nsReady = true
			break
		}
		time.Sleep(time.Second)
	}
	if !nsReady {
		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepFailed
			s.Error = nsErr.Error()
		})
		return fmt.Errorf("waiting for namespaces of %s on target: %w", projectName, nsErr)
	}

	session.UpdateStep(stepName, func(s *Step) {
		s.Status = StepCompleted
		s.Detail = fmt.Sprintf("%d namespaces ready", len(namespaces))
		now := time.Now()
		s.CompletedAt = &now
	})

	// Step 3: Transfer Secrets
	for _, ns := range namespaces {
		if err := h.transferSecrets(ctx, session, token, ns, res); err != nil {
			return fmt.Errorf("transferring secrets in %s: %w", ns, err)
		}
	}

	// Phase 2: Data — Services, databases, images

	// Step 4: Transfer Services and database data
	for _, ns := range namespaces {
		if err := h.migrateServices(ctx, session, token, ns); err != nil {
			return fmt.Errorf("migrating services in %s: %w", ns, err)
		}
	}

	// Step 5: Account for container images (git apps rebuild on the target)
	for _, ns := range namespaces {
		if err := h.migrateImages(ctx, session, ns); err != nil {
			return fmt.Errorf("migrating images in %s: %w", ns, err)
		}
	}

	// Step 6: Transfer Volumes and their data
	for _, ns := range namespaces {
		if err := h.migrateVolumes(ctx, session, token, ns); err != nil {
			return fmt.Errorf("migrating volumes in %s: %w", ns, err)
		}
	}

	// Phase 3: Resources — Apps, Functions, Jobs (with temporary routes)

	// Step 6: Transfer App CRs (movers' custom domains stripped for cutover;
	// coexist and gateway hosts stripped so the target derives its own)
	for _, ns := range namespaces {
		if err := h.migrateApps(ctx, session, token, ns, res); err != nil {
			return fmt.Errorf("migrating apps in %s: %w", ns, err)
		}
	}

	// Step 7: Transfer Function CRs
	for _, ns := range namespaces {
		if err := h.migrateFunctions(ctx, session, token, ns, res); err != nil {
			return fmt.Errorf("migrating functions in %s: %w", ns, err)
		}
	}

	// Step 8: Transfer Job CRs
	for _, ns := range namespaces {
		if err := h.migrateJobs(ctx, session, token, ns, res); err != nil {
			return fmt.Errorf("migrating jobs in %s: %w", ns, err)
		}
	}

	// Phase 4: Verification — wait for all resources to be healthy
	if err := h.waitForHealthy(session, token, namespaces); err != nil {
		return err
	}

	return nil
}

func (h *Handler) exportProjectSpec(ctx context.Context, name string) (map[string]interface{}, error) {
	var project kipperv1.Project
	if err := h.CRClient.Get(ctx, crclient.ObjectKey{Name: name}, &project); err != nil {
		return nil, err
	}

	envs := make([]map[string]interface{}, len(project.Spec.Environments))
	for i, env := range project.Spec.Environments {
		entry := map[string]interface{}{"name": env.Name}
		if env.Quota != nil {
			entry["quota"] = map[string]interface{}{
				"cpuRequest":    env.Quota.CPURequest,
				"cpuLimit":      env.Quota.CPULimit,
				"memoryRequest": env.Quota.MemoryRequest,
				"memoryLimit":   env.Quota.MemoryLimit,
			}
		}
		envs[i] = entry
	}

	spec := map[string]interface{}{
		"environments": envs,
	}
	if project.Spec.DisplayName != "" {
		spec["displayName"] = project.Spec.DisplayName
	}
	if project.Spec.Tier != "" {
		spec["tier"] = project.Spec.Tier
	}
	if project.Spec.SharedStorage != nil {
		spec["sharedStorage"] = map[string]interface{}{
			"enabled": project.Spec.SharedStorage.Enabled,
			"size":    project.Spec.SharedStorage.Size,
		}
	}

	return spec, nil
}

func (h *Handler) getProjectNamespaces(ctx context.Context, projectName string) ([]string, error) {
	nsList, err := h.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("kipper.run/project=%s", projectName),
	})
	if err != nil {
		return nil, err
	}

	var namespaces []string
	for _, ns := range nsList.Items {
		namespaces = append(namespaces, ns.Name)
	}

	if len(namespaces) == 0 {
		return nil, fmt.Errorf("no namespaces found for project %s", projectName)
	}

	return namespaces, nil
}

// transferableSecret reports whether a Secret moves in the bulk Secret phase.
// Service-account tokens and Helm release records are cluster-local state the
// target regenerates itself.
//
// Two kinds of credential are excluded because something else carries them. A
// service's shared credentials travel inside their own Service's handover, so
// that the target owns them from the moment they land. A derived per-binding
// Secret is a projection of those credentials that the workload's controller
// renders for itself, so sending one puts an object the receiving controller
// refuses to write through under a name it needs.
//
// ridesWithService holds the shared credentials names for the Services that
// exist in this namespace. A <name>-credentials Secret with no Service of that
// name is nobody's shared credentials and still travels as an ordinary Secret.
func transferableSecret(secret *corev1.Secret, ridesWithService map[string]bool) bool {
	if secret.Type == corev1.SecretTypeServiceAccountToken {
		return false
	}
	for _, prefix := range []string{"default-token-", "sh.helm.release"} {
		if strings.HasPrefix(secret.Name, prefix) {
			return false
		}
	}
	if ridesWithService[secret.Name] {
		return false
	}
	if isPublishedEnvironment(secret) {
		return false
	}
	return !isLiveProjection(secret)
}

// isPublishedEnvironment reports whether a Secret is one generation of a
// workload's environment, which the target's controller publishes for itself.
//
// Sending one is worse than sending something redundant. The name is a digest of
// the content, so the copy lands at exactly the name the target will compute,
// carrying an owner reference to a workload UID that does not exist there. The
// receiving controller then refuses it as somebody else's object and stops
// publishing, and the workload never starts.
func isPublishedEnvironment(secret *corev1.Secret) bool {
	owner := metav1.GetControllerOf(secret)
	if owner == nil || !strings.HasPrefix(owner.APIVersion, kipperv1.GroupVersion.Group+"/") {
		return false
	}
	var kind secretname.Kind
	switch owner.Kind {
	case "App":
		kind = secretname.KindApp
	case "Function":
		kind = secretname.KindFunction
	case "Job":
		kind = secretname.KindJob
	default:
		return false
	}
	return strings.HasPrefix(secret.Name, secretname.EnvGenerationPrefix(kind, owner.Name))
}

// isLiveProjection reports whether a Secret is a per-binding projection its
// owner's controller will render again on the target.
//
// The label alone is not enough to drop something from the only transfer that
// would have carried it. Anything able to write a Secret can set a label, and a
// projection whose workload is gone is not reproduced anywhere. Requiring the
// workload's controller reference means the exclusion covers what the render
// actually produces, and a mislabelled or orphaned object travels as the
// ordinary Secret it is.
func isLiveProjection(secret *corev1.Secret) bool {
	if secret.Labels[labels.Binding] != labels.BindingTrue {
		return false
	}
	owner := metav1.GetControllerOf(secret)
	return owner != nil && (owner.Kind == "App" || owner.Kind == "Function") &&
		strings.HasPrefix(owner.APIVersion, kipperv1.GroupVersion.Group+"/")
}

// credentialsRidingWithServices names the shared credentials Secret of every
// Service in a namespace, which is the set the bulk transfer leaves behind.
func (h *Handler) credentialsRidingWithServices(ctx context.Context, namespace string) (map[string]bool, error) {
	var services kipperv1.ServiceList
	if err := h.CRClient.List(ctx, &services, crclient.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing services in %s: %w", namespace, err)
	}
	riding := make(map[string]bool, len(services.Items))
	for _, svc := range services.Items {
		riding[secretname.ServiceCredentials(svc.Name)] = true
	}
	return riding, nil
}

func (h *Handler) transferSecrets(ctx context.Context, session *Session, token *Token, namespace string, res *domainResolution) error {
	secrets, err := h.Client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	riding, err := h.credentialsRidingWithServices(ctx, namespace)
	if err != nil {
		return err
	}

	toTransfer := 0
	for i := range secrets.Items {
		if transferableSecret(&secrets.Items[i], riding) {
			toTransfer++
		}
	}
	if toTransfer == 0 {
		return nil
	}

	stepName := fmt.Sprintf("Transferring secrets (%s)", namespace)
	session.AddStep(Step{
		Name:       stepName,
		Phase:      "structure",
		Status:     StepRunning,
		BytesTotal: int64(toTransfer),
	})

	sent := 0
	for _, secret := range secrets.Items {
		if !transferableSecret(&secret, riding) {
			continue
		}

		// The app's own sensitive secret can name a coexisting app's source
		// host (DATABASE_URL, CORS_ALLOWED_ORIGINS), and it is copied verbatim,
		// so rewrite those hosts here. Scope is exact: only this migration's
		// app-<app>-secrets, never TLS, registry, git, or user-owned secrets, and a
		// non-UTF-8 value is preserved byte-for-byte.
		rewriteSecret := res != nil && len(res.rewrites) > 0 && res.appSecrets[appKey(namespace, secret.Name)]

		// Always base64-encode secret data for safe JSON transport
		data := make(map[string]string, len(secret.Data))
		for k, v := range secret.Data {
			out := v
			if rewriteSecret && utf8.Valid(v) {
				out = []byte(rewriteHostRefs(string(v), res.rewrites))
			}
			data[k] = base64.StdEncoding.EncodeToString(out)
		}

		payload := map[string]interface{}{
			"name":      secret.Name,
			"namespace": namespace,
			"type":      string(secret.Type),
			"labels":    secret.Labels,
			"data":      data,
		}

		if err := h.sendToTarget(token, fmt.Sprintf("/api/v1/migrate-target/%s/secret", session.ID), payload); err != nil {
			session.UpdateStep(stepName, func(s *Step) {
				s.Status = StepFailed
				s.Error = fmt.Sprintf("failed to transfer %s: %v", secret.Name, err)
			})
			return fmt.Errorf("transferring secret %s: %w", secret.Name, err)
		}

		sent++
		session.UpdateStep(stepName, func(s *Step) {
			s.BytesDone = int64(sent)
			s.Detail = fmt.Sprintf("%d/%d secrets", sent, toTransfer)
		})
	}

	session.UpdateStep(stepName, func(s *Step) {
		s.Status = StepCompleted
		now := time.Now()
		s.CompletedAt = &now
	})

	return nil
}

// --- CR creation helpers for target side ---
//
// Every creator applies with create-or-update semantics: a retried migration
// (or a confirmed overwrite) replays resources that already exist on this
// cluster, and the replay must converge on the incoming spec instead of
// failing with AlreadyExists.

func (h *Handler) createProject(ctx context.Context, name string, spec map[string]interface{}) error {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return err
	}

	var projectSpec kipperv1.ProjectSpec
	if err := json.Unmarshal(specJSON, &projectSpec); err != nil {
		return err
	}

	project := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err = controllerutil.CreateOrUpdate(ctx, h.CRClient, project, func() error {
		setLabel(&project.ObjectMeta, "app.kubernetes.io/managed-by", "kipper")
		project.Spec = projectSpec
		return nil
	})
	return err
}

// transferredCredentials is a service's shared credentials Secret, carried in
// the same request as the Service CR it belongs to. Data values are base64
// encoded, as they are on the bulk Secret endpoint.
type transferredCredentials struct {
	Labels map[string]string `json:"labels,omitempty"`
	Data   map[string]string `json:"data"`
}

// createService recreates one service here: its credentials first, then the CR,
// then the ownership that ties the two together.
//
// The order is what makes the result correct rather than merely tidy. An engine
// reads its password from the credentials Secret as env when its container
// starts, and postgres, mysql, mongodb and rabbitmq write that value into
// persistent state the first time they initialise. Creating the CR first would
// have the reconciler mint a password of its own, the engine initialise against
// it, and the transferred credentials arrive afterwards to contradict a database
// that has already made up its mind.
//
// Ownership goes on last because it needs the CR's UID, which does not exist
// until the CR does. This is the only place that ties an existing Secret to a
// Service, and it may do so because it wrote that Secret's bytes itself, in this
// call, from a payload the sender is required to supply. Nothing here infers
// ownership from a name, a label or a set of keys, which is what the reconciler
// used to do and no longer does.
func (h *Handler) createService(ctx context.Context, sessionID, name, namespace string, spec map[string]interface{}, creds *transferredCredentials) error {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return err
	}

	var serviceSpec kipperv1.ServiceSpec
	if err := json.Unmarshal(specJSON, &serviceSpec); err != nil {
		return err
	}

	credentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretname.ServiceCredentials(name),
			Namespace: namespace,
			Labels:    creds.Labels,
		},
		Type: corev1.SecretTypeOpaque,
		Data: decodeSecretData(creds.Data),
	}

	if err := h.refuseUnsafeCredentialHandover(ctx, name, namespace, serviceSpec.Type, credentials.Data); err != nil {
		return err
	}

	written, err := h.applyTransferredSecret(ctx, sessionID, credentials)
	if err != nil {
		return err
	}

	service := &kipperv1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, h.CRClient, service, func() error {
		setLabel(&service.ObjectMeta, "app.kubernetes.io/managed-by", "kipper")
		setLabel(&service.ObjectMeta, "kipper.run/service-type", serviceSpec.Type)
		service.Spec = serviceSpec
		return nil
	}); err != nil {
		return err
	}

	return h.claimCredentials(ctx, service, written)
}

// refuseUnsafeCredentialHandover stops a handover that would write over
// something this migration has no business touching, before any of it is
// written. Checking after the write would mean the damage is already done and
// only the ownership was refused.
//
// Two things disqualify a handover. A credentials Secret under any controller
// other than the exact Service CR standing here now belongs to that controller,
// whatever its name says: a reference naming a Service of this name whose UID
// has moved on is a stale reference, not a permission. And a service already
// running here with a password of its own would keep it, because the engine
// holds what it initialised with and no service StatefulSet carries a credential
// digest, so nothing restarts the pod. Publishing the source's password would
// roll the bound workloads onto a credential the running database refuses.
func (h *Handler) refuseUnsafeCredentialHandover(ctx context.Context, name, namespace, serviceType string, incoming map[string][]byte) error {
	existing, err := h.Client.CoreV1().Secrets(namespace).Get(ctx, secretname.ServiceCredentials(name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the credentials of %s: %w", name, err)
	}

	if owner := metav1.GetControllerOf(existing); owner != nil {
		var current kipperv1.Service
		err := h.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &current)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("reading service %s: %w", name, err)
		}
		if apierrors.IsNotFound(err) || owner.UID != current.UID {
			return fmt.Errorf("the credentials of %s on this cluster are controlled by %s %q, which is not the service this migration is creating, so they will not be written over",
				name, owner.Kind, owner.Name)
		}
	}

	// No StatefulSet means nothing has initialised against these bytes yet, so
	// replacing them is free. This is also the replay case once the engine is
	// up, which the value comparison below lets through.
	if _, err := h.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading the statefulset of %s: %w", name, err)
	}

	// A key missing from either side counts as a difference. Comparing only
	// where both carry the key would let credentials with no PASSWORD at all
	// replace a running service's, which is the same outcome by a quieter route.
	for _, key := range kipperv1.CredentialKeys(serviceType) {
		if !kipperv1.IsSensitiveCredentialKey(key) {
			continue
		}
		if !bytes.Equal(existing.Data[key], incoming[key]) {
			return fmt.Errorf("service %s already runs on this cluster with a password of its own: taking the source's would leave its apps authenticating with a credential the running database does not accept. Remove the service here first, or migrate into a project that does not have one", name)
		}
	}
	return nil
}

// claimCredentials points the credentials Secret's controller reference at the
// Service that owns it, so the injection gate admits it into bound workloads.
//
// The update carries the resourceVersion of the write this call just made, so a
// third party writing in between loses the claim rather than having its bytes
// silently adopted. The only writer with business here in that window is the
// Service reconciler adding the type's default keys, which is why a conflict is
// retried against the credentials this call wrote rather than abandoned.
func (h *Handler) claimCredentials(ctx context.Context, svc *kipperv1.Service, written *corev1.Secret) error {
	claim := func(secret *corev1.Secret) error {
		if owner := metav1.GetControllerOf(secret); owner != nil {
			if owner.UID == svc.UID {
				return nil
			}
			return fmt.Errorf("the credentials of %s are controlled by %s %q, so this migration will not claim them",
				svc.Name, owner.Kind, owner.Name)
		}
		if err := controllerutil.SetControllerReference(svc, secret, h.CRClient.Scheme()); err != nil {
			return fmt.Errorf("claiming the credentials of %s: %w", svc.Name, err)
		}
		_, err := h.Client.CoreV1().Secrets(secret.Namespace).Update(ctx, secret, metav1.UpdateOptions{})
		return err
	}

	err := claim(written)
	if !apierrors.IsConflict(err) {
		return err
	}

	fresh, err := h.Client.CoreV1().Secrets(written.Namespace).Get(ctx, written.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("re-reading the credentials of %s: %w", svc.Name, err)
	}
	if !claimableAfterConflict(written.Data, fresh.Data, svc.Spec.Type) {
		return fmt.Errorf("the credentials of %s changed while they were being handed over, so this migration will not claim bytes it did not write", svc.Name)
	}
	return claim(fresh)
}

// claimableAfterConflict reports whether a Secret that changed under the claim
// still holds only what this handover wrote.
//
// One writer legitimately touches the object in that window: the Service
// reconciler, filling in the type's defaults. Everything it may add is known, so
// the comparison is exact rather than a superset test. Accepting any extra key
// would matter, because the shared credentials reach bound workloads through
// envFrom, so a key added here arrives in their environment under the Service's
// own provenance.
func claimableAfterConflict(written, fresh map[string][]byte, serviceType string) bool {
	defaults := kipperv1.CredentialDefaults(serviceType)
	for key, value := range fresh {
		if want, ours := written[key]; ours {
			if !bytes.Equal(value, want) {
				return false
			}
			continue
		}
		if def, allowed := defaults[key]; !allowed || string(value) != def {
			return false
		}
	}
	for key := range written {
		if _, present := fresh[key]; !present {
			return false
		}
	}
	return true
}

func (h *Handler) createApp(ctx context.Context, name, namespace string, spec map[string]interface{}) error {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return err
	}

	var appSpec kipperv1.AppSpec
	if err := json.Unmarshal(specJSON, &appSpec); err != nil {
		return err
	}

	// A migrated git app carries an image reference into its source
	// cluster's registry, which does not exist here. Deploy the standard
	// "building" placeholder and rebuild from git on this cluster — the
	// same flow a fresh git deploy uses. The build clones the branch head.
	// On a replay this rebuilds even if an earlier attempt already built,
	// which repeats work but always converges on a runnable image.
	rebuild := appSpec.Git != nil
	var commit string
	if rebuild {
		var err error
		if commit, err = migrationBuildID(); err != nil {
			return err
		}
		appSpec.Image = "busybox:latest"
	}

	app := &kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, h.CRClient, app, func() error {
		setLabel(&app.ObjectMeta, "app", name)
		setLabel(&app.ObjectMeta, "app.kubernetes.io/managed-by", "kipper")
		if rebuild {
			// The cutover build gate compares the reported build status
			// against this annotation, so a Succeeded left by an earlier
			// attempt's build cannot pass for the one triggered here.
			if app.Annotations == nil {
				app.Annotations = map[string]string{}
			}
			app.Annotations[migrationBuildAnnotation] = commit
		}
		app.Spec = appSpec
		return nil
	}); err != nil {
		return err
	}

	if rebuild {
		// The status resets before the Job exists: resetting afterwards
		// races the build controller, which writes status for every
		// completed build Job it reconciles. Retry on conflict: the App
		// reconciler modifies the just-created App (finalizers, status), so
		// the copy from CreateOrUpdate is often already stale by now.
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var fresh kipperv1.App
			if err := h.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &fresh); err != nil {
				return err
			}
			fresh.Status.Build = &kipperv1.AppBuildStatus{Phase: "Pending", Commit: commit}
			return h.CRClient.Status().Update(ctx, &fresh)
		}); err != nil {
			return fmt.Errorf("resetting build status for %s/%s: %w", namespace, name, err)
		}
		if _, err := builder.CreateBuildJob(ctx, h.Client, app, commit); err != nil {
			return fmt.Errorf("triggering rebuild for %s/%s: %w", namespace, name, err)
		}
	}

	return nil
}

func (h *Handler) createFunction(ctx context.Context, name, namespace string, spec map[string]interface{}) error {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return err
	}

	var fnSpec kipperv1.FunctionSpec
	if err := json.Unmarshal(specJSON, &fnSpec); err != nil {
		return err
	}

	fn := &kipperv1.Function{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, h.CRClient, fn, func() error {
		setLabel(&fn.ObjectMeta, "app.kubernetes.io/managed-by", "kipper")
		fn.Spec = fnSpec
		return nil
	})
	return err
}

// setLabel sets one label, initialising the map when the object is new.
func setLabel(meta *metav1.ObjectMeta, key, value string) {
	if meta.Labels == nil {
		meta.Labels = map[string]string{}
	}
	meta.Labels[key] = value
}

// migrationBuildID returns a unique identifier for one triggered rebuild.
// It doubles as the image tag and the build-gate identity, so a timestamp
// alone is not enough: two attempts within the same second would collide,
// letting the first attempt's build pass for the second's. The full ID
// travels on the build Job's commit annotation, so the gate comparison
// works even where the Job name digests it. A failed entropy read fails
// the migration — a predictable identifier would quietly void the gate.
func migrationBuildID() (string, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("generating build id: %w", err)
	}
	return fmt.Sprintf("migrate-%d-%s", time.Now().Unix(), hex.EncodeToString(suffix)), nil
}

func (h *Handler) updateAppRoute(ctx context.Context, name, namespace string, spec map[string]interface{}) error {
	routeData, ok := spec["route"]
	if !ok {
		return nil
	}

	routeJSON, err := json.Marshal(routeData)
	if err != nil {
		return err
	}

	var route kipperv1.AppRoute
	if err := json.Unmarshal(routeJSON, &route); err != nil {
		return err
	}

	// Re-fetch and retry on conflict: this runs at cutover while the App
	// reconciler is actively managing the app, so a bare read-modify-write
	// would lose the race.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var app kipperv1.App
		if err := h.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &app); err != nil {
			return err
		}
		app.Spec.Route = &route
		return h.CRClient.Update(ctx, &app)
	})
}

func (h *Handler) createJob(ctx context.Context, name, namespace string, spec map[string]interface{}) error {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return err
	}

	var jobSpec kipperv1.JobSpec
	if err := json.Unmarshal(specJSON, &jobSpec); err != nil {
		return err
	}

	job := &kipperv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, h.CRClient, job, func() error {
		setLabel(&job.ObjectMeta, "app.kubernetes.io/managed-by", "kipper")
		job.Spec = jobSpec
		return nil
	})
	return err
}

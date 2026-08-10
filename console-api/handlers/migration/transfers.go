package migration

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/hkdf"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// transferLeaseLabel carries the unix expiry of an import mover's
	// lease. The reaper deletes movers past it, so an interrupted
	// migration cannot leave receivers running forever.
	transferLeaseLabel = "kipper.run/transfer-lease-expiry"

	// transferIDLabel ties the import pod, service, and token secret to
	// their transfer.
	transferIDLabel = "kipper.run/transfer-id"

	// transferServiceLabel records the statefulset paused for a
	// service-PVC transfer, and transferServiceReplicasLabel its replica
	// count before the pause, so any teardown path can restore it.
	transferServiceLabel         = "kipper.run/transfer-service"
	transferServiceReplicasLabel = "kipper.run/transfer-service-replicas"

	// transferLeaseTTL bounds how long an import mover waits for its
	// transfer to finish before the reaper collects it.
	transferLeaseTTL = 24 * time.Hour

	// importPort is the import mover's ingest port.
	importPort = 8443
)

// deriveTransferToken derives the per-transfer bearer token from the
// session secret. Both clusters derive independently, so the master secret
// never enters a workload namespace: the movers only ever see this
// per-transfer value.
func deriveTransferToken(sessionSecret, transferID string) (string, error) {
	reader := hkdf.New(sha256.New, []byte(sessionSecret), nil, []byte("kipper-datatransfer/"+transferID))
	out := make([]byte, 32)
	if _, err := io.ReadFull(reader, out); err != nil {
		return "", fmt.Errorf("deriving transfer token: %w", err)
	}
	return hex.EncodeToString(out), nil
}

// transferRequest is the source cluster's ask to stand up one receiver.
type transferRequest struct {
	TransferID string `json:"transferID"`
	Namespace  string `json:"namespace"`
	Kind       string `json:"kind"`
	Target     struct {
		Volume  string `json:"volume,omitempty"`
		Service string `json:"service,omitempty"`
	} `json:"target"`
}

// CreateTransferHandler stands up the import side of one data transfer:
// a token Secret, an import mover pod, and its ClusterIP Service in the
// scoped namespace. Ingest traffic then flows through TransferProxyHandler
// on this host, so no new route, certificate, or ingress object exists.
// POST /api/v1/migrate-target/{session}/transfer
func (h *Handler) CreateTransferHandler(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "session")

	var req transferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TransferID == "" || req.Namespace == "" {
		respondError(w, http.StatusBadRequest, "transferID and namespace are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if !h.namespaceInScope(ctx, sessionID, req.Namespace) {
		respondError(w, http.StatusForbidden, "namespace not in migration scope")
		return
	}

	h.reapExpiredTransfers(ctx)

	session, ok := h.Sessions.Get(sessionID)
	if !ok {
		respondError(w, http.StatusUnauthorized, "unknown migration session")
		return
	}
	token, err := deriveTransferToken(session.Secret, req.TransferID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "deriving transfer token failed")
		return
	}

	if err := h.createImportResources(ctx, sessionID, &req, token); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("creating import resources: %v", err))
		return
	}

	respondJSON(w, http.StatusCreated, map[string]string{"transferID": req.TransferID})
}

// DeleteTransferHandler tears down one transfer's import resources.
// DELETE /api/v1/migrate-target/{session}/transfer/{transfer}
func (h *Handler) DeleteTransferHandler(w http.ResponseWriter, r *http.Request) {
	transferID := chi.URLParam(r, "transfer")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	ns, err := h.findTransferNamespace(ctx, chi.URLParam(r, "session"), transferID)
	if err != nil {
		respondError(w, http.StatusNotFound, "unknown transfer")
		return
	}
	if !h.namespaceInScope(ctx, chi.URLParam(r, "session"), ns) {
		respondError(w, http.StatusForbidden, "namespace not in migration scope")
		return
	}
	// The service is restarted only when the caller states the transfer
	// completed. The importer rewrites the destination in place, so any other
	// outcome may leave it part source and part target, and nothing here can
	// prove otherwise: the commit boundary is not observable from outside the
	// import pod. Defaulting to "leave it stopped" keeps a half-replaced
	// database off the network at the cost of a manual restart.
	restoreService := r.URL.Query().Get("restoreService") == "true"
	if err := h.deleteImportResources(ctx, ns, transferID, !restoreService); err != nil {
		// A failed restart of a paused service must not read as a clean
		// teardown, so the caller retries rather than assuming success.
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("teardown incomplete: %v", err))
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"deleted": transferID, "service_left_stopped": !restoreService})
}

// EnsureReceiverHandler repairs a receiver whose import pod died, without
// touching anything around it. The transfer retries by relaunching the
// exporter, but the importer lives here and is not part of that, so an evicted
// or OOM-killed one would leave every remaining attempt uploading to a dead
// backend. This never creates the Service, never scales the destination
// service, and never tears anything down, so it cannot collide with a setup
// that is still running or undo one that already finished.
// POST /api/v1/migrate-target/{session}/transfer/{transfer}/ensure
func (h *Handler) EnsureReceiverHandler(w http.ResponseWriter, r *http.Request) {
	transferID := chi.URLParam(r, "transfer")
	sessionID := chi.URLParam(r, "session")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	ns, err := h.findTransferNamespace(ctx, sessionID, transferID)
	if err != nil {
		respondError(w, http.StatusNotFound, "unknown transfer")
		return
	}
	if !h.namespaceInScope(ctx, sessionID, ns) {
		respondError(w, http.StatusForbidden, "namespace not in migration scope")
		return
	}

	// Only an established receiver is repaired. Without its token Secret the
	// setup either never ran or was torn down, and rebuilding a lone pod then
	// would strand it.
	existingPod, err := h.Client.CoreV1().Pods(ns).Get(ctx, importPodName(transferID), metav1.GetOptions{})
	if err != nil {
		respondError(w, http.StatusNotFound, "no receiver to repair")
		return
	}
	if existingPod.Status.Phase != corev1.PodSucceeded && existingPod.Status.Phase != corev1.PodFailed {
		respondJSON(w, http.StatusOK, map[string]interface{}{"receiver": transferID, "replaced": false})
		return
	}

	if err := h.ensureImportPod(ctx, schedulableCopy(existingPod)); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("repairing receiver: %v", err))
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"receiver": transferID, "replaced": true})
}

// TransferProxyHandler streams ingest requests through to the transfer's
// import mover Service. The route sits outside the session-secret
// middleware — the mover holds only its derived token — so this handler
// authenticates that token itself, and the mover verifies it again.
// Bodies stream through with constant memory.
// ANY /api/v1/migrate-target/{session}/transfer/{transfer}/*
func (h *Handler) TransferProxyHandler(w http.ResponseWriter, r *http.Request) {
	transferID := chi.URLParam(r, "transfer")
	sessionID := chi.URLParam(r, "session")

	// The derived bearer token is verified here, before any cluster-wide
	// API work, so unauthenticated internet traffic cannot amplify into
	// API-server load or probe which transfers exist. The mover verifies
	// the same token again behind the proxy.
	session, ok := h.Sessions.Get(sessionID)
	if !ok {
		respondError(w, http.StatusUnauthorized, "unknown migration session")
		return
	}
	// This route bypasses RequireMigrationSecret, so it enforces the same
	// session lifecycle itself: an empty secret and an expired accepted
	// session are both rejected before a token is derived, so a receiver
	// that outlives its session cannot keep serving an old bearer.
	if session.Secret == "" {
		respondError(w, http.StatusUnauthorized, "invalid migration session")
		return
	}
	if !session.ExpiresAt.IsZero() && time.Now().After(session.ExpiresAt) {
		h.Sessions.Delete(sessionID)
		respondError(w, http.StatusUnauthorized, "migration session expired")
		return
	}
	expected, err := deriveTransferToken(session.Secret, transferID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "token derivation failed")
		return
	}
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if presented == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(presented)) != 1 {
		respondError(w, http.StatusUnauthorized, "invalid transfer token")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	ns, err := h.findTransferNamespace(ctx, sessionID, transferID)
	cancel()
	if err != nil {
		respondError(w, http.StatusNotFound, "unknown transfer")
		return
	}
	if !h.namespaceInScope(r.Context(), sessionID, ns) {
		respondError(w, http.StatusForbidden, "namespace not in migration scope")
		return
	}

	rest := chi.URLParam(r, "*")
	backend := &url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("%s.%s.svc.cluster.local:%d", importServiceName(transferID), ns, importPort),
		Path:   "/" + rest,
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = backend
			pr.Out.Host = backend.Host
			// The session secret authenticates only this hop; the mover
			// must never see or log it.
			pr.Out.Header.Del("X-Migration-Secret")
		},
		FlushInterval: -1,
	}
	proxy.ServeHTTP(w, r)
}

// findTransferNamespace resolves a transfer ID to its namespace through the
// import Service's labels, bound to the session that created it, so the
// proxy never trusts caller-supplied paths.
func (h *Handler) findTransferNamespace(ctx context.Context, sessionID, transferID string) (string, error) {
	svcs, err := h.Client.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: transferIDLabel + "=" + transferID + ",kipper.run/migration-session=" + sessionID,
	})
	if err != nil {
		return "", err
	}
	if len(svcs.Items) == 0 {
		return "", fmt.Errorf("no import service for transfer %s", transferID)
	}
	return svcs.Items[0].Namespace, nil
}

func importPodName(transferID string) string     { return "dt-" + transferID + "-import" }
func importServiceName(transferID string) string { return "dt-" + transferID }

func (h *Handler) createImportResources(ctx context.Context, sessionID string, req *transferRequest, token string) error {
	labels := map[string]string{
		"app.kubernetes.io/managed-by": "kipper",
		"kipper.run/resource-type":     "datatransfer-import",
		"kipper.run/migration-session": sessionID,
		transferIDLabel:                req.TransferID,
		transferLeaseLabel:             strconv.FormatInt(time.Now().Add(transferLeaseTTL).Unix(), 10),
	}

	// A service-PVC transfer needs the claim released. The paused service
	// and its previous replica count are recorded in the labels BEFORE
	// anything is created or scaled, and the Secret and Service are created
	// before the scale-down: whatever partial state a failure leaves
	// behind, the lease sweep can always discover what to restart.
	if req.Kind == "servicePVC" && req.Target.Service != "" {
		labels[transferServiceLabel] = req.Target.Service
		sts, err := h.Client.AppsV1().StatefulSets(req.Namespace).Get(ctx, req.Target.Service, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading target service %s: %w", req.Target.Service, err)
		}
		replicas := int32(1)
		if sts.Spec.Replicas != nil && *sts.Spec.Replicas > 0 {
			replicas = *sts.Spec.Replicas
		}
		labels[transferServiceReplicasLabel] = strconv.Itoa(int(replicas))
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dt-" + req.TransferID + "-token", Namespace: req.Namespace, Labels: labels},
		StringData: map[string]string{"token": token},
	}
	// Creating the token Secret is the ownership gate for this receiver. The
	// API makes exactly one Create win, so the invocation that wins owns the
	// partial setup and is the only one permitted to tear it down. A loser must
	// never roll back: a re-sent request that raced the owner would otherwise
	// scale the destination service up underneath the owner's importer and
	// delete its pod mid-write. Losing is not an error — the owner is bringing
	// the same receiver up, and the caller waits for it to answer.
	owned := true
	if _, err := h.Client.CoreV1().Secrets(req.Namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("creating token secret: %w", err)
		}
		owned = false
	}
	// A loser does nothing further. Letting it also build the receiver would
	// break the very invariant this gate exists for: the owner's rollback would
	// then be free to delete a Service and pod a concurrent request had just
	// successfully created, leaving the exporter with no backend. Repairing a
	// receiver that died later is a separate operation (EnsureReceiverHandler),
	// which never creates or tears down the surrounding setup.
	if !owned {
		return nil
	}
	rollback := func() {
		h.rollbackImportSetup(ctx, req.Namespace, req.TransferID)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: importServiceName(req.TransferID), Namespace: req.Namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{transferIDLabel: req.TransferID},
			Ports:    []corev1.ServicePort{{Name: "ingest", Port: importPort}},
		},
	}
	if _, err := h.Client.CoreV1().Services(req.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		rollback()
		return fmt.Errorf("creating import service: %w", err)
	}

	if labels[transferServiceLabel] != "" {
		if err := h.scaleStatefulSet(ctx, req.Namespace, req.Target.Service, 0); err != nil {
			rollback()
			return fmt.Errorf("stopping target service %s: %w", req.Target.Service, err)
		}
	}

	pod, err := buildImportPod(req, labels, h.DatamoverImage)
	if err != nil {
		rollback()
		return err
	}
	if err := h.ensureImportPod(ctx, pod); err != nil {
		rollback()
		return err
	}
	return nil
}

// schedulableCopy rebuilds a pod so the scheduler places it afresh. Cloning a
// live Pod is not the same as its original manifest: scheduling writes the
// chosen node into the spec, so a receiver evicted when its node failed would
// come back pinned to that same node and fail again — defeating the repair it
// was created by.
func schedulableCopy(pod *corev1.Pod) *corev1.Pod {
	fresh := pod.DeepCopy()
	fresh.ResourceVersion = ""
	fresh.UID = ""
	fresh.CreationTimestamp = metav1.Time{}
	fresh.DeletionTimestamp = nil
	fresh.DeletionGracePeriodSeconds = nil
	fresh.Status = corev1.PodStatus{}
	// Scheduler-assigned placement. NodeSelector and affinity stay: those are
	// part of what the receiver asked for, not what it was given.
	fresh.Spec.NodeName = ""
	return fresh
}

// ensureImportPod brings the receiver up and keeps it up across retries. A pod
// that finished or failed cannot serve the next attempt, and its name is
// deterministic, so it is replaced rather than left in the way. A running one
// is left strictly alone: it may be mid-write to the destination.
func (h *Handler) ensureImportPod(ctx context.Context, pod *corev1.Pod) error {
	pods := h.Client.CoreV1().Pods(pod.Namespace)

	existing, err := pods.Get(ctx, pod.Name, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("reading import pod: %w", err)
	default:
		if existing.Status.Phase != corev1.PodSucceeded && existing.Status.Phase != corev1.PodFailed {
			return nil
		}
		if delErr := pods.Delete(ctx, pod.Name, metav1.DeleteOptions{}); delErr != nil && !errors.IsNotFound(delErr) {
			return fmt.Errorf("replacing the finished import pod: %w", delErr)
		}
		// Deletion is asynchronous, so the recreate below retries through the
		// window where the old object is still going away.
	}

	var createErr error
	for attempt := 0; attempt < 20; attempt++ {
		if _, createErr = pods.Create(ctx, pod, metav1.CreateOptions{}); createErr == nil {
			return nil
		}
		if !errors.IsAlreadyExists(createErr) {
			return fmt.Errorf("creating import pod: %w", createErr)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("creating import pod: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("creating import pod: %w", createErr)
}

func buildImportPod(req *transferRequest, labels map[string]string, image string) (*corev1.Pod, error) {
	args := []string{"import", "--listen", fmt.Sprintf(":%d", importPort), "--token-env", "DATAMOVER_TOKEN"}

	var (
		volumes      []corev1.Volume
		volumeMounts []corev1.VolumeMount
		env          []corev1.EnvVar
	)
	env = append(env, corev1.EnvVar{
		Name: "DATAMOVER_TOKEN",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "dt-" + req.TransferID + "-token"},
				Key:                  "token",
			},
		},
	})

	// Transient chunk staging and the bitmap live on a node-local scratch
	// volume, never on the target data volume: a shared (RWX Longhorn)
	// volume is NFS-backed, where the small-file churn of chunk staging
	// wedges the share-manager with stale handles. State lost to an import
	// pod crash just re-runs the item, which a frozen-source migration can
	// always afford.
	// A subdir under the scratch mount, never the mount point itself:
	// clearing previous state removes and recreates the state dir, which
	// fails on a mount point.
	args = append(args, "--state-dir", "/scratch/state")

	switch req.Kind {
	case "volume":
		if req.Target.Volume == "" {
			return nil, fmt.Errorf("volume transfer needs a target volume")
		}
		args = append(args, "--root", "/data")
		volumes = append(volumes, corev1.Volume{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shared-" + req.Target.Volume},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "data", MountPath: "/data"})
	case "servicePVC":
		// Raw PVC ingest: the statefulset is scaled to zero for the
		// transfer, so the import mover owns the claim until finalize.
		if req.Target.Service == "" {
			return nil, fmt.Errorf("service transfer needs a target service")
		}
		args = append(args, "--root", "/data")
		volumes = append(volumes, corev1.Volume{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-" + req.Target.Service + "-0"},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "data", MountPath: "/data"})
	default:
		return nil, fmt.Errorf("unsupported transfer kind %q", req.Kind)
	}

	volumes = append(volumes, corev1.Volume{
		Name:         "scratch",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "scratch", MountPath: "/scratch"})

	no := false
	yes := true
	root := int64(0)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: importPodName(req.TransferID), Namespace: req.Namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &no,
			Volumes:                      volumes,
			Containers: []corev1.Container{{
				Name:         "mover",
				Image:        image,
				Args:         args,
				Env:          env,
				VolumeMounts: volumeMounts,
				Ports:        []corev1.ContainerPort{{ContainerPort: importPort}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
				},
				SecurityContext: &corev1.SecurityContext{
					// Restoring arbitrary file ownership needs root; all
					// other privileges are dropped.
					RunAsUser:                &root,
					AllowPrivilegeEscalation: &no,
					ReadOnlyRootFilesystem:   &yes,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}, nil
}

// deleteImportResources tears down one transfer's receiver and restarts any
// service it paused. It restores the service BEFORE deleting the resources
// that record the pause, so a restore failure keeps the labels around for
// the next sweep to retry, and it returns that failure so callers do not
// report a half-done teardown as complete.
func (h *Handler) deleteImportResources(ctx context.Context, ns, transferID string, keepPaused bool) error {
	// A service stopped for a PVC transfer restarts on teardown, whichever
	// side initiated it and however partial the setup was: the pause is
	// recorded on secret, service, and pod alike, so any survivor names it.
	restoreLabels := map[string]string{}
	if secret, err := h.Client.CoreV1().Secrets(ns).Get(ctx, "dt-"+transferID+"-token", metav1.GetOptions{}); err == nil {
		restoreLabels = secret.Labels
	} else if svc, err := h.Client.CoreV1().Services(ns).Get(ctx, importServiceName(transferID), metav1.GetOptions{}); err == nil {
		restoreLabels = svc.Labels
	} else if pod, err := h.Client.CoreV1().Pods(ns).Get(ctx, importPodName(transferID), metav1.GetOptions{}); err == nil {
		restoreLabels = pod.Labels
	}
	if paused := restoreLabels[transferServiceLabel]; paused != "" {
		if keepPaused {
			// The destination may hold a partly-applied copy, so the service
			// stays down. The StatefulSet is annotated with why and with the
			// replica count to come back to, since the transfer resources that
			// recorded it are deleted below.
			replicas := restoreLabels[transferServiceReplicasLabel]
			if annErr := h.annotateHeldService(ctx, ns, paused, transferID, replicas); annErr != nil {
				return fmt.Errorf("recording why %s/%s is held down: %w", ns, paused, annErr)
			}
		} else {
			replicas := int32(1)
			if n, err := strconv.ParseInt(restoreLabels[transferServiceReplicasLabel], 10, 32); err == nil && n > 0 && n <= 64 {
				replicas = int32(n)
			}
			if err := h.scaleStatefulSet(ctx, ns, paused, replicas); err != nil {
				// Leave the pause labels in place so the next sweep retries; do
				// not delete the resources that record what to restore.
				return fmt.Errorf("restarting paused service %s/%s: %w", ns, paused, err)
			}
		}
	}
	_ = h.Client.CoreV1().Pods(ns).Delete(ctx, importPodName(transferID), metav1.DeleteOptions{})
	_ = h.Client.CoreV1().Services(ns).Delete(ctx, importServiceName(transferID), metav1.DeleteOptions{})
	_ = h.Client.CoreV1().Secrets(ns).Delete(ctx, "dt-"+transferID+"-token", metav1.DeleteOptions{})
	return nil
}

// rollbackImportSetup undoes a partial receiver setup. Only the invocation
// that won the token-Secret create reaches this, so no concurrent setup can be
// mid-flight for the same transfer. The receiver is still checked: a previous
// run may have left an importer behind that this setup adopted, and that one
// may already be rewriting the destination. Any lookup error counts as "an
// importer might exist", so the service stays stopped.
func (h *Handler) rollbackImportSetup(ctx context.Context, ns, transferID string) {
	receiverAbsent := false
	if _, err := h.Client.CoreV1().Pods(ns).Get(ctx, importPodName(transferID), metav1.GetOptions{}); errors.IsNotFound(err) {
		receiverAbsent = true
	}
	_ = h.deleteImportResources(ctx, ns, transferID, !receiverAbsent)
}

// annotateHeldService records on a StatefulSet why it was left stopped after a
// failed transfer and what to scale it back to. The transfer's own Secret,
// Service, and Pod carried that information and are about to be deleted, so
// without this an operator finds a stopped database with no explanation.
func (h *Handler) annotateHeldService(ctx context.Context, ns, name, transferID, replicas string) error {
	sts, err := h.Client.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if sts.Annotations == nil {
		sts.Annotations = map[string]string{}
	}
	if replicas == "" {
		replicas = "1"
	}
	sts.Annotations["kipper.run/held-down-reason"] = "a data transfer failed while writing to this volume, so it may hold a partly-replaced copy; check the data before starting it"
	sts.Annotations["kipper.run/held-down-transfer"] = transferID
	sts.Annotations["kipper.run/held-down-restore-replicas"] = replicas
	_, err = h.Client.AppsV1().StatefulSets(ns).Update(ctx, sts, metav1.UpdateOptions{})
	return err
}

// reapSessionTransfers tears down every import receiver a session created,
// restarting the services they paused. Used when a migration is aborted on
// the target so recovery does not wait for the per-receiver lease.
func (h *Handler) reapSessionTransfers(ctx context.Context, sessionID string) {
	svcs, err := h.Client.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: "kipper.run/migration-session=" + sessionID + "," + transferIDLabel,
	})
	if err != nil {
		return
	}
	for i := range svcs.Items {
		svc := &svcs.Items[i]
		_ = h.deleteImportResources(ctx, svc.Namespace, svc.Labels[transferIDLabel], true)
	}
}

// RunTransferLeaseSweeper reaps expired transfer receivers at start and
// then periodically until the context ends, so an interrupted migration's
// receivers and paused services recover even if no other event ever fires.
func (h *Handler) RunTransferLeaseSweeper(ctx context.Context, interval time.Duration) {
	h.reapExpiredTransfers(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.reapExpiredTransfers(ctx)
		}
	}
}

// reapExpiredTransfers deletes import-side resources whose lease expired.
// It sweeps Secrets (created first, deleted last) rather than pods, so even
// the most partial setup is discovered.
func (h *Handler) reapExpiredTransfers(ctx context.Context) {
	secrets, err := h.Client.CoreV1().Secrets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: transferLeaseLabel,
	})
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		// Reap on either trigger: the lease expired, or the owning session
		// is already terminal (aborted/failed). The latter bounds recovery
		// to one sweep interval when an abort's own teardown failed to
		// restart a paused service, instead of waiting out the 24h lease.
		expiry, err := strconv.ParseInt(secret.Labels[transferLeaseLabel], 10, 64)
		leaseExpired := err == nil && expiry <= now
		if !leaseExpired && !h.sessionTerminal(secret.Labels["kipper.run/migration-session"]) {
			continue
		}
		_ = h.deleteImportResources(ctx, secret.Namespace, secret.Labels[transferIDLabel], true)
	}
}

// sessionTerminal reports whether a migration session is known and no longer
// active, so its leftover receivers should be reaped promptly.
func (h *Handler) sessionTerminal(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	session, ok := h.Sessions.Get(sessionID)
	if !ok {
		return false
	}
	switch session.CurrentStatus() {
	case SessionCancelled, SessionFailed, SessionCompleted:
		return true
	default:
		return false
	}
}

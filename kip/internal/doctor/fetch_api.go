package doctor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

var (
	tlsStoreGVR        = schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "tlsstores"}
	clusterIdentityGVR = schema.GroupVersionResource{Group: "kipper.run", Version: "v1alpha1", Resource: "clusteridentities"}
	platformConfigGVR  = schema.GroupVersionResource{Group: "kipper.run", Version: "v1alpha1", Resource: "platformconfigs"}

	errSecretsNeedSSH = errors.New("the hop certificate is only read over SSH, where a single public field can be read. " +
		"Through the API, the hop Secret comes with its private key")
)

// FetchViaAPI reads a Snapshot through the Kubernetes API, for an operator who
// cannot reach the server over SSH. It never reads a Secret, so the hop
// certificate is reported as unread. Other read failures are recorded in Missing
// while the remaining reads continue. Heartbeat contains at most 20 lines from
// the last 48 hours, oldest first, from the pod with the newest heartbeat.
func FetchViaAPI(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface) Snapshot {
	snap := Snapshot{Workloads: map[string]*appsv1.Deployment{}, Missing: map[Part]error{PartHopCert: errSecretsNeedSSH}}

	if nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err != nil {
		snap.Missing[PartNodes] = err
	} else {
		snap.Nodes = nodes.Items
	}

	for _, w := range CoreWorkloads {
		d, err := cs.AppsV1().Deployments(w.Namespace).Get(ctx, w.Name, metav1.GetOptions{})
		switch {
		case err == nil:
			snap.Workloads[w.Key()] = d
		case !isNotFound(err):
			snap.Missing[PartWorkloads] = err
		}
	}

	if stores, err := dyn.Resource(tlsStoreGVR).Namespace("").List(ctx, metav1.ListOptions{}); err != nil {
		snap.Missing[PartTLSStores] = err
	} else {
		for _, s := range stores.Items {
			secret, _, _ := unstructured.NestedString(s.Object, "spec", "defaultCertificate", "secretName")
			snap.TLSStores = append(snap.TLSStores, TLSStore{Namespace: s.GetNamespace(), Name: s.GetName(), DefaultCert: secret})
		}
	}

	if host, err := nestedField(ctx, dyn, clusterIdentityGVR, "cluster", "spec", "gateway", "clusterHost"); err != nil {
		snap.Missing[PartClusterHost] = err
	} else {
		snap.ClusterHost = host
	}

	if profile, err := nestedField(ctx, dyn, platformConfigGVR, "platform", "spec", "profile"); err != nil {
		snap.Missing[PartProfile] = err
	} else {
		snap.Profile = profile
	}

	if lines, err := heartbeatLines(ctx, cs); err != nil {
		snap.Missing[PartHeartbeat] = err
	} else {
		snap.Heartbeat = lines
	}
	return snap
}

func nestedField(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, name string, fields ...string) (string, error) {
	obj, err := dyn.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	if isNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	value, _, _ := unstructured.NestedString(obj.Object, fields...)
	return value, nil
}

// heartbeatLines returns the newest gateway heartbeat lines from the
// console-api pods' logs over the last 48 hours.
func heartbeatLines(ctx context.Context, cs kubernetes.Interface) ([]string, error) {
	d, err := cs.AppsV1().Deployments(hopNamespace).Get(ctx, "console-api", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return nil, err
	}
	pods, err := cs.CoreV1().Pods(hopNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	since := int64((48 * time.Hour).Seconds())
	byPod := map[string][]string{}
	for _, p := range pods.Items {
		raw, err := cs.CoreV1().Pods(hopNamespace).GetLogs(p.Name, &corev1.PodLogOptions{SinceSeconds: &since, Timestamps: true}).DoRaw(ctx)
		if err != nil {
			return nil, fmt.Errorf("reading the logs of %s: %w", p.Name, err)
		}
		scanner := bufio.NewScanner(strings.NewReader(string(raw)))
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			if line := scanner.Text(); strings.Contains(line, "gateway heartbeat") {
				byPod[p.Name] = append(byPod[p.Name], line)
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("reading the logs of %s: %w", p.Name, err)
		}
	}
	lines := newestPodLines(byPod)
	return lines, nil
}

// newestPodLines returns the heartbeat lines of the pod that logged the newest
// one. During a rollout two pods can run beats at once, and merging their
// lines would split one pod's beat with the other's outcome lines.
func newestPodLines(byPod map[string][]string) []string {
	var newest []string
	var newestAt time.Time
	for _, lines := range byPod {
		sorted := newestHeartbeatLines(lines)
		if len(sorted) == 0 {
			continue
		}
		if at, _ := lineTime(sorted[len(sorted)-1]); newest == nil || at.After(newestAt) {
			newest, newestAt = sorted, at
		}
	}
	return newest
}

func isNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

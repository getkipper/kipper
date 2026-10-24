package controller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/controller/pkg/stranded"
)

var errNoLonghornClient = errors.New("no client for Longhorn replicas")

// nodeDownReason is the text of a node alert. For a node with Kipper
// services whose single-node volumes are on it, it names each service, the
// nodes holding a current copy of its data, and kip node fence.
func (rc *ResourceController) nodeDownReason(ctx context.Context, node *corev1.Node) string {
	if stranded.OutOfService(node) {
		return fmt.Sprintf("node %q is fenced and out of service. Rejoin it with 'kip node add <host>', or remove it with 'kip node remove %s --force'", node.Name, node.Name)
	}
	reason := fmt.Sprintf("node %q is not Ready: workloads on it may be disrupted", node.Name)
	services, err := rc.strandedOn(ctx, node.Name)
	if err != nil {
		log.Printf("resource controller: listing the services on node %s: %v", node.Name, err)
		return reason
	}
	if len(services) == 0 {
		return reason
	}
	replicas, err := rc.listReplicas(ctx)
	if err != nil && !meta.IsNoMatchError(err) && !errors.Is(err, errNoLonghornClient) {
		log.Printf("resource controller: reading Longhorn replicas for the alert on node %s: %v", node.Name, err)
	}
	var parts []string
	for _, s := range services {
		part := s.label
		if err == nil {
			if copies := stranded.CopiesOfAll(s.volumes, node.Name, replicas); len(copies) > 0 {
				part += ", current copy on " + strings.Join(copies, ", ")
			} else {
				part += ", no other node holds a current copy"
			}
		}
		parts = append(parts, part)
	}
	return fmt.Sprintf("%s. Kipper services with data on it: %s. If the node is down for good, run 'kip node fence %s'", reason, strings.Join(parts, "; "), node.Name)
}

type strandedService struct {
	label   string
	volumes []string
}

// strandedOn lists the Kipper services with a single-node Longhorn volume on
// node, in the order stranded.Find returns their pods.
func (rc *ResourceController) strandedOn(ctx context.Context, node string) ([]strandedService, error) {
	if rc.client == nil {
		return nil, nil
	}
	pods, err := rc.client.CoreV1().Pods("").List(ctx, metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	claims, err := rc.client.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	pvs, err := rc.client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []strandedService
	index := map[string]int{}
	for _, s := range stranded.Find(node, pods.Items, claims.Items, pvs.Items) {
		if !s.ReadWriteOnce {
			continue
		}
		label := s.Label()
		i, ok := index[label]
		if !ok {
			i = len(out)
			index[label] = i
			out = append(out, strandedService{label: label})
		}
		out[i].volumes = append(out[i].volumes, s.Volume)
	}
	return out, nil
}

// longhornReplicas lists Longhorn's replicas through the API. A cluster
// without Longhorn returns a no-match error.
func (rc *ResourceController) longhornReplicas(ctx context.Context) ([]stranded.Replica, error) {
	if rc.crClient == nil {
		return nil, errNoLonghornClient
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "longhorn.io", Version: "v1beta2", Kind: "ReplicaList"})
	if err := rc.crClient.List(ctx, list, crclient.InNamespace("longhorn-system")); err != nil {
		return nil, err
	}
	data, err := list.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return stranded.ReplicasFromJSON(data)
}

package service

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/getkipper/kipper/controller/pkg/longhorncap"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

// kubeNodeGVR reads Kubernetes nodes through the dynamic client, which every
// caller of ReplicaNodes already has.
var kubeNodeGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}

// ReplicaNodes counts the nodes that can take a copy now, as the controller
// counts them: tagged platform.ReplicaStorageTag, scheduling allowed, Ready
// and Schedulable in Longhorn, and a Kubernetes node that is Ready and not
// fenced. Read failures, including a cluster without Longhorn, return an error.
func (m *Manager) ReplicaNodes(ctx context.Context) (int, error) {
	list, err := m.Dynamic.Resource(longhornNodeGVR).Namespace("longhorn-system").List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("listing Longhorn nodes: %w", err)
	}
	kubeNodes, err := m.Dynamic.Resource(kubeNodeGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("listing nodes: %w", err)
	}
	usable := map[string]bool{}
	for _, n := range kubeNodes.Items {
		var node corev1.Node
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(n.Object, &node); err != nil {
			return 0, fmt.Errorf("reading node %s: %w", n.GetName(), err)
		}
		usable[n.GetName()] = platform.NodeAvailable(&node)
	}
	count := 0
	for _, n := range list.Items {
		allow, _, _ := unstructured.NestedBool(n.Object, "spec", "allowScheduling")
		tags, _, _ := unstructured.NestedStringSlice(n.Object, "spec", "tags")
		conditions, _, _ := unstructured.NestedSlice(n.Object, "status", "conditions")
		if allow && slices.Contains(tags, platform.ReplicaStorageTag) &&
			longhorncap.ConditionTrue(conditions, "Ready") && longhorncap.ConditionTrue(conditions, "Schedulable") && usable[n.GetName()] {
			count++
		}
	}
	return count, nil
}

// Copies is an object's storage copies as its spec and status describe them.
type Copies struct {
	Wanted    *int64
	Live      int64
	LiveKnown bool
	Status    string
	Reason    string
	Message   string
}

// CopiesOf reads spec.copies, status.storage and the StorageCopies condition
// from an object. Missing or incorrectly typed fields retain their zero values.
// For Projects, it reads the condition only because their copy setting is nested
// under spec.sharedStorage. If conditions repeat, the last matching entry wins.
func CopiesOf(obj map[string]interface{}) Copies {
	var c Copies
	if v, found, _ := unstructured.NestedInt64(obj, "spec", "copies"); found {
		c.Wanted = &v
	}
	c.Live, c.LiveKnown, _ = unstructured.NestedInt64(obj, "status", "storage", "copies")
	conditions, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	for _, entry := range conditions {
		condition, _ := entry.(map[string]interface{})
		if condition["type"] == "StorageCopies" {
			c.Status, _ = condition["status"].(string)
			c.Reason, _ = condition["reason"].(string)
			c.Message, _ = condition["message"].(string)
		}
	}
	return c
}

// Summary returns the requested copy count, "not set" for unmanaged storage,
// or an empty string when no copy information is present. A requested count
// without a known live count includes "not created yet". With a known live
// count, a running or pending conversion includes "converting", and a blocked
// conversion includes "blocked". Data location is reported separately by the
// placement view.
func (c Copies) Summary() string {
	switch {
	case c.Wanted == nil && !c.LiveKnown && c.Status == "":
		return ""
	case c.Wanted == nil:
		return "not set"
	case !c.LiveKnown:
		return fmt.Sprintf("%d, not created yet", *c.Wanted)
	case c.Converting():
		return fmt.Sprintf("%d, converting", *c.Wanted)
	case c.Reason == "Blocked":
		return fmt.Sprintf("%d, blocked", *c.Wanted)
	}
	return strconv.FormatInt(*c.Wanted, 10)
}

// Converting reports whether the copy count change is running or pending.
func (c Copies) Converting() bool {
	return c.Reason == "Converting" || c.Reason == "ConversionPending"
}

// Problem returns the controller's message unless the condition is True.
// A missing message returns an empty string.
func (c Copies) Problem() string {
	if c.Status == "True" {
		return ""
	}
	return c.Message
}

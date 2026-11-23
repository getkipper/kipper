package installer

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// podTemplate returns a kubectl JSONPath template that prints only the pod
// fields kip's capacity, placement, fence, and copy checks read. Each pod has a
// P line, followed by an I line per init container, a C line per container, an
// S line per container status, and a V line per volume. Environment variables,
// commands, arguments, annotations, and container termination messages stay on
// the server. Only Ready and PodScheduled conditions are printed because other
// conditions can contain free-text messages from any controller. Pod status
// messages stay on the server because they can contain line breaks. Volume
// sources include only PVC claim names.
// Each line ends with the supplied marker so the parser can detect tabs or
// newlines in unescaped strings, such as images, that could mimic output lines.
func podTemplate(end string) string {
	tail := `{"\t"}` + end + `{"\n"}`
	return `{range .items[*]}` +
		`P{"\t"}{.metadata.uid}{"\t"}{.metadata.name}{"\t"}{.metadata.namespace}{"\t"}{.metadata.deletionTimestamp}` +
		`{"\t"}{.metadata.labels}{"\t"}{.metadata.ownerReferences}{"\t"}{.spec.nodeName}{"\t"}{.spec.priorityClassName}` +
		`{"\t"}{.spec.priority}{"\t"}{.spec.nodeSelector}{"\t"}{.spec.affinity}{"\t"}{.spec.tolerations}` +
		`{"\t"}{.spec.overhead}{"\t"}{.spec.resources}{"\t"}{.spec.activeDeadlineSeconds}` +
		`{"\t"}{.status.phase}{"\t"}{.status.conditions[?(@.type=="Ready")]}{"\t"}{.status.conditions[?(@.type=="PodScheduled")]}` +
		`{"\t"}{.status.reason}` + tail +
		`{range .spec.initContainers[*]}I{"\t"}{.name}{"\t"}{.restartPolicy}{"\t"}{.resources}` + tail + `{end}` +
		`{range .spec.containers[*]}C{"\t"}{.name}{"\t"}{.image}{"\t"}{.resources}` + tail + `{end}` +
		`{range .status.containerStatuses[*]}S{"\t"}{.name}{"\t"}{.state.terminated.reason}{"\t"}{.state.terminated.exitCode}` + tail + `{end}` +
		`{range .spec.volumes[*]}V{"\t"}{.name}{"\t"}{.persistentVolumeClaim.claimName}` + tail + `{end}` +
		`{end}`
}

// podLineFields maps each tag to its number of tab-separated fields,
// including the end marker.
var podLineFields = map[string]int{"P": 20, "I": 4, "C": 4, "S": 4, "V": 3}

// newReadMarker generates an unpredictable end marker for each projected read
// so object values cannot reliably forge output lines.
var newReadMarker = randomReadMarker

func randomReadMarker() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("reading random bytes: %v", err))
	}
	return "end-" + hex.EncodeToString(b)
}

// readPods lists the pods that args select, such as "-A" or
// "-n shop --field-selector spec.nodeName=server-1", with only the fields in
// podTemplate. Read failures keep kubectl's output out of the error.
// A selector that matches nothing returns no pods and no error.
func readPods(runner CommandRunner, what, args string) ([]corev1.Pod, error) {
	end := newReadMarker()
	out, err := runner.Run(podsCommand(args, end))
	if err != nil {
		return nil, failedRead("reading "+what, out, err)
	}
	pods, err := parsePods(out, end)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	return pods, nil
}

func podsCommand(args, end string) string {
	return projectedGetCommand("pods", args, podTemplate(end))
}

// projectedGetCommand builds a kubectl get command with a JSONPath template.
// It captures stderr in a temporary file and prints it only on failure to keep
// warnings out of successful reads when the SSH runner merges stdout and stderr.
func projectedGetCommand(resource, args, template string) string {
	return fmt.Sprintf(`e=$(mktemp) || exit 1; kubectl get %s %s -o jsonpath='%s' 2>"$e"; rc=$?; [ "$rc" = 0 ] || cat "$e"; rm -f "$e"; exit "$rc"`,
		resource, args, template)
}

// parsePods rebuilds pods from podTemplate output using the supplied end marker.
// Every nonempty line must have a known tag, the expected field count, and the
// end marker. Invalid records fail the read, with the pod identified when possible.
func parsePods(out, end string) ([]corev1.Pod, error) {
	var pods []corev1.Pod
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		tag, rest, _ := strings.Cut(line, "\t")
		want, known := podLineFields[tag]
		f := strings.Split(rest, "\t")
		if !known || len(f) != want || f[want-1] != end {
			return nil, unreadablePodLine(pods, tag, known, f)
		}
		f = f[:want-1]
		if tag == "P" {
			pod, err := parsePodLine(f)
			if err != nil {
				return nil, err
			}
			pods = append(pods, pod)
			continue
		}
		if len(pods) == 0 {
			return nil, errors.New("a container or volume line came before any pod")
		}
		if err := addPodPart(&pods[len(pods)-1], tag, f); err != nil {
			return nil, fmt.Errorf("pod %s/%s: %w", pods[len(pods)-1].Namespace, pods[len(pods)-1].Name, err)
		}
	}
	return pods, nil
}

// unreadablePodLine reports a malformed record, identifying the pod from a
// P line's name and namespace when they pass isObjectName, or from the preceding
// pod for a recognized container, status, or volume tag. Otherwise, it returns
// a generic error.
func unreadablePodLine(pods []corev1.Pod, tag string, known bool, f []string) error {
	switch {
	case tag == "P" && len(f) >= 3 && isObjectName(f[1]) && isObjectName(f[2]):
		return fmt.Errorf("pod %s/%s has a tab or newline in a printed value, such as its status reason", f[2], f[1])
	case known && tag != "P" && len(pods) > 0:
		last := pods[len(pods)-1]
		return fmt.Errorf("pod %s/%s has a tab or newline in a printed value, such as its image", last.Namespace, last.Name)
	}
	return errors.New("the pod read printed a line that is not a pod record")
}

// isObjectName limits names quoted in errors to 1 to 253 bytes of lowercase ASCII
// letters, digits, hyphens, and periods. It checks length and characters only,
// not the full Kubernetes naming rules.
func isObjectName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

func parsePodLine(f []string) (corev1.Pod, error) {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID(f[0]), Name: f[1], Namespace: f[2]}}
	pod.Spec.NodeName, pod.Spec.PriorityClassName = f[6], f[7]
	pod.Status.Phase, pod.Status.Reason = corev1.PodPhase(f[15]), f[18]
	if f[3] != "" {
		var t metav1.Time
		if err := t.UnmarshalQueryParameter(f[3]); err != nil {
			return corev1.Pod{}, fmt.Errorf("pod %s/%s deletion time: %w", f[2], f[1], err)
		}
		pod.DeletionTimestamp = &t
	}
	for _, v := range []struct {
		raw  string
		into any
	}{
		{f[4], &pod.Labels},
		{f[5], &pod.OwnerReferences},
		{f[8], &pod.Spec.Priority},
		{f[9], &pod.Spec.NodeSelector},
		{f[10], &pod.Spec.Affinity},
		{f[11], &pod.Spec.Tolerations},
		{f[12], &pod.Spec.Overhead},
		{f[13], &pod.Spec.Resources},
		{f[14], &pod.Spec.ActiveDeadlineSeconds},
	} {
		if err := unmarshalIfSet(v.raw, v.into); err != nil {
			return corev1.Pod{}, fmt.Errorf("pod %s/%s: %w", f[2], f[1], err)
		}
	}
	for _, raw := range f[16:18] {
		if raw == "" {
			continue
		}
		var c corev1.PodCondition
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return corev1.Pod{}, fmt.Errorf("pod %s/%s condition: %w", f[2], f[1], err)
		}
		pod.Status.Conditions = append(pod.Status.Conditions, c)
	}
	return pod, nil
}

func addPodPart(pod *corev1.Pod, tag string, f []string) error {
	switch tag {
	case "I":
		c := corev1.Container{Name: f[0]}
		if f[1] != "" {
			policy := corev1.ContainerRestartPolicy(f[1])
			c.RestartPolicy = &policy
		}
		if err := unmarshalIfSet(f[2], &c.Resources); err != nil {
			return err
		}
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, c)
	case "C":
		c := corev1.Container{Name: f[0], Image: f[1]}
		if err := unmarshalIfSet(f[2], &c.Resources); err != nil {
			return err
		}
		pod.Spec.Containers = append(pod.Spec.Containers, c)
	case "S":
		status := corev1.ContainerStatus{Name: f[0]}
		if f[1] != "" || f[2] != "" {
			status.State.Terminated = &corev1.ContainerStateTerminated{Reason: f[1]}
			if err := unmarshalIfSet(f[2], &status.State.Terminated.ExitCode); err != nil {
				return err
			}
		}
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, status)
	case "V":
		v := corev1.Volume{Name: f[0]}
		if f[1] != "" {
			v.PersistentVolumeClaim = &corev1.PersistentVolumeClaimVolumeSource{ClaimName: f[1]}
		}
		pod.Spec.Volumes = append(pod.Spec.Volumes, v)
	}
	return nil
}

func unmarshalIfSet(raw string, into any) error {
	if raw == "" {
		return nil
	}
	return json.Unmarshal([]byte(raw), into)
}

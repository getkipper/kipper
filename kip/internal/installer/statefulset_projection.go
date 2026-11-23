package installer

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

// statefulSetAnnotations are the only StatefulSet annotations kip's fence and
// storage move read.
var statefulSetAnnotations = []string{FenceStopAnnotation, platform.StorageMoveAnnotation, platform.StorageMoveReplicasAnnotation}

// statefulSetTemplate returns a kubectl JSONPath template that prints one T
// line per StatefulSet with its namespace, name, resource version, replicas,
// the annotations in statefulSetAnnotations and its volume claim template
// names. Remote kubectl omits the pod template, including its environment and
// commands, and all other annotations from its output. Each line ends with the
// supplied marker.
func statefulSetTemplate(end string) string {
	t := `{range .items[*]}T{"\t"}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.metadata.resourceVersion}{"\t"}{.spec.replicas}`
	for _, key := range statefulSetAnnotations {
		t += `{"\t"}{.metadata.annotations.` + strings.ReplaceAll(key, ".", `\.`) + `}`
	}
	return t + `{"\t"}{.spec.volumeClaimTemplates[*].metadata.name}{"\t"}` + end + `{"\n"}{end}`
}

// statefulSetLineFields is the number of tab-separated fields after the T tag,
// including the end marker.
var statefulSetLineFields = 6 + len(statefulSetAnnotations)

// readStatefulSets lists the StatefulSets that args select, such as "-A" or
// "-n shop --field-selector metadata.name=db", with only the fields in
// statefulSetTemplate. A selector that matches nothing returns nil and no
// error. Command failures use failedRead to protect sensitive output.
func readStatefulSets(runner CommandRunner, what, args string) ([]appsv1.StatefulSet, error) {
	end := newReadMarker()
	out, err := runner.Run(statefulSetsCommand(args, end))
	if err != nil {
		return nil, failedRead("reading "+what, out, err)
	}
	sets, err := parseStatefulSets(out, end)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	return sets, nil
}

// readOneStatefulSet reads the named StatefulSet through readStatefulSets. A
// missing StatefulSet or namespace returns nil and no error.
func readOneStatefulSet(runner CommandRunner, namespace, name string) (*appsv1.StatefulSet, error) {
	sets, err := readStatefulSets(runner, "StatefulSet "+namespace+"/"+name,
		fmt.Sprintf("-n %s --field-selector metadata.name=%s", shellQuote(namespace), shellQuote(name)))
	if err != nil || len(sets) == 0 {
		return nil, err
	}
	return &sets[0], nil
}

func statefulSetsCommand(args, end string) string {
	return projectedGetCommand("statefulsets", args, statefulSetTemplate(end))
}

// parseStatefulSets rebuilds StatefulSets from statefulSetTemplate output.
// Every nonempty line must be a T record with the expected field count and end
// marker. Invalid records fail the read and identify the StatefulSet when possible.
// Empty annotations are treated as absent.
func parseStatefulSets(out, end string) ([]appsv1.StatefulSet, error) {
	var sets []appsv1.StatefulSet
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		tag, rest, _ := strings.Cut(line, "\t")
		f := strings.Split(rest, "\t")
		if tag != "T" || len(f) != statefulSetLineFields || f[len(f)-1] != end {
			if tag == "T" && len(f) >= 2 && isObjectName(f[0]) && isObjectName(f[1]) {
				return nil, fmt.Errorf("StatefulSet %s/%s has a tab or newline in a printed value, such as an annotation", f[0], f[1])
			}
			return nil, errors.New("the StatefulSet read printed a line that is not a StatefulSet record")
		}
		s := appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: f[0], Name: f[1], ResourceVersion: f[2]}}
		if f[3] != "" {
			n, err := strconv.ParseInt(f[3], 10, 32)
			if err != nil {
				return nil, fmt.Errorf("StatefulSet %s/%s: reading replicas %q: %w", f[0], f[1], f[3], err)
			}
			replicas := int32(n)
			s.Spec.Replicas = &replicas
		}
		for i, key := range statefulSetAnnotations {
			if v := f[4+i]; v != "" {
				if s.Annotations == nil {
					s.Annotations = map[string]string{}
				}
				s.Annotations[key] = v
			}
		}
		for _, name := range strings.Fields(f[4+len(statefulSetAnnotations)]) {
			s.Spec.VolumeClaimTemplates = append(s.Spec.VolumeClaimTemplates, corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name}})
		}
		sets = append(sets, s)
	}
	return sets, nil
}

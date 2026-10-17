package installer

import (
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// storageSizeQuery builds a command to read a claim's request and capacity.
// The marker separates sizes from SSH output. A missing claim produces no output.
func storageSizeQuery(namespace, claim, marker string) string {
	return fmt.Sprintf(`kubectl -n %s get pvc %s --ignore-not-found `+
		`-o jsonpath='{"%s["}{.spec.resources.requests.storage}{"|"}{.status.capacity.storage}{"]"}' 2>/dev/null`,
		namespace, claim, marker)
}

// preservedStorageRequest chooses the largest of the default size, the current
// request and the bound capacity, so a re-applied manifest never asks a claim
// to shrink. Output without the marker uses the default. Otherwise, the last
// complete record must contain a valid request and an optional valid capacity.
// A missing record or invalid selected quantity returns an error.
// defaultSize must be a valid quantity.
func preservedStorageRequest(out, marker, defaultSize string) (string, error) {
	if !strings.Contains(out, marker+"[") {
		return defaultSize, nil
	}
	payload := regexp.MustCompile(regexp.QuoteMeta(marker) + `\[([^|\]]*)\|([^|\]]*)\]`)
	matches := payload.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		return "", fmt.Errorf("could not read the volume size from %q", out)
	}
	request, capacity := matches[len(matches)-1][1], matches[len(matches)-1][2]
	if request == "" {
		return "", fmt.Errorf("the volume has no requested size")
	}
	size := resource.MustParse(defaultSize)
	for _, f := range []string{request, capacity} {
		if f == "" {
			continue
		}
		q, err := resource.ParseQuantity(f)
		if err != nil {
			return "", fmt.Errorf("reading the volume size %q: %w", f, err)
		}
		if q.Cmp(size) > 0 {
			size = q
		}
	}
	return size.String(), nil
}

// longhornVolumeDiagnosisQuery builds a command to read the name, robustness
// and Scheduled condition of a claim's Longhorn volume. A claim without a
// volume produces no output.
func longhornVolumeDiagnosisQuery(namespace, claim string) string {
	return fmt.Sprintf(`v=$(kubectl -n %s get pvc %s -o jsonpath='{.spec.volumeName}' 2>/dev/null); `+
		`[ -n "$v" ] && kubectl -n longhorn-system get volumes.longhorn.io "$v" --request-timeout=30s `+
		`-o jsonpath='{"lh-volume["}{.metadata.name}{"|"}{.status.robustness}{"|"}{.status.conditions[?(@.type=="Scheduled")].status}{"|"}{.status.conditions[?(@.type=="Scheduled")].message}{"]"}' 2>/dev/null; true`,
		namespace, claim)
}

// longhornVolumeDiagnosis returns storage details to help explain a rollout
// failure. Lookup failures return an empty string.
func longhornVolumeDiagnosis(client CommandRunner, namespace, claim string) string {
	out, err := client.Run(longhornVolumeDiagnosisQuery(namespace, claim))
	if err != nil {
		return ""
	}
	return formatLonghornVolumeDiagnosis(out)
}

var longhornVolumePayload = regexp.MustCompile(`lh-volume\[([^|\]]*)\|([^|\]]*)\|([^|\]]*)\|([^\]]*)\]`)

// formatLonghornVolumeDiagnosis turns the diagnosis query's output into a
// parenthetical for an error message, empty when there is no volume record.
func formatLonghornVolumeDiagnosis(out string) string {
	m := longhornVolumePayload.FindStringSubmatch(out)
	if len(m) < 5 || m[1] == "" {
		return ""
	}
	name, robustness, scheduled, message := m[1], m[2], m[3], m[4]
	if robustness == "" {
		robustness = "unknown"
	}
	if scheduled == "True" {
		return fmt.Sprintf(" (Longhorn volume %s is %s and scheduled)", name, robustness)
	}
	if message == "" {
		message = "no reason given"
	}
	return fmt.Sprintf(" (Longhorn volume %s (%s) is not scheduled: %s. Check the reported condition and available disk space)",
		name, robustness, message)
}

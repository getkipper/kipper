package installer

import (
	"fmt"
	"regexp"
	"strings"
)

// LonghornHopRecordName names the ConfigMap that records a Longhorn hop from
// before its first change until it has settled.
const LonghornHopRecordName = "kipper-longhorn-upgrade"

// LonghornHopRecord is the durable intent of one Longhorn upgrade step. From
// and To are equal for an engine catch-up at the applied release.
type LonghornHopRecord struct {
	From    string
	To      string
	Started string
	Backup  string
	Export  string
}

// LonghornMarkers are what an existing cluster says about its Longhorn version:
// Record is the unfinished hop, if any. Applied is the image tag in the
// longhorn-manager DaemonSet's pod template. Running is the
// current-longhorn-version setting, the release longhorn-manager finished
// upgrading to. CRDs reports whether any Longhorn CRD exists.
type LonghornMarkers struct {
	Record  *LonghornHopRecord
	Applied string
	Running string
	CRDs    bool
}

const (
	longhornRecordQuery  = `kubectl -n ` + NodeRecordsNamespace + ` get configmap ` + LonghornHopRecordName + ` --ignore-not-found --request-timeout=30s -o jsonpath='{.data.from}{"\t"}{.data.to}{"\t"}{.data.started}{"\t"}{.data.backup}{"\t"}{.data.export}'`
	longhornImageQuery   = `kubectl -n longhorn-system get daemonset longhorn-manager --ignore-not-found --request-timeout=30s -o jsonpath='{.spec.template.spec.containers[?(@.name=="longhorn-manager")].image}'`
	longhornRunningQuery = `kubectl -n longhorn-system get settings.longhorn.io current-longhorn-version --ignore-not-found --request-timeout=30s -o jsonpath='{.value}'`
	longhornCRDQuery     = `kubectl get crd --request-timeout=30s -o name`
	longhornSettingsCRD  = "customresourcedefinition.apiextensions.k8s.io/settings.longhorn.io"
)

var managerImagePattern = regexp.MustCompile(`(?:^|/)longhorn-manager:(v[0-9][^\s/]*)$`)

// ReadLonghornMarkers reads the hop record, the applied and running Longhorn
// versions and whether Longhorn CRDs exist, over kubectl on the server. The
// running version is read only once Longhorn's settings CRD exists, since
// kubectl fails on a resource type the API does not know. A read that
// fails or returns an unrecognisable manager image is an error. Version
// validation is left to LonghornVersionToApply.
func ReadLonghornMarkers(client CommandRunner) (LonghornMarkers, error) {
	var m LonghornMarkers
	out, err := client.Run(longhornRecordQuery)
	if err != nil {
		return m, fmt.Errorf("reading the Longhorn hop record: %w", err)
	}
	if line := kubectlLine(out); line != "" {
		f := strings.Split(line, "\t")
		for len(f) < 5 {
			f = append(f, "")
		}
		m.Record = &LonghornHopRecord{From: f[0], To: f[1], Started: f[2], Backup: f[3], Export: f[4]}
	}
	out, err = client.Run(longhornCRDQuery)
	if err != nil {
		return m, fmt.Errorf("checking for Longhorn's CRDs: %w", err)
	}
	settingsCRD := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		m.CRDs = m.CRDs || strings.HasSuffix(line, ".longhorn.io")
		settingsCRD = settingsCRD || line == longhornSettingsCRD
	}
	out, err = client.Run(longhornImageQuery)
	if err != nil {
		return m, fmt.Errorf("reading the longhorn-manager image: %w", err)
	}
	if line := kubectlLine(out); line != "" {
		if m.Applied, err = LonghornManagerVersion(line); err != nil {
			return m, err
		}
	}
	if settingsCRD {
		out, err = client.Run(longhornRunningQuery)
		if err != nil {
			return m, fmt.Errorf("reading Longhorn's current-longhorn-version setting: %w", err)
		}
		m.Running = kubectlLine(out)
	}
	return m, nil
}

// LonghornManagerVersion returns the release tag of a longhorn-manager image,
// or an error if the image name or tag format is unrecognised. It extracts
// tags beginning with v and a digit without validating the release version.
func LonghornManagerVersion(image string) (string, error) {
	match := managerImagePattern.FindStringSubmatch(image)
	if match == nil {
		return "", fmt.Errorf("kip cannot read the Longhorn version from the longhorn-manager image %q", image)
	}
	return match[1], nil
}

// kubectlLine returns the last line of kubectl output that is not a warning,
// trimmed of surrounding spaces and carriage returns, preserving tabs. The
// SSH runner merges stderr into the output.
func kubectlLine(out string) string {
	last := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.Trim(line, " \r")
		if line == "" || strings.HasPrefix(line, "Warning:") {
			continue
		}
		last = line
	}
	if strings.Trim(last, "\t") == "" {
		return ""
	}
	return last
}

// HopStarted reports whether a recorded version change has reached the
// manager DaemonSet or running release. It returns false for a missing record,
// an engine catch-up, or a hop whose target appears in neither version marker.
func (m LonghornMarkers) HopStarted() bool {
	return m.Record != nil && m.Record.From != m.Record.To && (m.Applied == m.Record.To || m.Running == m.Record.To)
}

// LonghornVersionToApply returns the Longhorn release a re-apply must use so
// that it never skips a minor or moves a half-finished hop backwards: the
// hop record's To once the hop has started and its From before that, else the
// applied manifest, else the running release. With none of these markers and
// no Longhorn CRDs, it returns the target and fresh is true.
// It rejects invalid hop records, unreadable versions, versions newer than
// the target, Longhorn CRDs without any version, and a manager already past
// the version found.
func LonghornVersionToApply(m LonghornMarkers) (version string, fresh bool, err error) {
	switch {
	case m.Record != nil:
		if err := checkLonghornHopRecord(*m.Record, m.Applied, m.Running); err != nil {
			return "", false, fmt.Errorf("the Longhorn hop record %s/%s %w. kip does not act on it, and nothing was changed", NodeRecordsNamespace, LonghornHopRecordName, err)
		}
		version = m.Record.From
		if m.HopStarted() {
			version = m.Record.To
		}
	case m.Applied != "":
		version = m.Applied
	case m.Running != "":
		version = m.Running
	case m.CRDs:
		return "", false, fmt.Errorf("the Longhorn CRDs exist, but kip cannot tell which Longhorn version is installed: there is no longhorn-manager DaemonSet and no current-longhorn-version setting. Nothing was changed")
	default:
		return longhornVersion, true, nil
	}
	v, err := parseLonghornVersion(version)
	if err != nil {
		return "", false, err
	}
	target, _ := parseLonghornVersion(longhornVersion)
	if target.less(v) {
		return "", false, fmt.Errorf("this cluster has Longhorn %s, newer than the %s this kip installs. Nothing was changed. Upgrade kip to manage this cluster's Longhorn", version, longhornVersion)
	}
	if m.Running != "" {
		running, err := parseLonghornVersion(m.Running)
		if err != nil {
			return "", false, err
		}
		if v.less(running) {
			return "", false, fmt.Errorf("this cluster's Longhorn already runs %s, but kip found %s to apply. Applying it would downgrade Longhorn, which Longhorn does not support. Nothing was changed", m.Running, version)
		}
	}
	return version, false, nil
}

// checkLonghornHopRecord accepts an engine catch-up, a later patch of the
// same minor, or the next minor within the same major, up to the target.
// The record must name a backup and export, and each available version marker
// must match From or To. Patch releases may differ from this kip's path so a
// later kip can finish a step started by an earlier version.
func checkLonghornHopRecord(rec LonghornHopRecord, applied, running string) error {
	from, err := parseLonghornVersion(rec.From)
	if err != nil {
		return fmt.Errorf("names a step from %q to %q, and %w", rec.From, rec.To, err)
	}
	to, err := parseLonghornVersion(rec.To)
	if err != nil {
		return fmt.Errorf("names a step from %q to %q, and %w", rec.From, rec.To, err)
	}
	target, _ := parseLonghornVersion(longhornVersion)
	forward := to.major == from.major && (to.minor == from.minor+1 || (to.minor == from.minor && from.less(to)))
	if (rec.From != rec.To && !forward) || target.less(to) {
		return fmt.Errorf("names a step from %s to %s, which is not a step kip takes", rec.From, rec.To)
	}
	if rec.Backup == "" || rec.Export == "" {
		return fmt.Errorf("names a step from %s to %s without the backup and export kip records before every step", rec.From, rec.To)
	}
	for what, v := range map[string]string{"the longhorn-manager DaemonSet": applied, "Longhorn's current-longhorn-version setting": running} {
		if v != "" && v != rec.From && v != rec.To {
			return fmt.Errorf("names a step from %s to %s, but %s shows %s", rec.From, rec.To, what, v)
		}
	}
	return nil
}

// writeLonghornHopRecord creates or replaces the hop record.
func writeLonghornHopRecord(client CommandRunner, rec LonghornHopRecord) error {
	manifest := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
  namespace: %s
data:
  from: %q
  to: %q
  started: %q
  backup: %q
  export: %q
`, LonghornHopRecordName, NodeRecordsNamespace, rec.From, rec.To, rec.Started, rec.Backup, rec.Export)
	if _, err := client.RunStdin("kubectl apply -f - >/dev/null", strings.NewReader(manifest)); err != nil {
		return fmt.Errorf("recording the Longhorn hop to %s: %w", rec.To, err)
	}
	return nil
}

// deleteLonghornHopRecord removes a completed or superseded hop record.
func deleteLonghornHopRecord(client CommandRunner) error {
	if _, err := client.Run(fmt.Sprintf("kubectl -n %s delete configmap %s --ignore-not-found >/dev/null", NodeRecordsNamespace, LonghornHopRecordName)); err != nil {
		return fmt.Errorf("removing the finished Longhorn hop record: %w", err)
	}
	return nil
}

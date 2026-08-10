package migrate

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// DexNamespace and DexConfigMapName name the live Dex ConfigMap that
// holds the full `config.yaml` document. The CLI command and any future
// boot-time hook target this single resource.
const (
	DexNamespace     = "dex"
	DexConfigMapName = "dex-config"
)

// DexUserImportReport summarises what ImportDexUsers added to the live
// dex-config when merging a snapshot.
type DexUserImportReport struct {
	StaticPasswordsAdded []string // emails
	ConnectorsAdded      []string // ids
	AlreadyPresent       int      // entries that already existed and were left untouched
}

// HasChanges returns true when the migration actually added anything.
func (r DexUserImportReport) HasChanges() bool {
	return len(r.StaticPasswordsAdded)+len(r.ConnectorsAdded) > 0
}

// ImportDexUsers merges the snapshot's `staticPasswords` and
// `connectors` blocks into the live `dex/dex-config` ConfigMap. Returns
// the report so the caller can print it.
//
// Snapshot rules:
//   - liveConfigYAML is the YAML body the live ConfigMap stores under
//     `data["config.yaml"]`.
//   - snapshotConfigYAML is the same body from a captured snapshot.
//   - For staticPasswords, entries are keyed by `email`. Existing
//     emails on the live side stay untouched; new emails from the
//     snapshot are appended.
//   - For connectors, entries are keyed by `id`. Same merge rule.
//   - Every other field of the Dex config is left exactly as the live
//     side had it (issuer, staticClients, storage, oauth2, etc.).
//
// Returns the merged config YAML, the report, and any parse error.
func MergeDexConfig(liveConfigYAML, snapshotConfigYAML []byte) ([]byte, DexUserImportReport, error) {
	var report DexUserImportReport

	var live map[string]any
	if err := yaml.Unmarshal(liveConfigYAML, &live); err != nil {
		return nil, report, fmt.Errorf("parsing live dex-config: %w", err)
	}
	if live == nil {
		live = map[string]any{}
	}
	var snap map[string]any
	if err := yaml.Unmarshal(snapshotConfigYAML, &snap); err != nil {
		return nil, report, fmt.Errorf("parsing snapshot dex-config: %w", err)
	}

	// staticPasswords merge.
	livePws, _ := live["staticPasswords"].([]any)
	snapPws, _ := snap["staticPasswords"].([]any)
	merged, added, present := mergeListByKey(livePws, snapPws, "email")
	report.StaticPasswordsAdded = added
	report.AlreadyPresent += present
	live["staticPasswords"] = merged

	// connectors merge.
	liveConns, _ := live["connectors"].([]any)
	snapConns, _ := snap["connectors"].([]any)
	merged, added, present = mergeListByKey(liveConns, snapConns, "id")
	report.ConnectorsAdded = added
	report.AlreadyPresent += present
	live["connectors"] = merged

	out, err := yaml.Marshal(live)
	if err != nil {
		return nil, report, fmt.Errorf("re-encoding dex-config: %w", err)
	}
	return out, report, nil
}

// ImportDexUsers reads the live `dex/dex-config` ConfigMap, merges in
// the snapshot's staticPasswords and connectors, and writes the result
// back. Caller is responsible for restarting the Dex Deployment so the
// new config takes effect.
func ImportDexUsers(ctx context.Context, client kubernetes.Interface, snapshotConfigYAML []byte) (DexUserImportReport, error) {
	var report DexUserImportReport

	cm, err := client.CoreV1().ConfigMaps(DexNamespace).Get(ctx, DexConfigMapName, metav1.GetOptions{})
	if err != nil {
		return report, fmt.Errorf("reading live %s/%s: %w", DexNamespace, DexConfigMapName, err)
	}

	liveRaw, ok := cm.Data["config.yaml"]
	if !ok {
		return report, fmt.Errorf("live dex-config has no config.yaml key")
	}

	merged, report, err := MergeDexConfig([]byte(liveRaw), snapshotConfigYAML)
	if err != nil {
		return report, err
	}
	if !report.HasChanges() {
		return report, nil
	}

	cm.Data["config.yaml"] = string(merged)
	if _, err := client.CoreV1().ConfigMaps(DexNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return report, fmt.Errorf("updating dex-config: %w", err)
	}
	return report, nil
}

// ExtractDexConfigYAML pulls the inner `config.yaml` document out of a
// captured ConfigMap snapshot (whatever the operator passes as
// --file). Accepts two shapes:
//
//   - A full ConfigMap manifest with `data: { config.yaml: ... }`.
//   - The raw Dex config YAML directly (issuer:, staticPasswords:, ...).
//
// Returns the inner YAML as bytes. The shape detection is best-effort:
// if a top-level `data` key is present we use its config.yaml value,
// otherwise the input is treated as the Dex config itself.
func ExtractDexConfigYAML(raw []byte) ([]byte, error) {
	var probe map[string]any
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parsing snapshot: %w", err)
	}
	if data, ok := probe["data"].(map[string]any); ok {
		if cfg, ok := data["config.yaml"].(string); ok {
			return []byte(cfg), nil
		}
		return nil, fmt.Errorf("snapshot ConfigMap has no data.config.yaml")
	}
	// Treat as the inner config directly.
	return raw, nil
}

// mergeListByKey returns dst with every entry from src whose `key`
// value is not already present in dst appended. The boolean `key` is
// the map field used to identify an entry (e.g. "email" for
// staticPasswords, "id" for connectors). Entries that aren't maps are
// preserved verbatim from dst.
//
// Returns: the merged slice, the list of keys actually added (for the
// report), and the count of snapshot entries that already existed in
// dst.
func mergeListByKey(dst, src []any, key string) ([]any, []string, int) {
	seen := make(map[string]bool, len(dst))
	for _, item := range dst {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if v, ok := m[key].(string); ok {
			seen[v] = true
		}
	}

	merged := append([]any{}, dst...)
	var added []string
	alreadyPresent := 0
	for _, item := range src {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		v, ok := m[key].(string)
		if !ok {
			continue
		}
		if seen[v] {
			alreadyPresent++
			continue
		}
		seen[v] = true
		added = append(added, v)
		merged = append(merged, item)
	}
	return merged, added, alreadyPresent
}

// DexConfigMap is exposed so the CLI command can use it as a fake-client
// fixture in tests without re-declaring the namespace/name pair.
func DexConfigMap(configYAML string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DexConfigMapName,
			Namespace: DexNamespace,
		},
		Data: map[string]string{"config.yaml": configYAML},
	}
}

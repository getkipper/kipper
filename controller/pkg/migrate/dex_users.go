package migrate

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/getkipper/kipper/controller/pkg/dexsecrets"
)

// DexNamespace and DexConfigMapName name the live Dex ConfigMap that
// holds the full config.yaml document used by the CLI import.
const (
	DexNamespace     = dexsecrets.Namespace
	DexConfigMapName = dexsecrets.ConfigMapName
)

// DexUserImportReport summarizes a snapshot merge. Added static passwords are
// identified by email and connectors by ID. ConnectorsSkipped lists IDs with
// environment variable secret references missing from the import. AlreadyPresent
// counts eligible snapshot entries whose keys were already in the merged list
// and were left unchanged. ConnectorCallback is the
// new callback URL for added connectors whose redirectURI matched the snapshot
// issuer's callback, or empty when none changed.
type DexUserImportReport struct {
	StaticPasswordsAdded []string
	ConnectorsAdded      []string
	ConnectorsSkipped    []string
	AlreadyPresent       int
	ConnectorCallback    string
}

// HasChanges reports whether the merge added static passwords or connectors.
func (r DexUserImportReport) HasChanges() bool {
	return len(r.StaticPasswordsAdded)+len(r.ConnectorsAdded) > 0
}

// MergeDexConfig merges staticPasswords and connectors from a snapshot into the
// live Dex config and returns the merged YAML and an import report. Both inputs
// are config.yaml contents, as stored in a ConfigMap's data["config.yaml"].
//
// Static passwords are matched by email and connectors by ID. Existing entries
// are preserved, and new entries are appended in snapshot order. Snapshot
// connectors whose clientSecret starts with "$" are reported as skipped because
// their environment variable values, including secrets in the source cluster's
// dex-connector-secrets Secret, are outside this config snapshot.
// Added connectors are moved to the live issuer's callback as MergeDexBackup
// describes. Other live config fields (issuer, staticClients, storage, oauth2,
// etc.) retain their values. YAML parsing or encoding failures return an error.
func MergeDexConfig(liveConfigYAML, snapshotConfigYAML []byte) ([]byte, DexUserImportReport, error) {
	merged, report, _, err := MergeDexBackup(liveConfigYAML, snapshotConfigYAML, nil)
	return merged, report, err
}

// MergeDexBackup merges a snapshot as described by MergeDexConfig, using
// snapshotSecrets from the source cluster's dex-connector-secrets Secret to
// resolve clientSecret references of the form $NAME or ${NAME}. Added connectors
// receive new references from dexsecrets.NewKey to preserve live secret values.
// The returned map contains the new keys and values and is empty when none are
// needed. Store these values before saving the merged config.
//
// Connectors referencing variables absent from snapshotSecrets are skipped. If
// both issuers are nonempty and differ, an added connector whose redirectURI
// equals the snapshot issuer plus /callback receives the live issuer plus
// /callback. Custom callback URLs are preserved.
func MergeDexBackup(liveConfigYAML, snapshotConfigYAML []byte, snapshotSecrets map[string][]byte) ([]byte, DexUserImportReport, map[string]string, error) {
	var report DexUserImportReport

	var live map[string]any
	if err := yaml.Unmarshal(liveConfigYAML, &live); err != nil {
		return nil, report, nil, fmt.Errorf("parsing live dex-config: %w", err)
	}
	if live == nil {
		live = map[string]any{}
	}
	var snap map[string]any
	if err := yaml.Unmarshal(snapshotConfigYAML, &snap); err != nil {
		return nil, report, nil, fmt.Errorf("parsing snapshot dex-config: %w", err)
	}

	livePws, _ := live["staticPasswords"].([]any)
	snapPws, _ := snap["staticPasswords"].([]any)
	merged, added, present := mergeListByKey(livePws, snapPws, "email")
	report.StaticPasswordsAdded = added
	report.AlreadyPresent += present
	live["staticPasswords"] = merged

	liveConns, _ := live["connectors"].([]any)
	var snapConns []any
	all, _ := snap["connectors"].([]any)
	for _, c := range all {
		conn, _ := c.(map[string]any)
		id, _ := conn["id"].(string)
		connConfig, _ := conn["config"].(map[string]any)
		if secret, _ := connConfig["clientSecret"].(string); strings.HasPrefix(secret, "$") {
			key := referencedVariable(secret)
			if _, ok := snapshotSecrets[key]; !ok {
				report.ConnectorsSkipped = append(report.ConnectorsSkipped, id)
				continue
			}
		}
		snapConns = append(snapConns, c)
	}
	merged, added, present = mergeListByKey(liveConns, snapConns, "id")
	report.ConnectorsAdded = added
	report.AlreadyPresent += present
	live["connectors"] = merged
	report.ConnectorCallback = moveCallbacks(merged[len(liveConns):], snap["issuer"], live["issuer"])

	values := map[string]string{}
	for _, c := range merged[len(liveConns):] {
		conn, _ := c.(map[string]any)
		connConfig, ok := conn["config"].(map[string]any)
		secret, _ := connConfig["clientSecret"].(string)
		if !ok || !strings.HasPrefix(secret, "$") {
			continue
		}
		id, _ := conn["id"].(string)
		newKey := dexsecrets.NewKey(id)
		connConfig["clientSecret"] = "$" + newKey
		values[newKey] = string(snapshotSecrets[referencedVariable(secret)])
	}

	out, err := yaml.Marshal(live)
	if err != nil {
		return nil, report, nil, fmt.Errorf("re-encoding dex-config: %w", err)
	}
	return out, report, values, nil
}

// moveCallbacks points each connector whose redirectURI is the callback of
// oldIssuer at newIssuer's callback, and returns that callback when it moved any.
func moveCallbacks(connectors []any, oldIssuer, newIssuer any) string {
	from, _ := oldIssuer.(string)
	to, _ := newIssuer.(string)
	if from == "" || to == "" || from == to {
		return ""
	}
	moved := ""
	for _, c := range connectors {
		conn, _ := c.(map[string]any)
		connConfig, _ := conn["config"].(map[string]any)
		if uri, _ := connConfig["redirectURI"].(string); uri == from+"/callback" {
			connConfig["redirectURI"] = to + "/callback"
			moved = to + "/callback"
		}
	}
	return moved
}

// referencedVariable returns the variable name in a Dex reference of the form
// $NAME or ${NAME}.
func referencedVariable(ref string) string {
	name := strings.TrimPrefix(ref, "$")
	if strings.HasPrefix(name, "{") && strings.HasSuffix(name, "}") {
		name = name[1 : len(name)-1]
	}
	return name
}

// ImportDexUsers reads the live `dex/dex-config` ConfigMap, merges in
// the snapshot's staticPasswords and connectors as described by MergeDexConfig,
// and writes the result only when entries are added. Read, merge and update
// errors are returned. The caller must restart the Dex Deployment for the new
// config to take effect.
func ImportDexUsers(ctx context.Context, client kubernetes.Interface, snapshotConfigYAML []byte) (DexUserImportReport, error) {
	return ImportDexBackup(ctx, client, snapshotConfigYAML, nil)
}

// ImportDexBackup imports a snapshot and the source cluster's connector secrets
// using MergeDexBackup. It stores new secrets and configures the Dex Deployment
// to read them before saving the config. The caller must restart Dex to load
// the new values. No writes occur when the merge adds no entries.
//
// Read, merge, secret setup and update errors are returned. Config updates use
// the resourceVersion read earlier, so concurrent changes return a conflict.
// Earlier secret or Deployment updates remain if a later operation fails.
func ImportDexBackup(ctx context.Context, client kubernetes.Interface, snapshotConfigYAML []byte, snapshotSecrets map[string][]byte) (DexUserImportReport, error) {
	var report DexUserImportReport

	cm, err := client.CoreV1().ConfigMaps(DexNamespace).Get(ctx, DexConfigMapName, metav1.GetOptions{})
	if err != nil {
		return report, fmt.Errorf("reading live %s/%s: %w", DexNamespace, DexConfigMapName, err)
	}

	liveRaw, ok := cm.Data["config.yaml"]
	if !ok {
		return report, fmt.Errorf("live dex-config has no config.yaml key")
	}

	merged, report, values, err := MergeDexBackup([]byte(liveRaw), snapshotConfigYAML, snapshotSecrets)
	if err != nil {
		return report, err
	}
	if !report.HasChanges() {
		return report, nil
	}
	if len(values) > 0 {
		if err := dexsecrets.Store(ctx, client, values); err != nil {
			return report, fmt.Errorf("storing SSO client secrets in %s/%s: %w", dexsecrets.Namespace, dexsecrets.SecretName, err)
		}
		if err := dexsecrets.EnsureDexReads(ctx, client); err != nil {
			return report, fmt.Errorf("loading %s into Dex: %w", dexsecrets.SecretName, err)
		}
	}

	cm.Data["config.yaml"] = string(merged)
	if _, err := client.CoreV1().ConfigMaps(DexNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return report, fmt.Errorf("updating dex-config: %w", err)
	}
	return report, nil
}

// ExtractDexConfigYAML pulls the inner `config.yaml` document out of a
// captured ConfigMap snapshot. It accepts two shapes:
//
//   - A full ConfigMap manifest with `data: { config.yaml: ... }`.
//   - The raw Dex config YAML directly (issuer:, staticPasswords:, ...).
//
// Returns the inner YAML as bytes. The shape detection is best-effort:
// a top-level data mapping must contain a string config.yaml value. Otherwise,
// the input is treated as the Dex config itself. Invalid YAML or a data mapping
// without a string config.yaml value returns an error.
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
	return raw, nil
}

// mergeListByKey returns dst with every entry from src whose `key`
// value is not already present in dst appended. The string key is
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

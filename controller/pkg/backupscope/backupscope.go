// Package backupscope holds the resource selection that every Kipper backup,
// schedule and restore shares, so kip, the installer and the console select
// the same objects.
package backupscope

// ExcludedNamespaces returns the namespaces that backups of user workloads
// skip: system namespaces with regenerable state and Velero's own namespace.
// Velero must not back up the MinIO PVC that hosts its own bucket, or the
// backup hangs in InProgress. Each call returns a fresh slice.
func ExcludedNamespaces() []string {
	return []string{
		"kube-system",
		"kube-public",
		"kube-node-lease",
		"monitoring",
		"keda",
		"longhorn-system",
		"traefik",
		"velero",
	}
}

// TransientResources returns the cert-manager issuance kinds that backups and
// restores skip. A restored CertificateRequest can collide with request names
// generated after its Certificate loses revision state. Certificates and TLS
// Secrets stay included. Each call returns a fresh slice.
func TransientResources() []string {
	return []string{
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}
}

func projectResources() []string {
	return []string{"projects.kipper.run", "resourceadjustments.kipper.run"}
}

func clusterIdentityResources() []string {
	return []string{"platformconfigs.kipper.run", "clusteridentities.kipper.run"}
}

// BackedUpClusterResources returns the cluster-scoped Kipper kinds selected by
// Backup. Velero can also include dependencies of namespaced objects, such as
// the PV backing a PVC. Each call returns a fresh slice.
func BackedUpClusterResources() []string {
	return append(projectResources(), clusterIdentityResources()...)
}

// Backup overwrites excludedNamespaceScopedResources and
// includedClusterScopedResources in a non-nil Velero Backup spec with the
// transient issuance exclusions and Kipper cluster resource selection.
// Callers control namespace selection. Velero's scoped filters cannot be
// combined with includedResources, excludedResources or includeClusterResources.
func Backup(spec map[string]interface{}) {
	spec["excludedNamespaceScopedResources"] = toAnySlice(TransientResources())
	spec["includedClusterScopedResources"] = toAnySlice(BackedUpClusterResources())
}

// BackupWholeCluster applies Backup to a non-nil spec, then selects every
// cluster-scoped kind. Callers must leave namespace selection unrestricted
// to back up every namespace.
func BackupWholeCluster(spec map[string]interface{}) {
	Backup(spec)
	spec["includedClusterScopedResources"] = []interface{}{"*"}
}

// Restore sets the shared selection on a Velero Restore spec: the platform
// configuration and cluster identity always stay out, projects only with
// clusterResources. It never sets includeClusterResources to false, which would
// skip the PV behind each restored PVC and leave the PVC bound to a missing volume.
func Restore(spec map[string]interface{}, clusterResources bool) {
	excluded := TransientResources()
	if !clusterResources {
		excluded = append(excluded, projectResources()...)
	}
	excluded = append(excluded, clusterIdentityResources()...)
	spec["excludedResources"] = toAnySlice(excluded)
	if clusterResources {
		spec["includeClusterResources"] = true
	}
}

func toAnySlice(items []string) []interface{} {
	out := make([]interface{}, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

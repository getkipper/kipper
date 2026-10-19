package controllers

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	stderrors "errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

const (
	// serviceCAMountRoot is where a bound workload finds the CA of each
	// service it is bound to, one directory per service.
	serviceCAMountRoot = "/etc/kipper/service-ca"
	// sslRootCertKey is the credentials key naming the CA file, which bound
	// workloads see with their binding prefix, for example DB_SSLROOTCERT.
	sslRootCertKey = "SSLROOTCERT"
	serviceCAFile  = "ca.crt"
)

func serviceCAConfigMapName(service string) string { return service + "-ca" }

func serviceCAPath(service string) string {
	return serviceCAMountRoot + "/" + service + "/" + serviceCAFile
}

// verifyIssuedCertificate verifies the certificate chain against the CA in secret,
// including server authentication usage, the DNS name host and validity at now.
func verifyIssuedCertificate(secret *corev1.Secret, host string, now time.Time) error {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(secret.Data["ca.crt"]) {
		return stderrors.New("the Secret holds no usable CA certificate")
	}
	block, rest := pem.Decode(secret.Data[corev1.TLSCertKey])
	if block == nil {
		return stderrors.New("the Secret holds no certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parsing the certificate: %w", err)
	}
	intermediates := x509.NewCertPool()
	intermediates.AppendCertsFromPEM(rest)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: host, CurrentTime: now}); err != nil {
		return fmt.Errorf("the certificate does not verify for %s: %w", host, err)
	}
	return nil
}

// certificateProblem returns the verification error for host at the current time,
// or an empty string if verification succeeds.
func certificateProblem(secret *corev1.Secret, host string) string {
	if err := verifyIssuedCertificate(secret, host, time.Now()); err != nil {
		return err.Error()
	}
	return ""
}

// ensureServiceCA publishes only the CA certificate from the issued Secret in
// the ConfigMap bound workloads mount. An existing ConfigMap must be owned by
// the service or have no owner references and carry the service's tls-for label
// for adoption. It updates the ConfigMap when the data changes or it is adopted.
func (r *ServiceReconciler) ensureServiceCA(ctx context.Context, svc *kipperv1.Service, secret *corev1.Secret) error {
	data := map[string]string{serviceCAFile: string(secret.Data["ca.crt"])}
	var existing corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: serviceCAConfigMapName(svc.Name)}, &existing)
	if errors.IsNotFound(err) {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: serviceCAConfigMapName(svc.Name), Namespace: svc.Namespace,
				Labels: map[string]string{kipperLabel: kipperValue, "app": svc.Name, labels.TLSFor: svc.Name},
			},
			Data: data,
		}
		if err := controllerutil.SetControllerReference(svc, cm, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, cm)
	}
	if err != nil {
		return err
	}
	adopted, err := r.takeBackTLSObject("ConfigMap", &existing, svc)
	if err != nil {
		return err
	}
	if !adopted && stringMapEqual(existing.Data, data) {
		return nil
	}
	existing.Data = data
	return r.Update(ctx, &existing)
}

// serviceCAPublished reports whether a Postgres service has TLS configured as
// on or required and owns its CA ConfigMap, allowing SSLROOTCERT in credentials.
// Restored ConfigMaps eligible for adoption also count, so bound apps retain
// SSLROOTCERT during a restore. A missing ConfigMap returns false without an
// error. Other API errors are returned.
func (r *ServiceReconciler) serviceCAPublished(ctx context.Context, svc *kipperv1.Service) (bool, error) {
	if svc.Spec.Type != "postgres" || !tlsEnabled(svc) {
		return false, nil
	}
	var cm corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: serviceCAConfigMapName(svc.Name)}, &cm)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ownsTLSObject(&cm, svc), nil
}

// serviceCAVolumes returns one read-only CA mount per bound service whose
// SSLROOTCERT matches its managed path, keeping mounts consistent with the
// environment snapshot. Results are sorted by service name, and volume names
// avoid those in taken. Declared mounts that overlap a CA directory are rejected
// so SSLROOTCERT points to a file Kipper controls. ConfigMaps are optional so pods
// from older templates can start after CA removal. Clients that verify the server
// then fail because the CA file is missing.
func serviceCAVolumes(sources []envSource, taken map[string]bool, declared []corev1.VolumeMount) ([]corev1.Volume, []corev1.VolumeMount, error) {
	services := map[string]bool{}
	for _, s := range sources {
		if s.origin != originBinding || s.service == "" {
			continue
		}
		for _, v := range s.values {
			if v.Name == sslRootCertKey && v.Value == serviceCAPath(s.service) {
				services[s.service] = true
			}
		}
	}
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dir := serviceCAMountRoot + "/" + name
		for _, m := range declared {
			p := path.Clean(m.MountPath)
			if p == "/" || p == dir || strings.HasPrefix(dir, p+"/") || strings.HasPrefix(p, dir+"/") {
				return nil, nil, fmt.Errorf("volume %q is mounted at %s, which overlaps %s, where Kipper mounts the CA of service %q. Mount it somewhere else",
					m.Name, m.MountPath, dir, name)
			}
		}
	}

	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount
	optional := true
	n := 0
	for _, name := range names {
		volume := fmt.Sprintf("kipper-service-ca-%d", n)
		for taken[volume] {
			n++
			volume = fmt.Sprintf("kipper-service-ca-%d", n)
		}
		n++
		volumes = append(volumes, corev1.Volume{Name: volume, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: serviceCAConfigMapName(name)},
			Optional:             &optional,
		}}})
		mounts = append(mounts, corev1.VolumeMount{Name: volume, MountPath: serviceCAMountRoot + "/" + name, ReadOnly: true})
	}
	return volumes, mounts, nil
}

func volumeNames(volumes []corev1.Volume) map[string]bool {
	names := make(map[string]bool, len(volumes))
	for _, v := range volumes {
		names[v.Name] = true
	}
	return names
}

// functionVolumeNames reserves the code and shared-volume names to prevent CA
// volume name collisions, even when the function does not use inline code.
func functionVolumeNames(fn *kipperv1.Function) map[string]bool {
	names := map[string]bool{"function-code": true}
	for _, vm := range fn.Spec.Volumes {
		names[vm.Name] = true
	}
	return names
}

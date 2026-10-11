package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// nameUnusedAnnotation stores the start of the observed idle period as
// <label>@<RFC3339> on the gateway-credentials Secret.
const nameUnusedAnnotation = "kipper.run/gateway-name-unused-since"

const (
	nameUnusedGrace   = 24 * time.Hour
	nameUnusedRecheck = time.Hour
)

// hostUnderName matches <label>.kipper.run and hosts ending in --<label>.kipper.run.
func hostUnderName(host, label string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	name := strings.ToLower(label) + ".kipper.run"
	return host == name || strings.HasSuffix(host, "--"+name)
}

// nameIdle reports whether checks have found no Ingress host using the name
// for at least nameUnusedGrace. Returning true pauses heartbeat renewal.
// Read failures reset the grace period so uncertainty keeps renewal enabled.
func (h *gatewayHeartbeat) nameIdle(ctx context.Context) bool {
	secrets := h.client.CoreV1().Secrets(gatewayCredentialsNamespace)
	inUse, err := h.nameInUse(ctx)
	if err != nil {
		log.Printf("gateway heartbeat: checking whether %s.kipper.run is still served: %v", h.subdomain, err)
	}
	secret, getErr := secrets.Get(ctx, gatewayCredentialsSecret, metav1.GetOptions{})
	if getErr != nil {
		h.writtenRecord = ""
		return false
	}
	record := secret.Annotations[nameUnusedAnnotation]

	if inUse || err != nil {
		h.writtenRecord = ""
		if record != "" {
			delete(secret.Annotations, nameUnusedAnnotation)
			if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
				log.Printf("gateway heartbeat: clearing the unused record for %s.kipper.run: %v", h.subdomain, err)
			}
		}
		return false
	}

	now := h.clock()
	if since, ok := unusedSince(record, h.subdomain); ok && record == h.writtenRecord {
		if now.Sub(since) < nameUnusedGrace {
			return false
		}
		log.Printf("gateway heartbeat: no Ingress host has used %s.kipper.run since %s; pausing renewal; "+
			"the name will expire under the gateway's retention policy",
			h.subdomain, since.Format(time.RFC3339))
		return true
	}

	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	fresh := fmt.Sprintf("%s@%s", h.subdomain, now.Format(time.RFC3339))
	secret.Annotations[nameUnusedAnnotation] = fresh
	if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
		log.Printf("gateway heartbeat: recording that %s.kipper.run is unused: %v", h.subdomain, err)
		return false
	}
	h.writtenRecord = fresh
	return false
}

// nameInUse checks configured Ingress hosts across all namespaces, regardless
// of traffic or backend health.
func (h *gatewayHeartbeat) nameInUse(ctx context.Context) (bool, error) {
	list, err := h.client.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for _, ing := range list.Items {
		for _, rule := range ing.Spec.Rules {
			if hostUnderName(rule.Host, h.subdomain) {
				return true, nil
			}
		}
	}
	return false, nil
}

// unusedSince parses the timestamp from a valid record for label.
func unusedSince(record, label string) (time.Time, bool) {
	name, at, ok := strings.Cut(record, "@")
	if !ok || name != label {
		return time.Time{}, false
	}
	since, err := time.Parse(time.RFC3339, at)
	return since, err == nil
}

func (h *gatewayHeartbeat) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var certCmd = &cobra.Command{
	Use:   "cert",
	Short: "Manage TLS certificates",
}

var certEmailCmd = &cobra.Command{
	Use:   "email [address]",
	Short: "Show or update the Let's Encrypt email used for TLS certificates",
	Long: `Shows or updates the ACME registration email on the cluster's ClusterIssuer.

Without arguments, shows the current email. With an email address, updates
it and triggers renewal for any stuck certificates.

The email must be a real address with a valid public domain. Addresses like
admin@kipper.local will be rejected by Let's Encrypt.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runCertEmail,
}

func init() {
	certCmd.AddCommand(certEmailCmd)
	rootCmd.AddCommand(certCmd)
}

func runCertEmail(cmd *cobra.Command, args []string) error {
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dynClient := k8sClient.Dynamic()
	gvr := schema.GroupVersionResource{
		Group:    "cert-manager.io",
		Version:  "v1",
		Resource: "clusterissuers",
	}

	// Show current email when called without arguments
	if len(args) == 0 {
		issuer, err := dynClient.Resource(gvr).Get(ctx, "letsencrypt-prod", metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading ClusterIssuer: %w", err)
		}
		currentEmail, _, _ := unstructured.NestedString(issuer.Object, "spec", "acme", "email")
		fmt.Printf("\n  Let's Encrypt email: %s\n\n", currentEmail)
		return nil
	}

	email := args[0]
	clientset := k8sClient.Clientset()

	// Delete the old ACME account secret so cert-manager re-registers
	// with the new email address. Without this, the old registration
	// (which may have failed) is reused.
	_ = clientset.CoreV1().Secrets("cert-manager").Delete(ctx, "letsencrypt-prod", metav1.DeleteOptions{})

	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"acme": map[string]interface{}{
				"email": email,
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("building patch: %w", err)
	}

	_, err = dynClient.Resource(gvr).Patch(ctx, "letsencrypt-prod", types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("updating ClusterIssuer email: %w", err)
	}

	fmt.Printf("\n  ✔  Let's Encrypt email updated to %s\n", email)

	// Check if any certificates are stuck and trigger renewal
	certGVR := schema.GroupVersionResource{
		Group:    "cert-manager.io",
		Version:  "v1",
		Resource: "certificates",
	}

	certs, err := dynClient.Resource(certGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Printf("  ⚠  Could not list certificates: %v\n", err)
		fmt.Printf("  Certificates will renew on their next cycle.\n\n")
		return nil
	}

	renewed := 0
	for _, cert := range certs.Items {
		ready := isCertReady(cert)
		if !ready {
			ns := cert.GetNamespace()
			name := cert.GetName()
			secretName, _, _ := unstructured.NestedString(cert.Object, "spec", "secretName")
			if secretName != "" {
				_ = clientset.CoreV1().Secrets(ns).Delete(ctx, secretName, metav1.DeleteOptions{})
			}
			renewed++
			fmt.Printf("  ✔  Triggered renewal for %s/%s\n", ns, name)
		}
	}

	if renewed == 0 {
		fmt.Printf("  All certificates are healthy.\n")
	} else {
		fmt.Printf("\n  %d certificate(s) queued for renewal. This usually takes 1-2 minutes.\n", renewed)
	}
	fmt.Println()

	return nil
}

func isCertReady(cert unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(cert.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cond["type"] == "Ready" && cond["status"] == "True" {
			return true
		}
	}
	return false
}

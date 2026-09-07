package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

var certificateGVR = schema.GroupVersionResource{
	Group:    "cert-manager.io",
	Version:  "v1",
	Resource: "certificates",
}

var certListCmd = &cobra.Command{
	Use:   "list",
	Short: "List TLS certificates and their current state",
	Args:  cobra.NoArgs,
	RunE:  runCertList,
}

func init() {
	certCmd.AddCommand(certListCmd)
}

func runCertList(cmd *cobra.Command, args []string) error {
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return listCertificates(ctx, os.Stdout, k8sClient.Dynamic(), time.Now())
}

type certificateListRow struct {
	namespace string
	name      string
	host      string
	state     string
	age       string
	reason    string
}

func listCertificates(ctx context.Context, out io.Writer, dyn dynamic.Interface, now time.Time) error {
	certificates, err := dyn.Resource(certificateGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing certificates: %w", err)
	}

	if len(certificates.Items) == 0 {
		say(out, "\n  No certificates found\n\n")
		return nil
	}

	rows := make([]certificateListRow, 0, len(certificates.Items))
	for _, certificate := range certificates.Items {
		rows = append(rows, certificateRow(certificate, now))
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].namespace == rows[j].namespace {
			return rows[i].name < rows[j].name
		}
		return rows[i].namespace < rows[j].namespace
	})

	say(out, "\n  %-20s %-25s %-40s %-10s %-6s %s\n", "NAMESPACE", "NAME", "HOST", "STATE", "AGE", "REASON")
	for _, row := range rows {
		say(out, "  %-20s %-25s %-40s %-10s %-6s %s\n",
			row.namespace, row.name, row.host, row.state, row.age, row.reason)
	}
	say(out, "\n")
	return nil
}

func certificateRow(certificate unstructured.Unstructured, now time.Time) certificateListRow {
	hosts, _, _ := unstructured.NestedStringSlice(certificate.Object, "spec", "dnsNames")
	if len(hosts) == 0 {
		if commonName, ok, _ := unstructured.NestedString(certificate.Object, "spec", "commonName"); ok {
			hosts = []string{commonName}
		}
	}
	host := strings.Join(hosts, ",")
	if host == "" {
		host = "-"
	}

	state := "Pending"
	reason := "Waiting for cert-manager"
	changedAt := certificate.GetCreationTimestamp().Time
	conditions, _, _ := unstructured.NestedSlice(certificate.Object, "status", "conditions")
	for _, value := range conditions {
		condition, ok := value.(map[string]interface{})
		if !ok || condition["type"] != "Ready" {
			continue
		}

		switch condition["status"] {
		case "True":
			state = "Ready"
			reason = "-"
		case "False":
			state = "Not Ready"
			reason = certificateConditionReason(condition)
		default:
			state = "Unknown"
			reason = certificateConditionReason(condition)
		}
		if raw, ok := condition["lastTransitionTime"].(string); ok {
			if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
				changedAt = parsed
			}
		}
		break
	}

	return certificateListRow{
		namespace: certificate.GetNamespace(),
		name:      certificate.GetName(),
		host:      host,
		state:     state,
		age:       certificateAge(now, changedAt),
		reason:    reason,
	}
}

func certificateConditionReason(condition map[string]interface{}) string {
	reason, _ := condition["reason"].(string)
	message, _ := condition["message"].(string)
	switch {
	case reason != "" && message != "":
		return reason + ": " + message
	case reason != "":
		return reason
	case message != "":
		return message
	default:
		return "Reason unavailable"
	}
}

func certificateAge(now, changedAt time.Time) string {
	if changedAt.IsZero() {
		return "-"
	}
	d := now.Sub(changedAt)
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}

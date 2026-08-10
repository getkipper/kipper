package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var appAutoscaleCmd = &cobra.Command{
	Use:   "autoscale [app-name]",
	Short: "Configure automatic scaling for an app",
	Long: `Configure horizontal pod autoscaling based on CPU and memory usage.

Examples:
  kip app autoscale api --min 1 --max 5 --cpu 70
  kip app autoscale api --min 2 --max 10 --cpu 80 --memory 80
  kip app autoscale api --status
  kip app autoscale api --off`,
	Args: cobra.ExactArgs(1),
	RunE: runAutoscale,
}

func init() {
	appAutoscaleCmd.Flags().Int32("min", 1, "minimum number of replicas")
	appAutoscaleCmd.Flags().Int32("max", 5, "maximum number of replicas")
	appAutoscaleCmd.Flags().Int32("cpu", 0, "target CPU utilization percentage (e.g. 70)")
	appAutoscaleCmd.Flags().Int32("memory", 0, "target memory utilization percentage (e.g. 80)")
	appAutoscaleCmd.Flags().Bool("status", false, "show current autoscaling status")
	appAutoscaleCmd.Flags().Bool("off", false, "disable autoscaling")
	appAutoscaleCmd.Flags().String("project", "", "project name")
	appAutoscaleCmd.Flags().String("environment", "", "target environment")

	appCmd.AddCommand(appAutoscaleCmd)
}

func runAutoscale(cmd *cobra.Command, args []string) error {
	appName := args[0]
	showStatus, _ := cmd.Flags().GetBool("status")
	off, _ := cmd.Flags().GetBool("off")

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}

	ctx := context.Background()
	clientset := k8sClient.Clientset()

	if showStatus {
		return printAutoscaleStatus(ctx, clientset, ns, appName)
	}

	if off {
		err := clientset.AutoscalingV2().HorizontalPodAutoscalers(ns).Delete(ctx, appName, metav1.DeleteOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				fmt.Printf("  Autoscaling is not enabled for %s\n", appName)
				return nil
			}
			return fmt.Errorf("disabling autoscaling: %w", err)
		}
		fmt.Printf("  ✔  Autoscaling disabled for %s\n", appName)
		return nil
	}

	minReplicas, _ := cmd.Flags().GetInt32("min")
	maxReplicas, _ := cmd.Flags().GetInt32("max")
	cpuTarget, _ := cmd.Flags().GetInt32("cpu")
	memoryTarget, _ := cmd.Flags().GetInt32("memory")

	if cpuTarget == 0 && memoryTarget == 0 {
		cpuTarget = 70
	}

	var metrics []autoscalingv2.MetricSpec
	if cpuTarget > 0 {
		metrics = append(metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: "cpu",
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: &cpuTarget,
				},
			},
		})
	}
	if memoryTarget > 0 {
		metrics = append(metrics, autoscalingv2.MetricSpec{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name: "memory",
				Target: autoscalingv2.MetricTarget{
					Type:               autoscalingv2.UtilizationMetricType,
					AverageUtilization: &memoryTarget,
				},
			},
		})
	}

	// Ensure the deployment has resource requests — HPA needs them to calculate utilisation
	deploy, err := clientset.AppsV1().Deployments(ns).Get(ctx, appName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("deployment %q not found: %w", appName, err)
	}

	needsUpdate := false
	for i := range deploy.Spec.Template.Spec.Containers {
		c := &deploy.Spec.Template.Spec.Containers[i]
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		if _, ok := c.Resources.Requests[corev1.ResourceCPU]; !ok && cpuTarget > 0 {
			c.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("100m")
			needsUpdate = true
		}
		if _, ok := c.Resources.Requests[corev1.ResourceMemory]; !ok && memoryTarget > 0 {
			c.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("128Mi")
			needsUpdate = true
		}
	}
	if needsUpdate {
		if _, err := clientset.AppsV1().Deployments(ns).Update(ctx, deploy, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("setting resource requests: %w", err)
		}
		fmt.Printf("  ✔  Added default resource requests (100m CPU, 128Mi memory)\n")
	}

	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appName,
			Namespace: ns,
			Labels: map[string]string{
				"app":                          appName,
				"app.kubernetes.io/managed-by": "kipper",
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       appName,
			},
			MinReplicas: &minReplicas,
			MaxReplicas: maxReplicas,
			Metrics:     metrics,
		},
	}

	_, err = clientset.AutoscalingV2().HorizontalPodAutoscalers(ns).Create(ctx, hpa, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		existing, getErr := clientset.AutoscalingV2().HorizontalPodAutoscalers(ns).Get(ctx, appName, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("getting existing HPA: %w", getErr)
		}
		existing.Spec = hpa.Spec
		_, err = clientset.AutoscalingV2().HorizontalPodAutoscalers(ns).Update(ctx, existing, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("configuring autoscaling: %w", err)
	}

	fmt.Printf("\n  ✔  Autoscaling enabled for %s\n", appName)
	fmt.Printf("  Replicas: %d–%d\n", minReplicas, maxReplicas)
	if cpuTarget > 0 {
		fmt.Printf("  CPU target: %d%%\n", cpuTarget)
	}
	if memoryTarget > 0 {
		fmt.Printf("  Memory target: %d%%\n", memoryTarget)
	}
	fmt.Println()

	return nil
}

func printAutoscaleStatus(ctx context.Context, clientset kubernetes.Interface, ns, appName string) error {
	hpa, err := clientset.AutoscalingV2().HorizontalPodAutoscalers(ns).Get(ctx, appName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			fmt.Printf("\n  Autoscaling is not enabled for %s (using fixed replicas)\n\n", appName)
			return nil
		}
		return fmt.Errorf("getting autoscale status: %w", err)
	}

	fmt.Printf("\n  Autoscaling: enabled\n")
	fmt.Printf("  Replicas: %d–%d (current: %d)\n", *hpa.Spec.MinReplicas, hpa.Spec.MaxReplicas, hpa.Status.CurrentReplicas)

	for _, metric := range hpa.Spec.Metrics {
		if metric.Resource != nil && metric.Resource.Target.AverageUtilization != nil {
			current := "unknown"
			for _, status := range hpa.Status.CurrentMetrics {
				if status.Resource != nil && status.Resource.Name == metric.Resource.Name && status.Resource.Current.AverageUtilization != nil {
					current = fmt.Sprintf("%d%%", *status.Resource.Current.AverageUtilization)
				}
			}
			fmt.Printf("  %s: target %d%%, current %s\n", metric.Resource.Name, *metric.Resource.Target.AverageUtilization, current)
		}
	}
	fmt.Println()

	return nil
}

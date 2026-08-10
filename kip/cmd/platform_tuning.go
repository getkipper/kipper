package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	tuningModeConfigMap = "kipper-mode"
	tuningModeNamespace = "kipper-system"
	tuningModeAuto      = "auto"
	tuningModeExpert    = "expert"
)

var platformTuningCmd = &cobra.Command{
	Use:   "tuning",
	Short: "Show or set the resource tuning mode",
	Long: `The resource controller watches every Kipper-managed workload and adjusts
CPU and memory automatically (auto mode). In expert mode it only reports
problems and leaves all resource values alone.

Examples:
  kip platform tuning show
  kip platform tuning expert   # hands off, no automatic resource changes
  kip platform tuning auto     # default, resources adjust automatically`,
}

var platformTuningShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print the active tuning mode",
	RunE: func(cmd *cobra.Command, args []string) error {
		_, k8sClient, err := loadCurrentCluster()
		if err != nil {
			return err
		}
		mode, err := getTuningMode(context.Background(), k8sClient.Clientset())
		if err != nil {
			return err
		}
		fmt.Printf("\n  Tuning mode: %s\n\n", mode)
		return nil
	},
}

func platformTuningSetCmd(mode, short string) *cobra.Command {
	return &cobra.Command{
		Use:   mode,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, k8sClient, err := loadCurrentCluster()
			if err != nil {
				return err
			}
			if err := setTuningMode(context.Background(), k8sClient.Clientset(), mode); err != nil {
				return err
			}
			fmt.Printf("\n  ✔  Tuning mode set to %s\n\n", mode)
			return nil
		},
	}
}

func init() {
	platformTuningCmd.AddCommand(platformTuningShowCmd)
	platformTuningCmd.AddCommand(platformTuningSetCmd(tuningModeAuto, "Adjust workload resources automatically (default)"))
	platformTuningCmd.AddCommand(platformTuningSetCmd(tuningModeExpert, "Report problems only, never change resources"))
	platformCmd.AddCommand(platformTuningCmd)
}

// getTuningMode reads the mode ConfigMap; a missing map or value means the
// resource controller runs with its default, auto.
func getTuningMode(ctx context.Context, clientset kubernetes.Interface) (string, error) {
	cm, err := clientset.CoreV1().ConfigMaps(tuningModeNamespace).Get(ctx, tuningModeConfigMap, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return tuningModeAuto, nil
	}
	if err != nil {
		return "", fmt.Errorf("reading tuning mode: %w", err)
	}
	mode := cm.Data["mode"]
	if mode != tuningModeAuto && mode != tuningModeExpert {
		return tuningModeAuto, nil
	}
	return mode, nil
}

// setTuningMode writes the mode ConfigMap the resource controller reads
// every tick, creating it on first use.
func setTuningMode(ctx context.Context, clientset kubernetes.Interface, mode string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		maps := clientset.CoreV1().ConfigMaps(tuningModeNamespace)
		cm, err := maps.Get(ctx, tuningModeConfigMap, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			cm = &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tuningModeConfigMap,
					Namespace: tuningModeNamespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "kipper",
					},
				},
				Data: map[string]string{"mode": mode},
			}
			if _, err := maps.Create(ctx, cm, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("writing tuning mode: %w", err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading tuning mode: %w", err)
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["mode"] = mode
		if _, err := maps.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("writing tuning mode: %w", err)
		}
		return nil
	})
}

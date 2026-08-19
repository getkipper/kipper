package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/kip/internal/deployer"
)

const historyAnnotation = "kipper.run/deploy-history"
const maxHistoryEntries = 10

// DeployEntry represents a single deployment event.
//
// Three structs marshal this annotation: this one, controllers.buildDeployEntry
// and handlers.deployEntry in console-api. All three round-trip the whole list,
// so a field missing from any of them is stripped from every entry the first
// time that writer runs. Add a field to all three or to none.
type DeployEntry struct {
	Revision  int    `json:"revision"`
	Image     string `json:"image"`
	Commit    string `json:"commit,omitempty"`
	Trigger   string `json:"trigger"` // "build", "webhook", "manual", "promote", "rollback"
	Timestamp string `json:"timestamp"`
	// Build is the job a build entry came from, which is what stops a replayed
	// completion recording itself twice.
	Build string `json:"build,omitempty"`
}

// RecordDeploy adds an entry to the deploy history stored on the App CR
// annotations. The App CR is the single source of truth: the build controller
// records its builds on the same annotation, so kip and the console share one
// history instead of each keeping its own.
func RecordDeploy(ctx context.Context, dyn dynamic.Interface, namespace, appName, image, commit, trigger string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := dyn.Resource(deployer.AppGVR).Namespace(namespace).Get(ctx, appName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting app: %w", err)
		}

		history := loadHistory(app.GetAnnotations())
		history = prepend(history, DeployEntry{
			Revision:  nextRevision(history),
			Image:     image,
			Commit:    commit,
			Trigger:   trigger,
			Timestamp: time.Now().Format(time.RFC3339),
		})

		if err := setHistory(app, history); err != nil {
			return err
		}
		_, err = dyn.Resource(deployer.AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{})
		return err
	})
}

// GetHistory returns the deploy history for an app, read from its App CR.
func GetHistory(ctx context.Context, dyn dynamic.Interface, namespace, appName string) ([]DeployEntry, error) {
	app, err := dyn.Resource(deployer.AppGVR).Namespace(namespace).Get(ctx, appName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting app: %w", err)
	}
	return loadHistory(app.GetAnnotations()), nil
}

// Rollback reverts an app to a previous revision by setting the App CR's
// spec.image to that revision's image and recording a rollback entry. The
// reconciler then rolls the Deployment. Patching the Deployment directly would
// be undone on the next reconcile, since the App CR owns the desired image.
func Rollback(ctx context.Context, dyn dynamic.Interface, namespace, appName string, targetRevision int) (*DeployEntry, error) {
	var target DeployEntry
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		app, err := dyn.Resource(deployer.AppGVR).Namespace(namespace).Get(ctx, appName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting app: %w", err)
		}

		history := loadHistory(app.GetAnnotations())
		if targetRevision != 0 {
			// An explicit revision must exist. Falling back to "previous" would
			// silently roll somewhere the user did not ask for — e.g. when the
			// revision has aged out of the bounded history.
			t := findRevision(history, targetRevision)
			if t == nil {
				return fmt.Errorf("revision %d not found in deploy history", targetRevision)
			}
			target = *t
		} else {
			if len(history) < 2 {
				return fmt.Errorf("no previous version to rollback to")
			}
			// Default: roll back to the immediately previous version.
			target = history[1]
		}

		if err := unstructured.SetNestedField(app.Object, target.Image, "spec", "image"); err != nil {
			return fmt.Errorf("setting image: %w", err)
		}

		history = prepend(history, DeployEntry{
			Revision:  nextRevision(history),
			Image:     target.Image,
			Commit:    target.Commit,
			Trigger:   "rollback",
			Timestamp: time.Now().Format(time.RFC3339),
		})
		if err := setHistory(app, history); err != nil {
			return err
		}

		_, err = dyn.Resource(deployer.AppGVR).Namespace(namespace).Update(ctx, app, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &target, nil
}

func nextRevision(history []DeployEntry) int {
	if len(history) > 0 {
		return history[0].Revision + 1
	}
	return 1
}

func prepend(history []DeployEntry, entry DeployEntry) []DeployEntry {
	history = append([]DeployEntry{entry}, history...)
	if len(history) > maxHistoryEntries {
		history = history[:maxHistoryEntries]
	}
	return history
}

func findRevision(history []DeployEntry, revision int) *DeployEntry {
	for i := range history {
		if history[i].Revision == revision {
			return &history[i]
		}
	}
	return nil
}

func setHistory(app *unstructured.Unstructured, history []DeployEntry) error {
	data, err := json.Marshal(history)
	if err != nil {
		return fmt.Errorf("marshalling history: %w", err)
	}
	annotations := app.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[historyAnnotation] = string(data)
	app.SetAnnotations(annotations)
	return nil
}

func loadHistory(annotations map[string]string) []DeployEntry {
	if annotations == nil {
		return nil
	}

	raw, ok := annotations[historyAnnotation]
	if !ok || raw == "" {
		return nil
	}

	var history []DeployEntry
	if err := json.Unmarshal([]byte(raw), &history); err != nil {
		return nil
	}

	return history
}

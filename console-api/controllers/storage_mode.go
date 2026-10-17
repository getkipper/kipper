package controllers

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

const conditionSharedStorageUnsupported = "SharedStorageUnsupported"

// clusterStorageMode reads the platform's storage mode, defaulting to Longhorn
// when PlatformConfig or spec.storage is missing for compatibility with older clusters.
func clusterStorageMode(ctx context.Context, c client.Reader) (string, error) {
	var pc kipperv1.PlatformConfig
	if err := c.Get(ctx, types.NamespacedName{Name: PlatformConfigName}, &pc); err != nil {
		if errors.IsNotFound(err) {
			return platform.StorageLonghorn, nil
		}
		return "", fmt.Errorf("reading the storage mode: %w", err)
	}
	return platform.StorageMode(pc.Spec.Storage), nil
}

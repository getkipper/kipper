package migrate

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/usergrants"
)

// ImportUserRoles adds the console roles and monitoring grants in backupData,
// the data of a kipper-users ConfigMap, to the live ConfigMap as described by
// usergrants.Merge. It returns added emails in sorted order, or nil without a
// write when none are added. Read, merge and update errors are returned. The
// update uses the resourceVersion read earlier, so concurrent changes return
// a conflict and preserve the stored ConfigMap.
func ImportUserRoles(ctx context.Context, client kubernetes.Interface, backupData map[string]string) ([]string, error) {
	configMaps := client.CoreV1().ConfigMaps(usergrants.Namespace)
	cm, err := configMaps.Get(ctx, usergrants.ConfigMapName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading live %s/%s: %w", usergrants.Namespace, usergrants.ConfigMapName, err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	added, err := usergrants.Merge(cm.Data, backupData)
	if err != nil || len(added) == 0 {
		return nil, err
	}
	if _, err := configMaps.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return nil, fmt.Errorf("updating %s/%s: %w", usergrants.Namespace, usergrants.ConfigMapName, err)
	}
	return added, nil
}

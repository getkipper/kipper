package migration

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

func ownedBy(kind, name string) []metav1.OwnerReference {
	controller := true
	return []metav1.OwnerReference{{
		APIVersion: kipperv1.GroupVersion.String(),
		Kind:       kind,
		Name:       name,
		UID:        types.UID("uid-" + name),
		Controller: &controller,
	}}
}

// A workload's published environment is named for a digest of its own content,
// so a copy lands at exactly the name the target will compute — carrying an
// owner reference to a UID that does not exist there. The receiving controller
// refuses it as somebody else's object and stops publishing, and the workload
// never starts. It has to stay behind and be republished.
func TestTransferableSecret_LeavesAPublishedEnvironmentBehind(t *testing.T) {
	cases := []struct {
		name   string
		secret *corev1.Secret
		want   bool
	}{
		{
			name: "an app's published environment",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name:            secretname.EnvGeneration(secretname.KindApp, "api", "9f2c1a7b40de"),
				OwnerReferences: ownedBy("App", "api"),
			}},
			want: false,
		},
		{
			name: "a function's",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name:            secretname.EnvGeneration(secretname.KindFunction, "resize", "aabbccddeeff"),
				OwnerReferences: ownedBy("Function", "resize"),
			}},
			want: false,
		},
		{
			name: "a job's",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name:            secretname.EnvGeneration(secretname.KindJob, "migrate", "112233445566"),
				OwnerReferences: ownedBy("Job", "migrate"),
			}},
			want: false,
		},
		{
			name: "the pre-generation env Secret, which still travels",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name:            secretname.Env(secretname.KindApp, "api"),
				OwnerReferences: ownedBy("App", "api"),
			}},
			want: true,
		},
		{
			name: "an ordinary Secret whose name only looks like one",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: secretname.EnvGeneration(secretname.KindApp, "api", "9f2c1a7b40de"),
			}},
			want: true,
		},
		{
			name: "a workload's own secrets, which travel",
			secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name:            secretname.Secrets(secretname.KindApp, "api"),
				OwnerReferences: ownedBy("App", "api"),
			}},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, transferableSecret(tc.secret, map[string]bool{}))
		})
	}
}

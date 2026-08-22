package cmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/getkipper/kipper/kip/internal/service"
)

// The wait gives up after a while, and a service the reconciler refused never
// produces the Secret it waits for. Ticking the lines regardless says the thing
// came up on the one flow where it most often has not: a volume left by an
// earlier delete, and a new service of the same name landing on it.
func TestSayHowItCameUp_DoesNotTickAServiceThatNeverCameUp(t *testing.T) {
	var out bytes.Buffer
	sayHowItCameUp(&out, service.Snapshot{
		BlockedReason:  "DataWithoutCredentials",
		BlockedMessage: "service db has data in data-db-0 and no PASSWORD in db-credentials",
	}, "db", false, false)

	printed := out.String()
	assert.NotContains(t, printed, "✔", "a service that never came up was reported as up")
	assert.Contains(t, printed, "DataWithoutCredentials", "the reason the operator has to clear was not shown")
	assert.Contains(t, printed, "data-db-0", "the remedy was not shown")
}

// Nothing has reported a reason yet, which is the ordinary case for a service
// that is simply slow. Say what is known rather than inventing either answer.
func TestSayHowItCameUp_SaysWhenNothingHasReportedWhy(t *testing.T) {
	var out bytes.Buffer
	sayHowItCameUp(&out, service.Snapshot{}, "db", false, false)

	printed := out.String()
	assert.NotContains(t, printed, "✔")
	assert.Contains(t, printed, "db")
	assert.Contains(t, printed, "kip service list")
}

func TestSayHowItCameUp_ReportsAServiceThatCameUp(t *testing.T) {
	var out bytes.Buffer
	sayHowItCameUp(&out, service.Snapshot{}, "db", true, true)

	printed := out.String()
	assert.Contains(t, printed, "Credentials generated")
	assert.Contains(t, printed, "Persistent storage provisioned")
}

// The wait finds a Secret by name, and a name is all it proves: a Secret already
// standing there is the very thing the reconciler refuses over, so a service can
// be blocked and look ready at the same time.
func TestSayHowItCameUp_TakesTheRefusalOverASecretThatWasAlreadyThere(t *testing.T) {
	var out bytes.Buffer
	sayHowItCameUp(&out, service.Snapshot{
		BlockedReason:  "SecretNotOwned",
		BlockedMessage: "secret db-credentials is not owned by this service",
	}, "db", true, true)

	printed := out.String()
	assert.NotContains(t, printed, "✔", "a service the reconciler refused was reported as up")
	assert.Contains(t, printed, "SecretNotOwned")
}

// The wait has to find this service's Secret, not one merely standing under its
// name. A Secret already there is what the reconciler refuses over, and the
// refusal it writes can arrive after the wait has read the name.
func TestOwnedBy_OnlyAcceptsTheServicesOwnSecret(t *testing.T) {
	controller := true
	theirs := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "db-credentials", Namespace: "shop-prod",
		OwnerReferences: []metav1.OwnerReference{{
			Kind: "Service", Name: "db", UID: types.UID("somebody-elses"), Controller: &controller,
		}},
	}}
	unowned := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "db-credentials", Namespace: "shop-prod",
	}}
	mine := theirs.DeepCopy()
	mine.OwnerReferences[0].UID = types.UID("the-service-just-created")

	assert.False(t, ownedBy(theirs, types.UID("the-service-just-created")),
		"another service's credentials were read as this one coming up")
	assert.False(t, ownedBy(unowned, types.UID("the-service-just-created")),
		"a secret nobody owns was read as this service coming up")
	assert.True(t, ownedBy(mine, types.UID("the-service-just-created")))
}

// The reconciler writes the credentials before it makes the workload, so finding
// the Secret says nothing about whether the volume exists. A StatefulSet that
// never got made must not be reported as storage provisioned.
func TestSayHowItCameUp_DoesNotClaimStorageItNeverSaw(t *testing.T) {
	var out bytes.Buffer
	sayHowItCameUp(&out, service.Snapshot{}, "db", true, false)

	printed := out.String()
	assert.Contains(t, printed, "Credentials generated")
	assert.NotContains(t, printed, "Persistent storage provisioned",
		"a volume that was never made was reported as provisioned")
	assert.Contains(t, printed, "volume is not there yet")
}

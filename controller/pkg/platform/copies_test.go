package platform

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClaimClassForCopies(t *testing.T) {
	one, two := int32(1), int32(2)
	assert.Equal(t, "longhorn-replicated", ClaimClassForCopies(StorageLonghorn, &two))
	assert.Equal(t, "longhorn-replicated", ClaimClassForCopies("", &two), "a legacy cluster runs Longhorn")
	assert.Equal(t, "longhorn-single", ClaimClassForCopies(StorageLonghorn, &one))
	assert.Equal(t, "longhorn-single", ClaimClassForCopies(StorageLonghorn, nil), "unset creates a single copy")
	assert.Equal(t, "local-path", ClaimClassForCopies(StorageLocal, nil))
	assert.Equal(t, "local-path", ClaimClassForCopies(StorageLocal, &one))
	assert.Equal(t, "local-path", ClaimClassForCopies(StorageLocal, &two), "local storage has no second copy to give")
}

func TestWantsTwoCopies(t *testing.T) {
	one, two := int32(1), int32(2)
	assert.True(t, WantsTwoCopies(&two))
	assert.False(t, WantsTwoCopies(&one))
	assert.False(t, WantsTwoCopies(nil))
}

func TestHolderClasses(t *testing.T) {
	for _, class := range []string{"longhorn", "longhorn-single", "longhorn-replicated"} {
		assert.True(t, IsLonghornClass(class), class)
	}
	assert.False(t, IsLonghornClass("local-path"))
	assert.False(t, IsLonghornClass(""))
}

func TestOfferedCopies(t *testing.T) {
	two := int32(2)
	assert.Equal(t, int32(1), OfferedCopies(nil), "copies 2 is offered only when the admin says so")
	assert.Equal(t, int32(2), OfferedCopies(&two))
	three := int32(3)
	assert.Equal(t, int32(1), OfferedCopies(&three), "a value the CRD would refuse is no offer")
}

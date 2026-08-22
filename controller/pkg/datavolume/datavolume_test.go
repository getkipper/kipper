package datavolume_test

import (
	"testing"

	"github.com/getkipper/kipper/controller/pkg/datavolume"
)

func TestSelectorNarrowsToTheService(t *testing.T) {
	if got := datavolume.Selector("db"); got != "app=db" {
		t.Errorf("selector is %q, which would list somebody else's volumes", got)
	}
}

// The name has to be one a StatefulSet built from a claim template called
// "data", because the label alone is not proof: an App of the same name carries
// it, and so does anything else somebody labelled by hand.
func TestBelongsMatchesWhatAStatefulSetBuilds(t *testing.T) {
	for _, claim := range []string{"data-db-0", "data-db-1", "data-db-12"} {
		if !datavolume.Belongs("db", claim) {
			t.Errorf("%s is the service's own volume and was not recognised", claim)
		}
	}

	for _, claim := range []string{
		"data-db",          // no ordinal
		"data-db-",         // no ordinal
		"data-db-x",        // not an ordinal
		"data-dbs-0",       // another service whose name starts the same
		"data-cache-0",     // another service
		"db-uploads",       // the service's, but not its data volume
		"xdata-db-0",       // something else that ends in the name
		"data-db-0-backup", // a copy somebody took
	} {
		if datavolume.Belongs("db", claim) {
			t.Errorf("%s is not the service's data volume and would have been destroyed", claim)
		}
	}
}

// A service name carrying regex punctuation must not be read as a pattern. Names
// are DNS labels today, so this is a guard rather than a live case.
func TestBelongsTakesTheNameLiterally(t *testing.T) {
	if datavolume.Belongs("d.b", "data-dxb-0") {
		t.Error("the name was read as a pattern, so one service matched another's volume")
	}
}

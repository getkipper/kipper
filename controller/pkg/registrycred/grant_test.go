package registrycred

import (
	"errors"
	"testing"
)

func listOf(entries ...Entry) []Entry { return entries }

func TestGrantIsAdditiveAndIdempotent(t *testing.T) {
	entries, err := Grant(listOf(Entry{Name: "ghcr", AllowedProjects: []string{"shop"}}), "ghcr", []string{"blog", "shop"})
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if !entries[0].AllowsProject("shop") || !entries[0].AllowsProject("blog") {
		t.Errorf("allowed projects = %v", entries[0].AllowedProjects)
	}
	if len(entries[0].AllowedProjects) != 2 {
		t.Errorf("granting a project twice stored it twice: %v", entries[0].AllowedProjects)
	}
}

func TestRevokeRemovesOnlyTheProjectNamed(t *testing.T) {
	entries, err := Revoke(listOf(Entry{Name: "ghcr", AllowedProjects: []string{"shop", "blog"}}), "ghcr", []string{"shop"})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if entries[0].AllowsProject("shop") || !entries[0].AllowsProject("blog") {
		t.Errorf("allowed projects = %v", entries[0].AllowedProjects)
	}
}

// Revoking the last project is a decision, and it is stored as an empty list
// rather than as nothing, so the document says what was decided.
func TestRevokingTheLastProjectLeavesAnEmptyList(t *testing.T) {
	entries, err := Revoke(listOf(Entry{Name: "ghcr", AllowedProjects: []string{"shop"}}), "ghcr", []string{"shop"})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if entries[0].AllowedProjects == nil {
		t.Error("the decision read back as no decision at all")
	}
}

func TestChangeOnACredentialThatIsNotThereIsRecognisable(t *testing.T) {
	_, err := Grant(listOf(Entry{Name: "ghcr"}), "quay", []string{"shop"})

	var unknown *UnknownRegistryError
	if !errors.As(err, &unknown) {
		t.Fatalf("a caller cannot tell this from any other failure: %v", err)
	}
}

func TestGrantRefusesABlankProject(t *testing.T) {
	if _, err := Grant(listOf(Entry{Name: "ghcr"}), "ghcr", []string{""}); err == nil {
		t.Fatal("granted a blank project, which AllowsProject can never match")
	}
}

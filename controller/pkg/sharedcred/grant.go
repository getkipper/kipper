package sharedcred

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// Grant allows projects to build with the named credential, keeping the ones
// already allowed. Granting is additive because a grant that replaced the list
// would revoke every other project as a side effect of allowing one.
//
// It fails when the credential is not there, rather than reporting a success
// that leaves the operator believing a build will now work.
func Grant(entries []Entry, name string, projects []string) ([]Entry, error) {
	return change(entries, name, projects, func(e *Entry, project string) {
		if !e.AllowsProject(project) {
			e.AllowedProjects = append(e.AllowedProjects, project)
		}
	})
}

// Revoke stops projects building with the named credential. Projects that are
// not on the list are left alone, so revoking twice is not an error.
func Revoke(entries []Entry, name string, projects []string) ([]Entry, error) {
	return change(entries, name, projects, func(e *Entry, project string) {
		kept := make([]string, 0, len(e.AllowedProjects))
		for _, p := range e.AllowedProjects {
			if p != project {
				kept = append(kept, p)
			}
		}
		e.AllowedProjects = kept
	})
}

func change(entries []Entry, name string, projects []string, apply func(*Entry, string)) ([]Entry, error) {
	entry := Find(entries, name)
	if entry == nil {
		return nil, &UnknownCredentialError{Name: name}
	}
	for _, project := range projects {
		if project == "" {
			// AllowsProject can never match one, so storing it would look like
			// a grant and behave like nothing.
			return nil, fmt.Errorf("a project name is required")
		}
		apply(entry, project)
	}
	if entry.AllowedProjects == nil {
		// Every write records a decision, so that revoking the last project
		// reads back as "nobody" rather than as a credential nobody has
		// decided about, which the upgrade would seed from the apps again.
		entry.AllowedProjects = []string{}
	}
	return entries, nil
}

// UnknownCredentialError is a change asked for against a name the list does not
// hold. It is its own type so a caller can tell it from a failure to read or
// write, and answer with something more useful than the name is not there.
type UnknownCredentialError struct {
	Name string
}

func (e *UnknownCredentialError) Error() string {
	return fmt.Sprintf("no shared git credential named %q is configured", e.Name)
}

// Seed fills an allow-list nobody has ever decided with the projects already
// building with that credential. It reports which credentials it granted, and
// whether it changed anything at all.
//
// A credential written before allow-lists existed carries no list, so on a
// cluster upgraded into the guard the first build of an app that was working
// fails. Seeding writes down what the cluster was already doing.
//
// What it must not do is read every empty list as that. An admin who revokes the
// last project leaves an empty list too, and so does a credential added and not
// yet granted, and seeding either from the apps that reference it would hand
// back exactly what somebody had withheld. So the two are stored apart: a list
// nobody has decided is absent, and one decided to be empty is an empty list.
// Seed only fills. What is left undecided is closed by CloseUndecided, once the
// upgrade has replaced the writer whose absent lists caused all this.
func Seed(entries []Entry, usage map[string][]string) ([]Entry, []string, bool) {
	var seeded []string
	for i := range entries {
		if entries[i].AllowedProjects != nil {
			continue
		}
		if projects := usage[entries[i].Name]; len(projects) > 0 {
			entries[i].AllowedProjects = append([]string(nil), projects...)
			seeded = append(seeded, entries[i].Name)
		}
	}
	return entries, seeded, len(seeded) > 0
}

// CloseUndecided records that nobody may build with the credentials still
// undecided, which is what ends the migration for a cluster.
//
// It is separate from Seed because it can only be done once the old writer is
// gone. Deciding a credential nothing references is what stops the inference
// running again, and doing it while builds can still start would freeze a
// snapshot: an app pointed at that credential a second later was building
// perfectly well under the old rules, and a decision already recorded is one
// Seed will not revisit.
func CloseUndecided(entries []Entry) ([]Entry, bool) {
	changed := false
	for i := range entries {
		if entries[i].AllowedProjects == nil {
			entries[i].AllowedProjects = []string{}
			changed = true
		}
	}
	return entries, changed
}

// Update applies mutate to the stored list and writes the result back, retrying
// when another writer changed the list in between.
//
// The live Secret is edited rather than replaced. It is one object shared by
// every writer, so building a fresh one to hold the list drops whatever else is
// on it, and writing without the version it was read at drops whatever another
// writer did in the meantime. A mutate that returns an error writes nothing.
func Update(ctx context.Context, client kubernetes.Interface, mutate func([]Entry) ([]Entry, error)) error {
	secrets := client.CoreV1().Secrets(Namespace)
	// AlreadyExists as well as Conflict: two writers on a cluster with no list
	// yet both find it missing, and the one that loses the race has to re-read
	// and apply its change to what the other wrote rather than give up.
	writeRace := func(err error) bool {
		return k8serrors.IsConflict(err) || k8serrors.IsAlreadyExists(err)
	}
	return retry.OnError(retry.DefaultRetry, writeRace, func() error {
		live, err := secrets.Get(ctx, ConfigSecretName, metav1.GetOptions{})
		missing := k8serrors.IsNotFound(err)
		if err != nil && !missing {
			return fmt.Errorf("reading shared git credentials: %w", err)
		}
		if missing {
			live = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name:      ConfigSecretName,
				Namespace: Namespace,
				Labels:    map[string]string{managedByLabel: managedByValue},
			}}
		}

		var entries []Entry
		if stored := live.Data[dataKey]; len(stored) > 0 {
			if err := json.Unmarshal(stored, &entries); err != nil {
				return fmt.Errorf("parsing shared git credentials: %w", err)
			}
		}

		updated, err := mutate(entries)
		if err != nil {
			return err
		}

		data, err := json.Marshal(updated) //nolint:gosec // tokens are intentionally stored in a K8s Secret
		if err != nil {
			return err
		}
		if bytes.Equal(live.Data[dataKey], data) {
			// Every write is a new resourceVersion, an audit entry and a
			// conflict for whoever else is writing, and an upgrade that
			// changes nothing runs on every cluster.
			return nil
		}
		if live.Data == nil {
			live.Data = map[string][]byte{}
		}
		live.Data[dataKey] = data

		if missing {
			_, err = secrets.Create(ctx, live, metav1.CreateOptions{})
			return err
		}
		_, err = secrets.Update(ctx, live, metav1.UpdateOptions{})
		return err
	})
}

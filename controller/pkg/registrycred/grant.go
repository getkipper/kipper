package registrycred

import "fmt"

// Grant allows projects to pull with the named credential, keeping the ones
// already allowed. Granting is additive because a grant that replaced the list
// would revoke every other project as a side effect of allowing one.
//
// That is what `kip registry add --allow-project` does, and it keeps doing it:
// the flag is published surface documented as replacing, and a caller who asked
// the replacement surface to drop a project should not silently keep it. These
// are the verbs for adding one.
func Grant(entries []Entry, name string, projects []string) ([]Entry, error) {
	return change(entries, name, projects, func(e *Entry, project string) {
		if !e.AllowsProject(project) {
			e.AllowedProjects = append(e.AllowedProjects, project)
		}
	})
}

// Revoke stops projects pulling with the named credential. Projects that are not
// on the list are left alone, so revoking twice is not an error.
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
		return nil, &UnknownRegistryError{Name: name}
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
		entry.AllowedProjects = []string{}
	}
	return entries, nil
}

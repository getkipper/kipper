package installer

import "testing"

// The NotReady alert finds the services stranded on a node through their
// claims' volumes and reads Longhorn's replicas to say where a current copy
// is. It needs to read both, and no more.
func TestConsoleCanReadWhatTheStrandedAlertNeeds(t *testing.T) {
	console := parseConsoleClusterRole(t)
	for _, want := range []struct{ group, resource, verb string }{
		{"", "persistentvolumes", "get"},
		{"", "persistentvolumes", "list"},
		{"longhorn.io", "replicas", "get"},
		{"longhorn.io", "replicas", "list"},
	} {
		if !grants(console, want.group, want.resource, want.verb) {
			t.Errorf("console-api cannot %s %s/%s", want.verb, want.group, want.resource)
		}
	}
	for _, verb := range []string{"watch", "create", "update", "patch", "delete"} {
		if grants(console, "", "persistentvolumes", verb) {
			t.Errorf("console-api may %s persistent volumes, but only reads them", verb)
		}
	}
}

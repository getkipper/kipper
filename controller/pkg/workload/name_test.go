package workload

import "testing"

// The message is what an operator acts on, so it has to read as English for
// every kind that can hold a name.
func TestNameTakenError_ReadsAsEnglishForEveryHolder(t *testing.T) {
	for kind, want := range map[string]string{
		"app":      `the name "checkout" is already used by an app in this environment`,
		"function": `the name "checkout" is already used by a function in this environment`,
		"job":      `the name "checkout" is already used by a job in this environment`,
	} {
		t.Run(kind, func(t *testing.T) {
			got := NameTakenError{Name: "checkout", Kind: kind}.Error()
			if len(got) < len(want) || got[:len(want)] != want {
				t.Fatalf("message = %q, want it to start %q", got, want)
			}
		})
	}
}

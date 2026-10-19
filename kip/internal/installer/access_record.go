package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

const (
	accessRecordConfigMap = "kipper-access"
	accessRecordKey       = "record"
	accessRecordVersion   = 1
	accessHistoryLimit    = 100
)

// ErrInvalidAccessRecord reports an access record that is present but cannot
// be used. Only show and reset work with one.
var ErrInvalidAccessRecord = errors.New("the operator access record is invalid")

// AccessRecord is the cluster's operator access list in the kipper-access
// ConfigMap. History is bounded to the last 100 changes.
type AccessRecord struct {
	Version    int            `json:"version"`
	Generation int            `json:"generation"`
	Sources    []AccessSource `json:"sources"`
	History    []AccessChange `json:"history,omitempty"`
}

// AccessSource is one operator prefix and who added it.
type AccessSource struct {
	Prefix  string `json:"prefix"`
	Note    string `json:"note,omitempty"`
	AddedBy string `json:"addedBy,omitempty"`
	AddedAt string `json:"addedAt,omitempty"`
}

// AccessChange is one history entry. Nodes maps node names to the outcome
// on that node, and is empty until the change has run.
type AccessChange struct {
	At         string            `json:"at"`
	By         string            `json:"by,omitempty"`
	Action     string            `json:"action"`
	Prefixes   []string          `json:"prefixes,omitempty"`
	Generation int               `json:"generation"`
	Nodes      map[string]string `json:"nodes,omitempty"`
}

// Prefixes returns the record's operator prefixes in record order, or an error
// if any prefix fails ParseAccessPrefix.
func (r AccessRecord) Prefixes() ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(r.Sources))
	for _, s := range r.Sources {
		p, err := ParseAccessPrefix(s.Prefix)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

func (r AccessRecord) validate() error {
	if r.Version != accessRecordVersion {
		return fmt.Errorf("this version of kip cannot read access record version %d", r.Version)
	}
	if len(r.Sources) == 0 {
		return errors.New("it lists no operator prefixes")
	}
	if r.Generation < 1 {
		return fmt.Errorf("generation %d must be at least 1", r.Generation)
	}
	seen := map[string]bool{}
	for _, s := range r.Sources {
		if _, err := ParseAccessPrefix(s.Prefix); err != nil {
			return err
		}
		if seen[s.Prefix] {
			return fmt.Errorf("it lists %s twice", s.Prefix)
		}
		seen[s.Prefix] = true
	}
	return nil
}

// ReadAccessRecord reads the kipper-access ConfigMap. With no error, present
// is false when kubectl returns no data. A command failure returns an error
// and false. Invalid returned data sets present to true and wraps
// ErrInvalidAccessRecord.
func ReadAccessRecord(client CommandRunner) (AccessRecord, bool, error) {
	out, err := client.Run(fmt.Sprintf("kubectl -n %s get configmap %s --ignore-not-found -o jsonpath='{.data}' 2>/dev/null",
		nodeRecordsNamespace, accessRecordConfigMap))
	if err != nil {
		return AccessRecord{}, false, fmt.Errorf("reading the operator access record: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return AccessRecord{}, false, nil
	}
	var data map[string]string
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		return AccessRecord{}, true, fmt.Errorf("%w: %s/%s is not a map", ErrInvalidAccessRecord, nodeRecordsNamespace, accessRecordConfigMap)
	}
	value, ok := data[accessRecordKey]
	if !ok {
		return AccessRecord{}, true, fmt.Errorf("%w: %s/%s has no %s entry", ErrInvalidAccessRecord, nodeRecordsNamespace, accessRecordConfigMap, accessRecordKey)
	}
	var rec AccessRecord
	if err := json.Unmarshal([]byte(value), &rec); err != nil {
		return AccessRecord{}, true, fmt.Errorf("%w: %v", ErrInvalidAccessRecord, err)
	}
	if err := rec.validate(); err != nil {
		return AccessRecord{}, true, fmt.Errorf("%w: %v", ErrInvalidAccessRecord, err)
	}
	return rec, true, nil
}

// WriteAccessRecord creates or replaces the record, keeping the last 100
// history entries. It validates the record before writing and returns an error
// if validation or a Kubernetes command fails.
func WriteAccessRecord(client CommandRunner, rec AccessRecord) error {
	if err := rec.validate(); err != nil {
		return fmt.Errorf("writing the operator access record: %w", err)
	}
	if n := len(rec.History); n > accessHistoryLimit {
		rec.History = rec.History[n-accessHistoryLimit:]
	}
	value, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encoding the operator access record: %w", err)
	}
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"labels": map[string]string{"app.kubernetes.io/managed-by": "kipper"}},
		"data":     map[string]string{accessRecordKey: string(value)},
	})
	if err != nil {
		return fmt.Errorf("encoding the operator access record: %w", err)
	}
	cmd := fmt.Sprintf("{ kubectl -n %[1]s create configmap %[2]s >/dev/null 2>&1 || true; } && kubectl -n %[1]s patch configmap %[2]s --type merge --patch-file /dev/stdin >/dev/null",
		nodeRecordsNamespace, accessRecordConfigMap)
	if _, err := client.RunStdin(cmd, strings.NewReader(string(patch))); err != nil {
		return fmt.Errorf("writing the operator access record: %w", err)
	}
	return nil
}

// DeleteAccessRecord removes the record. A missing record is not an error.
func DeleteAccessRecord(client CommandRunner) error {
	if _, err := client.Run(fmt.Sprintf("kubectl -n %s delete configmap %s --ignore-not-found >/dev/null", nodeRecordsNamespace, accessRecordConfigMap)); err != nil {
		return fmt.Errorf("deleting the operator access record: %w", err)
	}
	return nil
}

// AccessNodeHeader is the generation of the committed access files on one
// node. Present also covers an operators.nft with a newer valid header. Sources counts
// the operator prefixes in its committed operators.nft.
type AccessNodeHeader struct {
	Name       string
	Generation int
	Present    bool
	Sources    int
}

// NextAccessGeneration is one above both the record and every node's
// committed header.
func NextAccessGeneration(record int, nodes []AccessNodeHeader) int {
	highest := record
	for _, n := range nodes {
		if n.Present && n.Generation > highest {
			highest = n.Generation
		}
	}
	return highest + 1
}

// StaleAccessRecord refuses a record older than any node's committed
// generation, because that node may hold a revoke the record lost.
func StaleAccessRecord(record int, nodes []AccessNodeHeader) error {
	for _, n := range nodes {
		if n.Present && n.Generation > record {
			return fmt.Errorf("the record (generation %[1]d) is older than %[2]s (generation %[3]d), so it may hold prefixes that generation %[3]d revoked. Run 'kip cluster access apply --accept-older-record' to see the difference and restore the record's list, or restore the cluster settings backup that matches",
				record, n.Name, n.Generation)
		}
	}
	return nil
}

// OrphanedAccessTable refuses when the record is gone but a node still
// holds a committed table.
func OrphanedAccessTable(nodes []AccessNodeHeader) error {
	for _, n := range nodes {
		if n.Present {
			return fmt.Errorf("the operator access record is gone but %s still restricts SSH and 6443 to %s. Run 'kip cluster access reset' to open them, or restore the cluster settings backup", n.Name, plural(n.Sources, "one source", fmt.Sprintf("%d sources", n.Sources)))
		}
	}
	return nil
}

// AccessWidening lists, per node, the record's prefixes that the node's live
// operator sets do not admit in full. A node with no live prefixes, as after
// a closed boot or with no table, is treated as admitting nothing for this
// comparison. Nodes that already admit
// every prefix are left out.
func AccessWidening(record []netip.Prefix, live map[string][]netip.Prefix) map[string][]string {
	out := map[string][]string{}
	for node, have := range live {
		var missing []string
		for _, p := range collapse(record) {
			if !covers(have, p) {
				missing = append(missing, p.String())
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			out[node] = missing
		}
	}
	return out
}

// covers reports whether the prefixes in have admit every address in p.
func covers(have []netip.Prefix, p netip.Prefix) bool {
	want := prefixRange(p)
	for _, r := range mergedRanges(have) {
		if r.lo.BitLen() == want.lo.BitLen() && r.lo.Compare(want.lo) <= 0 && r.hi.Compare(want.hi) >= 0 {
			return true
		}
	}
	return false
}

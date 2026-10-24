package platform

import (
	"encoding/json"
	"time"
)

// Annotations that a copies conversion keeps on a volume's claim. Each holds
// one JSON record. The claim outlives the Longhorn volume and tenants cannot
// write it.
const (
	// CopyChangeStopAnnotation holds a CopyChangeStop: Kipper stopped what it
	// owns that uses the claim, so the volume detaches for a copy change.
	CopyChangeStopAnnotation = "kipper.run/copy-change-stop"
	// CopiesBackoffAnnotation holds a CopiesBackoff: how long a conversion waits
	// before asking Longhorn again for a copy it refused to build.
	CopiesBackoffAnnotation = "kipper.run/copies-backoff"
)

// CopyChangeStop records a copy change's stop of Kipper's workloads using a
// claim. Replicas is the StatefulSet count to restore, with a minimum of one,
// or nil for a claim without a StatefulSet. At is the stop time in RFC 3339.
type CopyChangeStop struct {
	Replicas *int32 `json:"replicas,omitempty"`
	At       string `json:"at"`
}

// CopiesBackoff delays requests to build a copy until Until, an RFC 3339
// timestamp. Target identifies the requested copy count. Failures counts
// consecutive refusals, and Error holds the latest error message.
type CopiesBackoff struct {
	Target   int64  `json:"target"`
	Until    string `json:"until"`
	Failures int    `json:"failures"`
	Error    string `json:"error,omitempty"`
}

// BackoffDelay returns the delay after consecutive refusals to build a copy.
// It starts at one minute, doubles with each refusal and stops at 30 minutes.
// A failure count of one or less returns one minute.
func BackoffDelay(failures int) time.Duration {
	delay := time.Minute
	for i := 1; i < failures && delay < 30*time.Minute; i++ {
		delay *= 2
	}
	return min(delay, 30*time.Minute)
}

// ReadRecord decodes the JSON annotation at key into the destination into,
// which must be a non-nil pointer accepted by json.Unmarshal. It returns
// false and nil when the key is absent, leaving the destination unchanged.
// A present key returns true, including when decoding returns an error.
func ReadRecord(annotations map[string]string, key string, into any) (bool, error) {
	raw, ok := annotations[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal([]byte(raw), into)
}

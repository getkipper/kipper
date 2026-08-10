package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UsagePlanSpec defines rate and quota limits that API keys can reference.
// A plan lives in the same environment namespace as the apps and keys it
// governs.
type UsagePlanSpec struct {
	// DisplayName is the human-readable plan name shown in the console.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Rate is the steady-state request rate in requests per second,
	// enforced per authz replica (best effort, AWS-style).
	// +kubebuilder:validation:Minimum=1
	Rate int `json:"rate"`

	// Burst is the token-bucket size: how many requests may arrive at
	// once before the rate applies.
	// +kubebuilder:validation:Minimum=1
	Burst int `json:"burst"`

	// Quota caps total requests per calendar period. Unset means no
	// period quota, only the rate limit.
	// +optional
	Quota *PlanQuota `json:"quota,omitempty"`
}

// PlanQuota caps total requests over a calendar period. Counting is
// eventually consistent: counters accumulate in authz memory and flush in
// batches, so a period may over-admit by roughly replicas x flush-window x
// rate.
type PlanQuota struct {
	// Requests is the number of requests allowed per period.
	// +kubebuilder:validation:Minimum=1
	Requests int64 `json:"requests"`

	// Period is the calendar window the quota applies to.
	// +kubebuilder:validation:Enum=day;week;month
	Period string `json:"period"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Rate",type=integer,JSONPath=`.spec.rate`
// +kubebuilder:printcolumn:name="Burst",type=integer,JSONPath=`.spec.burst`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// UsagePlan is the Schema for the usageplans API.
type UsagePlan struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec UsagePlanSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// UsagePlanList contains a list of UsagePlan.
type UsagePlanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UsagePlan `json:"items"`
}

// ApiKeySpec identifies one API key: who it belongs to, which plan
// throttles it, and which apps it may call. The key's secret value is never
// stored; only its SHA-256 digest is. The secret carries roughly 206 bits of
// entropy (40 characters from a 36-symbol alphabet), so the plain digest
// cannot be brute-forced and no separate HMAC secret is needed.
type ApiKeySpec struct {
	// DisplayName is the human-readable key name (e.g. the consumer it
	// was issued to). It is forwarded as the X-Kipper-Key-Name header, so
	// its length is capped here to bound the header size. authz refuses to
	// forward a name carrying control bytes, so a direct CR write cannot
	// break the route with one.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Plan is the name of the UsagePlan in the same namespace that
	// throttles this key.
	Plan string `json:"plan"`

	// Enabled gates the key: a disabled key is rejected without deleting
	// its usage history.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Apps lists the app names in this namespace the key may call. Empty
	// means every key-gated app in the namespace.
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`
	// +kubebuilder:validation:items:MaxLength=63
	// +optional
	Apps []string `json:"apps,omitempty"`

	// Prefix is the key's non-secret lookup handle (the part before the
	// last underscore of the issued key). It routes validation to this CR
	// without scanning all key hashes. Two keys must never share a prefix;
	// authz denies a prefix that matches more than one CR.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]{8}$`
	Prefix string `json:"prefix"`

	// HashSHA256 is the hex SHA-256 digest of the full issued key.
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	HashSHA256 string `json:"hashSHA256"`

	// ExpiresAt is an optional instant after which the key is rejected on
	// the same uniform 401 path as a disabled key. Empty means the key
	// never expires.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
}

// IsEnabled resolves the Enabled default (true).
func (k *ApiKey) IsEnabled() bool {
	return k.Spec.Enabled == nil || *k.Spec.Enabled
}

// IsExpired reports whether the key's optional expiry has passed as of now.
func (k *ApiKey) IsExpired(now time.Time) bool {
	return k.Spec.ExpiresAt != nil && now.After(k.Spec.ExpiresAt.Time)
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Plan",type=string,JSONPath=`.spec.plan`
// +kubebuilder:printcolumn:name="Prefix",type=string,JSONPath=`.spec.prefix`
// +kubebuilder:printcolumn:name="Enabled",type=boolean,JSONPath=`.spec.enabled`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ApiKey is the Schema for the apikeys API.
type ApiKey struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ApiKeySpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ApiKeyList contains a list of ApiKey.
type ApiKeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ApiKey `json:"items"`
}

// UsageRollupSpec is one key's request counters for one calendar day.
// Rollups are always daily; week and month quotas sum the days they cover.
// Daily granularity keeps cardinality bounded (keys x retained days) while
// staying fine-grained enough for any billing period.
//
// Counters are cumulative. Authz replicas buffer increments in memory and
// flush them in batches with optimistic concurrency, so totals are
// eventually consistent, never per-request writes.
type UsageRollupSpec struct {
	// KeyPrefix identifies the ApiKey (spec.prefix) these counters
	// belong to.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]{8}$`
	KeyPrefix string `json:"keyPrefix"`

	// Day is the calendar day in ISO form (2026-07-07), UTC.
	// +kubebuilder:validation:Pattern=`^\d{4}-\d{2}-\d{2}$`
	Day string `json:"day"`

	// Allowed counts requests that passed validation, rate, and quota.
	// Never negative: the quota gate sums these, so a negative value
	// would subtract usage.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Allowed int64 `json:"allowed,omitempty"`

	// DeniedRate counts requests rejected by the token bucket.
	// +kubebuilder:validation:Minimum=0
	// +optional
	DeniedRate int64 `json:"deniedRate,omitempty"`

	// DeniedQuota counts requests rejected by the period quota.
	// +kubebuilder:validation:Minimum=0
	// +optional
	DeniedQuota int64 `json:"deniedQuota,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Key",type=string,JSONPath=`.spec.keyPrefix`
// +kubebuilder:printcolumn:name="Day",type=string,JSONPath=`.spec.day`
// +kubebuilder:printcolumn:name="Allowed",type=integer,JSONPath=`.spec.allowed`

// UsageRollup is the Schema for the usagerollups API.
type UsageRollup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec UsageRollupSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// UsageRollupList contains a list of UsageRollup.
type UsageRollupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UsageRollup `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&UsagePlan{}, &UsagePlanList{},
		&ApiKey{}, &ApiKeyList{},
		&UsageRollup{}, &UsageRollupList{},
	)
}

package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// ChangeKind is what an apply would do to one field.
type ChangeKind int

const (
	// Added is a field the manifest carries and the cluster does not.
	Added ChangeKind = iota
	// Changed is a field both carry, with different values.
	Changed
	// Cleared is a field live in the cluster that the manifest does not carry.
	// Applying removes it, because a spec is replaced rather than merged.
	Cleared
	// Reset is a field the manifest does not carry whose live value the cluster
	// will replace with its schema default. The value the operator set goes
	// either way, so it is confirmed like a clear rather than reported like an
	// ordinary change — omitting `replicas` from a manifest scales a live app of
	// four back to one, and nothing supplied the one.
	Reset
)

func (k ChangeKind) String() string {
	switch k {
	case Added:
		return "added"
	case Changed:
		return "changed"
	case Reset:
		return "reset to default"
	default:
		return "cleared"
	}
}

// Change is one field an apply would touch.
type Change struct {
	// Path is dotted, as it reads in a kipper.yaml: route.redirectFrom.
	Path string
	Kind ChangeKind
	// Live and New are rendered for display and truncated. They are here to be
	// read, not parsed.
	Live string
	New  string
}

// maxRenderedValue keeps a diff line readable. A value longer than this is
// worth knowing the shape of rather than the whole of.
const maxRenderedValue = 60

// DiffSpec reports what replacing live with desired would do, field by field.
//
// Cleared is the reason this exists. `kip apply` assigns a spec wholesale, so
// every field the manifest does not carry is removed, and nothing said so at
// the point it mattered: the old diff printed "exists, will be updated" and
// named nothing. Two people worked that rule out from behaviour rather than
// from the tool.
//
// defaults are the CRD's own, by dotted path. A field the manifest omits whose
// live value is the schema default is not going anywhere — assigning a spec
// without it makes admission write the same value back — so it is not reported
// at all. One whose live value differs from the default is reported as taking
// that default rather than as being cleared, because that is what happens.
// Without this, a manifest that leaves an optional field out is told it is
// about to destroy one, and apply refuses work that is entirely ordinary.
//
// preserved names paths apply carries forward rather than replacing, so they
// are not reported as cleared. A git app's built image is one: it is build
// output the controller owns, and apply keeps it precisely so an apply of a
// git-only spec cannot reset a running app to the build placeholder.
func DiffSpec(live, desired map[string]interface{}, preserved []string, defaults map[string]interface{}) []Change {
	keep := make(map[string]struct{}, len(preserved))
	for _, p := range preserved {
		keep[p] = struct{}{}
	}
	var out []Change
	diffInto(&out, "", live, desired, keep, defaults)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func diffInto(out *[]Change, prefix string, live, desired map[string]interface{}, keep map[string]struct{}, defaults map[string]interface{}) {
	for _, k := range sortedKeys(live, desired) {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if _, skip := keep[path]; skip {
			continue
		}

		liveVal, inLive := live[k]
		newVal, inNew := desired[k]

		liveMap, liveIsMap := liveVal.(map[string]interface{})
		newMap, newIsMap := newVal.(map[string]interface{})

		if def, declared := defaults[path]; declared && inLive && !inNew {
			if sameValue(liveVal, def) {
				continue // admission puts it straight back
			}
			*out = append(*out, Change{Path: path, Kind: Reset, Live: display(path, liveVal), New: display(path, def)})
			continue
		}

		switch {
		// Two maps: recurse, so a change reads as route.redirectFrom rather
		// than as the whole route block being different.
		case liveIsMap && newIsMap:
			diffInto(out, path, liveMap, newMap, keep, defaults)
		// The manifest drops a whole block. Report the leaves inside it: an
		// operator needs to know which values go, not that "route" changed.
		case liveIsMap && !inNew:
			// The whole block goes, so nothing inside it takes a default:
			// admission does not rebuild an absent parent because a child
			// declares one.
			diffInto(out, path, liveMap, map[string]interface{}{}, keep, nil)
		case newIsMap && !inLive:
			diffInto(out, path, map[string]interface{}{}, newMap, keep, defaults)
		case inLive && !inNew:
			*out = append(*out, Change{Path: path, Kind: Cleared, Live: display(path, liveVal)})
		case !inLive && inNew:
			*out = append(*out, Change{Path: path, Kind: Added, New: display(path, newVal)})
		default:
			if !sameValue(liveVal, newVal) {
				*out = append(*out, Change{Path: path, Kind: Changed, Live: display(path, liveVal), New: display(path, newVal)})
			}
		}
	}
}

func sortedKeys(a, b map[string]interface{}) []string {
	seen := map[string]struct{}{}
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sameValue reports whether two spec values are equal, on the whole value
// rather than on what is displayed for it.
//
// Display must not decide equality, and this used to. render truncates at 60
// runes, so two values sharing a long prefix compared equal and the diff
// reported no change while apply replaced the field anyway. It also flattens:
// a one-element slice holding "a, b" renders exactly like a two-element slice
// holding "a" and "b".
//
// JSON is the canonical form because it is what both sides came from and it
// settles the type question for free: a manifest is YAML, so its numbers arrive
// as float64, while the API server answers int64, and Go marshals a whole float
// without a fractional part. So 3000 is 3000 whichever type carries it, without
// a rule saying so.
func sameValue(a, b interface{}) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		// Nothing in a spec should fail to marshal. If one does, treat the two
		// as different rather than silently equal: a spurious "changed" line is
		// recoverable, a missed clear is not.
		return false
	}
	return bytes.Equal(ja, jb)
}

// hidden stands in for a value that is not printed.
const hidden = "(value hidden)"

// redacted marks the place in a URL something was taken out of, so a reader can
// tell a scrubbed URL from one that never carried anything.
const redacted = "***"

// ScrubURLCredential removes what an http(s) URL can carry a credential in,
// leaving the scheme, host and path legible. A git URL is worth reading in a
// diff; the userinfo, the query and the fragment are not.
//
// The whole userinfo goes, not just the password. A token is a valid username
// on its own — https://ghp_xxxx@github.com/acme/shop.git is what a personal
// access token looks like in a URL — so a rule that needs a colon to find a
// credential prints the ones carried without one.
//
// The query and the fragment go for the same reason one step further along: a
// provider that takes a token as a parameter puts it after the path, where
// removing the userinfo finds nothing. Neither carries anything a diff needs, so
// they are marked rather than parsed — deciding which parameter is the secret
// means keeping a list of every provider's spelling, and the one not on the list
// is the one that gets printed.
//
// ssh:// and the scp-style git@host:path are left alone. Their username is the
// remote's convention rather than a secret, and ssh does not carry one here.
//
// A string that will not parse is hidden rather than printed. Failing to parse
// is not evidence that there is nothing in it.
func ScrubURLCredential(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		// Only something that looked like a URL with an authority could have
		// been carrying userinfo. A scp-style remote has no "://" and its
		// username is the remote's convention.
		if strings.Contains(raw, "://") && strings.Contains(raw, "@") {
			return hidden
		}
		return raw
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return raw
	}
	if parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == "" {
		return raw
	}

	hadUser := parsed.User != nil
	parsed.User = nil
	if parsed.RawQuery != "" {
		parsed.RawQuery = redacted
	}
	if parsed.Fragment != "" {
		parsed.Fragment = redacted
		parsed.RawFragment = ""
	}
	out := parsed.String()
	if hadUser {
		// Marked by hand rather than through url.User, which would
		// percent-encode the marker and leave a line nobody can read.
		out = strings.Replace(out, "://", "://"+redacted+"@", 1)
	}
	return out
}

// printable are the spec paths whose values are printed. Everything else is
// named and hidden.
//
// This is an allowlist because the other way round does not converge. A spec
// carries arbitrary operator text — an environment variable, a build argument,
// a command line, a function's source, an element of some array a later CRD
// adds — and a list of the places a credential can hide grew by two or three
// every time somebody looked. A list of the places one cannot hide is short,
// and a field added tomorrow is withheld until somebody decides otherwise,
// which is the direction to be wrong in.
//
// The path is what the warning is for. Knowing that route.rateLimit was 100 is
// a nicety; knowing that spec.env.DATABASE_URL is about to go is the point.
var printable = map[string]struct{}{
	// Workload shape.
	"image": {}, "replicas": {}, "port": {}, "runtime": {},
	"schedule": {}, "storage": {}, "size": {}, "type": {},
	"version": {}, "noSecurityHeaders": {}, "source.handler": {},

	// Resources, on every kind that carries them.
	"resources.cpuRequest": {}, "resources.cpuLimit": {},
	"resources.memoryRequest": {}, "resources.memoryLimit": {},
	"resources.profile":      {},
	"git.buildResources.cpu": {}, "git.buildResources.memory": {},

	// Scaling.
	"autoscale.enabled": {}, "autoscale.minReplicas": {}, "autoscale.maxReplicas": {},
	"autoscale.cpuTarget": {}, "autoscale.memoryTarget": {},

	// Routing. route.path is absent on purpose: an unguessable prefix is a
	// documented way to protect a webhook, so it is the operator's secret as
	// often as it is their layout.
	"route.host": {}, "route.group": {}, "route.rateLimit": {},
	"route.noSecurityHeaders": {}, "route.requireApiKey": {},
	"route.noInstanceHeader": {}, "route.redirectFrom": {},

	// Build inputs that name rather than carry.
	"git.branch": {}, "git.context": {}, "git.dockerfilePath": {},
	"git.credentialsSecret": {},
}

// display renders a value for a person to read, hides it, or scrubs it.
//
// Some spec values are the operator's own text and can carry a credential
// outright: an environment variable holding a token, a build argument passed a
// token, a command line with a password on it, a git URL with one in its
// userinfo. kip warns about exactly that when one is set. Printing them here
// would put them into terminal scrollback and, in a GitOps job, into durable CI
// logs, so the path is named and the value is not.
//
// A git URL is the exception worth making: the host and repository are most of
// why the line is there, so only the userinfo goes.
func display(path string, v interface{}) string {
	if path == "git.url" {
		if u, ok := v.(string); ok {
			return render(ScrubURLCredential(u))
		}
		return hidden
	}
	if _, ok := printable[path]; ok {
		return render(v)
	}
	return hidden
}

// render turns a value into one short line, for a person to read. It is never
// the basis of a comparison — see sameValue.
func render(v interface{}) string {
	var s string
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		s = t
	case []interface{}:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = render(e)
		}
		s = "[" + strings.Join(parts, ", ") + "]"
	case float64:
		// YAML gives numbers as float64 and the API server gives int64. Render
		// a whole float as an integer so the two agree.
		if t == float64(int64(t)) {
			s = fmt.Sprintf("%d", int64(t))
		} else {
			s = fmt.Sprintf("%v", t)
		}
	default:
		s = fmt.Sprintf("%v", t)
	}
	// Runes, not bytes. Slicing a UTF-8 string by byte can cut a character in
	// half, and these values carry hostnames and env values that are not all
	// ASCII.
	if r := []rune(s); len(r) > maxRenderedValue {
		return string(r[:maxRenderedValue-1]) + "…"
	}
	return s
}

// Clears returns the changes that take away a value the manifest does not
// carry, whether they remove it outright or put the cluster's default in its
// place. Both lose what the operator set, and neither was asked for.
func Clears(changes []Change) []Change {
	var out []Change
	for _, c := range changes {
		if c.Kind == Cleared || c.Kind == Reset {
			out = append(out, c)
		}
	}
	return out
}

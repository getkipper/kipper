package issuelink

import (
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var (
	urlUserinfo     = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^/\s@]+@`)
	email           = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	userAtKnownHost = regexp.MustCompile(`[A-Za-z0-9._+-]+@<redacted>`)
	credentialFlag  = regexp.MustCompile(`(?i)(--?[a-z0-9-]*(?:password|passwd|secret|token|api-?key))(\s+|=)(` + assignedValuePattern + `)`)
	credentialValue = regexp.MustCompile(`(?i)("?)\b([a-z0-9_]*(?:password|passwd|secret|token|api_?key))("?)(\s*[:=]\s*)(` + assignedValuePattern + `)`)
	bearerToken     = regexp.MustCompile(`(?i)\bBearer\s+\S+`)
	jwt             = regexp.MustCompile(`\beyJ[\w-]+\.[\w-]+\.[\w-]+`)
	kipperRunName   = regexp.MustCompile(`(?i)\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.kipper\.run\b`)
	unixHome        = regexp.MustCompile(`(/home|/Users)/[^/\s:'"<>]+`)
	windowsHome     = regexp.MustCompile(`(?i)\b([a-z]:\\Users\\)[^\\\s:'"<>]+`)
	urlHost         = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)(<[a-z]+>|[a-z0-9._~-]+)`)
	hostname        = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}\b`)
	ipv6            = regexp.MustCompile(`(?i)(?:[0-9a-f]{0,4}:){2,7}(?:\d{1,3}(?:\.\d{1,3}){3}|[0-9a-f]{0,4})(?:%[0-9a-z]+)?`)
	ipv4            = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	longHex         = regexp.MustCompile(`\b[0-9A-Fa-f]{32,}\b`)
	longBase64      = regexp.MustCompile(`[A-Za-z0-9+/_-]{32,}={0,2}`)
	percentEscape   = regexp.MustCompile(`%[0-9A-F]{2}`)

	// Preserve these public dependency domains and their subdomains to identify
	// failed downloads. Exclude providers with customer-specific hostnames,
	// such as S3 bucket hosts.
	publicDependencyDomains = []string{
		"almalinux.org", "canonical.com", "centos.org", "cert-manager.io", "debian.org", "dexidp.io",
		"docker.com", "docker.io", "fedoraproject.org", "gcr.io", "getkipper.com", "ghcr.io",
		"github.com", "githubusercontent.com", "golang.org", "grafana.com", "helm.sh", "jetstack.io",
		"k3s.io", "k8s.io", "keda.sh", "kipper.run", "kubernetes.io", "letsencrypt.org", "longhorn.io",
		"prometheus.io", "quay.io", "rancher.io", "redhat.com", "rockylinux.org", "traefik.io",
		"ubuntu.com", "velero.io",
	}
	// Restrict hostname detection outside URLs to common top-level domains
	// because other dotted names are usually file names or field paths.
	commonTLDs = setOf("ai", "app", "at", "au", "be", "biz", "br", "ca", "ch", "cloud", "cn", "co",
		"com", "cz", "de", "dev", "dk", "es", "eu", "fi", "fr", "host", "in", "info", "io", "it", "jp",
		"me", "net", "nl", "no", "nz", "online", "org", "pl", "pt", "ru", "se", "site", "tech", "uk",
		"us", "xyz")

	k3sClusterRanges = []netip.Prefix{netip.MustParsePrefix("10.42.0.0/16"), netip.MustParsePrefix("10.43.0.0/16")}
)

// assignedValuePattern matches an escaped-quoted, quoted or bare value.
const assignedValuePattern = `\\"(?:[^"\\]|\\[^"])*\\"|"(?:[^"\\]|\\.)*"|'[^']*'|[^\s"',;}]+`

// Scrub replaces personal and secret data in text with placeholders. It is
// best effort and meant for text a person reads before sharing it.
//
// Known values match case-sensitively in plain and URL-encoded forms, with
// longer matches taking priority and empty values ignored. Secrets become
// <redacted> wherever they appear before any pattern rules run. Names (hosts,
// domains, users or organizations) become <redacted> at ASCII letter and digit
// boundaries, so a short user name leaves longer words intact.
//
// Pattern rules replace email addresses, URL credentials, credential values
// and flag arguments, bearer tokens, JWTs, kipper.run names and home directory
// users. They also replace hosts in URLs and dotted names ending in a common
// top-level domain, preserving listed public dependencies. IP detection
// preserves loopback, link-local unicast, unspecified addresses and the k3s
// pod and service ranges. Secret detection replaces hex strings of at least
// 32 characters and base64-like strings of at least 32 characters that mix
// digits with uppercase and lowercase letters.
func Scrub(text string, secrets, names []string) string {
	text = replaceKnown(text, knownForms(secrets), false)
	text = urlUserinfo.ReplaceAllString(text, "$1<secret>@")
	text = email.ReplaceAllString(text, "<email>")
	text = replaceKnown(text, knownForms(names), true)
	text = userAtKnownHost.ReplaceAllString(text, "<user>@<redacted>")
	text = credentialFlag.ReplaceAllStringFunc(text, func(m string) string {
		parts := credentialFlag.FindStringSubmatch(m)
		return parts[1] + parts[2] + secretLike(parts[3])
	})
	text = credentialValue.ReplaceAllStringFunc(text, func(m string) string {
		parts := credentialValue.FindStringSubmatch(m)
		return parts[1] + parts[2] + parts[3] + parts[4] + secretLike(parts[5])
	})
	text = bearerToken.ReplaceAllString(text, "Bearer <secret>")
	text = jwt.ReplaceAllString(text, "<secret>")
	text = kipperRunName.ReplaceAllString(text, "<cluster>.kipper.run")
	text = unixHome.ReplaceAllString(text, "$1/<user>")
	text = windowsHome.ReplaceAllString(text, "$1<user>")
	text = urlHost.ReplaceAllStringFunc(text, redactURLHost)
	text = hostname.ReplaceAllStringFunc(text, redactHostname)
	text = ipv6.ReplaceAllStringFunc(text, redactAddress)
	text = ipv4.ReplaceAllStringFunc(text, redactAddress)
	text = longHex.ReplaceAllString(text, "<secret>")
	return longBase64.ReplaceAllStringFunc(text, func(s string) string {
		if looksRandom(s) {
			return "<secret>"
		}
		return s
	})
}

func knownForms(known []string) []string {
	seen := map[string]bool{}
	var forms []string
	for _, v := range known {
		query, path := url.QueryEscape(v), url.PathEscape(v)
		for _, form := range []string{v, query, path, lowerEscapes(query), lowerEscapes(path)} {
			if form != "" && !seen[form] {
				seen[form] = true
				forms = append(forms, form)
			}
		}
	}
	// A shorter value can be part of a longer one, such as a domain inside its console hostname.
	sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	return forms
}

func lowerEscapes(s string) string {
	return percentEscape.ReplaceAllStringFunc(s, strings.ToLower)
}

// replaceKnown runs in a single pass so a placeholder is never matched again.
// With wholeTokens a value only matches where it is not part of a longer word.
func replaceKnown(text string, values []string, wholeTokens bool) string {
	if len(values) == 0 {
		return text
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		if v := knownAt(text, i, values, wholeTokens); v != "" {
			b.WriteString("<redacted>")
			i += len(v)
			continue
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

func knownAt(text string, i int, values []string, wholeTokens bool) string {
	for _, v := range values {
		if !strings.HasPrefix(text[i:], v) {
			continue
		}
		if !wholeTokens {
			return v
		}
		end := i + len(v)
		if isWordByte(v[0]) && i > 0 && isWordByte(text[i-1]) {
			continue
		}
		if isWordByte(v[len(v)-1]) && end < len(text) && isWordByte(text[end]) {
			continue
		}
		return v
	}
	return ""
}

func isWordByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// secretLike replaces a credential value and keeps the quoting around it.
func secretLike(value string) string {
	switch {
	case strings.HasPrefix(value, `\"`):
		return `\"<secret>\"`
	case strings.HasPrefix(value, `"`):
		return `"<secret>"`
	case strings.HasPrefix(value, "'"):
		return "'<secret>'"
	}
	return "<secret>"
}

func redactURLHost(match string) string {
	parts := urlHost.FindStringSubmatch(match)
	host := parts[2]
	if strings.HasPrefix(host, "<") || isPublicDependency(host) {
		return match
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return match
	}
	return parts[1] + "<host>"
}

func isPublicDependency(host string) bool {
	lower := strings.ToLower(host)
	for _, domain := range publicDependencyDomains {
		if lower == domain || strings.HasSuffix(lower, "."+domain) {
			return true
		}
	}
	return false
}

func redactHostname(name string) string {
	labels := strings.Split(strings.ToLower(name), ".")
	if isPublicDependency(name) || !commonTLDs[labels[len(labels)-1]] {
		return name
	}
	return "<host>"
}

// redactAddress preserves trailing colons from the surrounding text when
// redacting an address, such as the one in "2001:db8::1: connection refused".
func redactAddress(candidate string) string {
	addrText := candidate
	for {
		if addr, err := netip.ParseAddr(addrText); err == nil {
			if keepAddress(addr.Unmap()) {
				return candidate
			}
			return "<ip>" + candidate[len(addrText):]
		}
		if !strings.HasSuffix(addrText, ":") {
			return candidate
		}
		addrText = strings.TrimSuffix(addrText, ":")
	}
}

func keepAddress(addr netip.Addr) bool {
	if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() {
		return true
	}
	for _, p := range k3sClusterRanges {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// looksRandom uses mixed digits and letter cases as a heuristic for generated
// secrets because ordinary paths and identifiers rarely combine all three.
func looksRandom(s string) bool {
	var digit, upper, lower bool
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= 'a' && r <= 'z':
			lower = true
		}
	}
	return digit && upper && lower
}

func setOf(items ...string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}

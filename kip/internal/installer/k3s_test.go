package installer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDefaultDNSResolvers(t *testing.T) {
	got := DefaultDNSResolvers()

	// IPv4-only on purpose: an IPv6 upstream on an IPv4-only cluster is
	// unreachable and was the original cause of the cluster-wide DNS
	// flapping.
	assert.Equal(t, []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}, got)
	for _, r := range got {
		assert.NotContains(t, r, ":", "default resolvers must be IPv4")
	}

	// Independent calls return distinct slices so callers can mutate.
	a := DefaultDNSResolvers()
	b := DefaultDNSResolvers()
	a[0] = "mutated"
	assert.NotEqual(t, a[0], b[0])
}

func TestDNSProbeCommand(t *testing.T) {
	cmd := dnsProbeCommand([]string{"1.1.1.1", "10.0.0.53"})

	// One probe per resolver, joined so a single SSH round trip covers
	// the whole list.
	assert.Equal(t,
		"if timeout 3 bash -c ':< /dev/tcp/1.1.1.1/53' 2>/dev/null; then echo '1.1.1.1 ok'; else echo '1.1.1.1 unreachable'; fi; "+
			"if timeout 3 bash -c ':< /dev/tcp/10.0.0.53/53' 2>/dev/null; then echo '10.0.0.53 ok'; else echo '10.0.0.53 unreachable'; fi",
		cmd)
}

func TestParseDNSProbeOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   []string
	}{
		{"all reachable", "1.1.1.1 ok\n8.8.8.8 ok\n", nil},
		{"one unreachable", "1.1.1.1 ok\n10.0.0.53 unreachable\n", []string{"10.0.0.53"}},
		{"all unreachable", "1.1.1.1 unreachable\n8.8.8.8 unreachable\n", []string{"1.1.1.1", "8.8.8.8"}},
		{"empty output", "", nil},
		{"noise ignored", "bash: warning\n1.1.1.1 unreachable\nsomething else entirely\n", []string{"1.1.1.1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseDNSProbeOutput(tt.output))
		})
	}
}

func TestRenderResolvConf(t *testing.T) {
	out := renderResolvConf([]string{"1.1.1.1", "8.8.8.8"})
	assert.Equal(t, "nameserver 1.1.1.1\nnameserver 8.8.8.8\n", out)

	// A custom internal resolver renders the same way.
	assert.Equal(t, "nameserver 10.0.0.53\n", renderResolvConf([]string{"10.0.0.53"}))
}

func TestResolveDNSResolvers(t *testing.T) {
	// Nil and all-empty inputs fall back to the defaults rather than
	// writing an empty (broken) resolv.conf.
	got, err := resolveDNSResolvers(nil)
	assert.NoError(t, err)
	assert.Equal(t, DefaultDNSResolvers(), got)

	got, err = resolveDNSResolvers([]string{"", "  "})
	assert.NoError(t, err)
	assert.Equal(t, DefaultDNSResolvers(), got)

	// Valid IPv4 is trimmed and kept; blanks in the middle are dropped.
	got, err = resolveDNSResolvers([]string{" 10.0.0.53 ", "", "9.9.9.9"})
	assert.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.53", "9.9.9.9"}, got)

	// Duplicates collapse to one, preserving first-seen order.
	got, err = resolveDNSResolvers([]string{"1.1.1.1", "8.8.8.8", "1.1.1.1"})
	assert.NoError(t, err)
	assert.Equal(t, []string{"1.1.1.1", "8.8.8.8"}, got)

	// IPv6 is rejected: the pod network is IPv4-only, so an IPv6 upstream
	// is unreachable — the exact fault that broke cluster DNS originally.
	_, err = resolveDNSResolvers([]string{"2606:4700:4700::1111"})
	assert.Error(t, err)

	// More than three nameservers is rejected rather than silently
	// truncated by the kubelet.
	_, err = resolveDNSResolvers([]string{"1.1.1.1", "8.8.8.8", "9.9.9.9", "8.8.4.4"})
	assert.Error(t, err)

	// Non-IP input is rejected — this is what keeps a stray value from
	// producing a broken resolv.conf or breaking out of the install
	// heredoc.
	for _, bad := range []string{"dns.example.com", "1.1.1.1 # comment", "1.1.1.1\nKIPEOF\nrm -rf /"} {
		_, err := resolveDNSResolvers([]string{bad})
		assert.Error(t, err, "expected %q to be rejected", bad)
	}
}

func TestParseResolvConf(t *testing.T) {
	body := "# managed by kipper\nnameserver 1.1.1.1\n\nnameserver 9.9.9.9\nsearch example.com\noptions ndots:5\n"
	assert.Equal(t, []string{"1.1.1.1", "9.9.9.9"}, parseResolvConf(body))

	// An empty or comment-only body yields nothing, so the worker join
	// falls back to the defaults rather than mirroring a blank file.
	assert.Empty(t, parseResolvConf("# nothing here\n"))
	assert.Empty(t, parseResolvConf(""))

	// A malformed nameserver line is surfaced as an IP that
	// resolveDNSResolvers then rejects, forcing the safe fallback.
	_, err := resolveDNSResolvers(parseResolvConf("nameserver not-an-ip\n"))
	assert.Error(t, err)

	// Inline-comment lines have more than two fields and are ignored — a
	// conservative choice, since Kipper only ever writes plain two-field
	// `nameserver <ip>` lines.
	assert.Empty(t, parseResolvConf("nameserver 10.0.0.53 # corp\n"))
}

func TestK3sConfigPointsCoreDNSAtCuratedResolvConf(t *testing.T) {
	// The rendered k3s config must set resolv-conf to the file
	// InstallK3s writes, or CoreDNS keeps forwarding to the host's
	// resolvers and the fix does nothing.
	rendered := fmt.Sprintf(k3sConfig, "myserver.example.com")
	assert.Contains(t, rendered, "resolv-conf: "+k3sResolvConfPath)
	assert.True(t, strings.Contains(rendered, "disable:\n  - traefik"),
		"traefik must still be disabled")
}

func TestK3sConfigEnablesProtectKernelDefaults(t *testing.T) {
	rendered := fmt.Sprintf(k3sConfig, "myserver.example.com")
	assert.Contains(t, rendered, "kubelet-arg:\n  - \"protect-kernel-defaults=true\"",
		"the kubelet must run with protect-kernel-defaults")

	// The flag makes the kubelet refuse to start unless the host already sets
	// exactly these sysctls, so writeKubeletSysctls must supply every one the
	// kubelet checks. Missing any would brick the node on start.
	for _, s := range []string{
		"vm.panic_on_oom=0",
		"vm.overcommit_memory=1",
		"kernel.panic=10",
		"kernel.panic_on_oops=1",
		"kernel.keys.root_maxbytes=25000000",
		"kernel.keys.root_maxkeys=1000000",
	} {
		assert.Contains(t, kubeletProtectSysctls, s,
			"protect-kernel-defaults requires the %s sysctl to be set before the kubelet starts", s)
	}
}

func TestReplaceServerAddress(t *testing.T) {
	tests := []struct {
		name       string
		kubeconfig string
		host       string
		want       string
	}{
		{
			name: "replaces localhost with public IP",
			kubeconfig: `apiVersion: v1
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: default
`,
			host: "198.51.100.1",
			want: `apiVersion: v1
clusters:
- cluster:
    server: https://198.51.100.1:6443
  name: default
`,
		},
		{
			name: "replaces hostname with domain",
			kubeconfig: `    server: https://127.0.0.1:6443
`,
			host: "myserver.example.com",
			want: `    server: https://myserver.example.com:6443
`,
		},
		{
			name:       "no-op when address not present",
			kubeconfig: "server: https://10.0.0.1:6443",
			host:       "198.51.100.1",
			want:       "server: https://10.0.0.1:6443",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceServerAddress(tt.kubeconfig, tt.host)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestK3sPreexistingFromSample(t *testing.T) {
	tests := []struct {
		name   string
		output string
		err    error
		want   bool
	}{
		{name: "explicit fresh skips the restart", output: "fresh\n", want: false},
		{name: "existing data dir restarts", output: "existing\n", want: true},
		{name: "transport error counts as existing", output: "", err: fmt.Errorf("connection reset"), want: true},
		{name: "empty output is ambiguous, counts as existing", output: "", want: true},
		{name: "shell noise without an answer counts as existing", output: "W0713 warning: something happened\n", want: true},
		{name: "fresh answer alongside noise still skips", output: "W0713 warning\nfresh\n", want: false},
		{name: "fresh as a substring of noise is not an answer", output: "refreshing configuration\n", want: true},
		{name: "conflicting markers stay fail-closed", output: "fresh\nexisting\n", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, k3sPreexistingFromSample(tt.output, tt.err))
		})
	}
}

func TestCheckResolvConf(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		want      []string
		wantError string
	}{
		{
			name: "curated defaults are healthy",
			body: "nameserver 1.1.1.1\nnameserver 8.8.8.8\nnameserver 9.9.9.9\n",
			want: []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"},
		},
		{
			name: "a valid custom IPv4 set passes",
			body: "nameserver 10.0.0.53\n",
			want: []string{"10.0.0.53"},
		},
		{
			name:      "empty file has no external DNS",
			body:      "\n# nothing here\n",
			wantError: "no nameserver entries",
		},
		{
			name:      "an IPv6 entry is unsafe on an IPv4 cluster",
			body:      "nameserver 2606:4700:4700::1111\n",
			wantError: "only IPv4 resolvers are supported",
		},
		{
			name:      "a hostname is not an IP",
			body:      "nameserver dns.example.com\n",
			wantError: "must be an IP address",
		},
		{
			name:      "more than three nameservers is over the limit",
			body:      "nameserver 1.1.1.1\nnameserver 8.8.8.8\nnameserver 9.9.9.9\nnameserver 8.8.4.4\n",
			wantError: "too many DNS resolvers",
		},
		{
			name:      "an IPv6 entry with a trailing comment is not hidden",
			body:      "nameserver 1.1.1.1\nnameserver 2606:4700:4700::1111 # added manually\n",
			wantError: "only IPv4 resolvers are supported",
		},
		{
			name:      "four raw entries with a duplicate still exceed the limit",
			body:      "nameserver 1.1.1.1\nnameserver 1.1.1.1\nnameserver 1.1.1.1\nnameserver 8.8.8.8\n",
			wantError: "too many DNS resolvers",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CheckResolvConf(tt.body)
			if tt.wantError != "" {
				assert.ErrorContains(t, err, tt.wantError)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolversMatch(t *testing.T) {
	tests := []struct {
		name       string
		live       []string
		configured []string
		want       bool
	}{
		{"same set in same order", []string{"1.1.1.1", "8.8.8.8"}, []string{"1.1.1.1", "8.8.8.8"}, true},
		{"reorder counts as drift", []string{"8.8.8.8", "1.1.1.1"}, []string{"1.1.1.1", "8.8.8.8"}, false},
		{"changed resolver", []string{"9.9.9.9"}, []string{"1.1.1.1"}, false},
		{"live has an extra entry", []string{"1.1.1.1", "8.8.8.8"}, []string{"1.1.1.1"}, false},
		{"live missing an entry", []string{"1.1.1.1"}, []string{"1.1.1.1", "8.8.8.8"}, false},
		{"unparseable live entry differs", []string{"not-an-ip"}, []string{"1.1.1.1"}, false},
		{"both empty", nil, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ResolversMatch(tt.live, tt.configured))
		})
	}
}

func TestK3sVersionIsPinnedToAValidTag(t *testing.T) {
	// A typo in the pin would surface as a failed download on a real server;
	// this catches it at test time instead.
	assert.Regexp(t, k3sVersionRe, k3sVersion)
}

func TestParseK3sVersion(t *testing.T) {
	tests := []struct {
		name    string
		tag     string
		want    [4]int
		wantErr bool
	}{
		{"release tag", "v1.36.2+k3s1", [4]int{1, 36, 2, 1}, false},
		{"multi-digit components", "v1.35.14+k3s12", [4]int{1, 35, 14, 12}, false},
		{"missing k3s revision", "v1.36.2", [4]int{}, true},
		{"missing v prefix", "1.36.2+k3s1", [4]int{}, true},
		{"release candidate", "v1.36.2-rc1+k3s1", [4]int{}, true},
		{"trailing junk", "v1.36.2+k3s1-extra", [4]int{}, true},
		{"empty", "", [4]int{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseK3sVersion(tt.tag)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestK3sVersionNewer(t *testing.T) {
	tests := []struct {
		name    string
		a, b    string
		want    bool
		wantErr bool
	}{
		{"equal", "v1.36.2+k3s1", "v1.36.2+k3s1", false, false},
		{"newer minor", "v1.37.0+k3s1", "v1.36.2+k3s1", true, false},
		{"older minor", "v1.35.6+k3s1", "v1.36.2+k3s1", false, false},
		{"newer patch", "v1.36.3+k3s1", "v1.36.2+k3s1", true, false},
		{"newer k3s revision only", "v1.36.2+k3s2", "v1.36.2+k3s1", true, false},
		{"older k3s revision only", "v1.36.2+k3s1", "v1.36.2+k3s2", false, false},
		{"newer major", "v2.0.0+k3s1", "v1.36.2+k3s1", true, false},
		{"numeric not lexicographic", "v1.36.10+k3s1", "v1.36.9+k3s1", true, false},
		{"malformed a", "junk", "v1.36.2+k3s1", false, true},
		{"malformed b", "v1.36.2+k3s1", "junk", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := k3sVersionNewer(tt.a, tt.b)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseInstalledK3sVersion(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    string
		wantErr bool
	}{
		{
			name:   "real k3s --version output",
			output: "k3s version v1.35.4+k3s1 (97623a41)\ngo version go1.24.2\n",
			want:   "v1.35.4+k3s1",
		},
		{
			name:   "not installed (absent marker)",
			output: "KIP_K3S_ABSENT\n",
			want:   "",
		},
		{
			name:   "tag preceded by shell noise",
			output: "some warning line\nk3s version v1.36.2+k3s1 (abc123)\n",
			want:   "v1.36.2+k3s1",
		},
		{
			// The version drives the downgrade guard and agent skew pinning,
			// so unreadable output must fail rather than pass as "absent".
			name:    "present but unparseable",
			output:  "k3s version unknown\n",
			wantErr: true,
		},
		{
			// A binary that exists but cannot run must not authorise a fresh
			// install by masquerading as absent.
			name:    "present but version command failed",
			output:  "KIP_K3S_VERSION_FAILED\n",
			wantErr: true,
		},
		{
			// k3s can print a version and still exit nonzero; the failure
			// marker wins so the guard stays fail-closed.
			name:    "version printed but command failed",
			output:  "k3s version v1.36.2+k3s1 (abc123)\nKIP_K3S_VERSION_FAILED\n",
			wantErr: true,
		},
		{
			// The probe always emits a marker or a version, so silence is an
			// anomaly, never proof of absence.
			name:    "empty output",
			output:  "",
			wantErr: true,
		},
		{
			name:    "whitespace only",
			output:  "  \n",
			wantErr: true,
		},
		{
			// The whole output is scanned before deciding: an absence marker
			// followed by contradictory evidence must not authorise a fresh
			// install.
			name:    "absence marker followed by failure marker",
			output:  "KIP_K3S_ABSENT\nKIP_K3S_VERSION_FAILED\n",
			wantErr: true,
		},
		{
			name:    "absence marker followed by a version",
			output:  "KIP_K3S_ABSENT\nk3s version v1.37.0+k3s1 (abc123)\n",
			wantErr: true,
		},
		{
			name:    "failure marker before a version",
			output:  "KIP_K3S_VERSION_FAILED\nk3s version v1.36.2+k3s1 (abc123)\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseInstalledK3sVersion(tt.output)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDecideK3sInstall(t *testing.T) {
	const pin = "v1.36.2+k3s1"
	tests := []struct {
		name       string
		installed  string
		run        bool
		skipReason string
		wantErr    bool
	}{
		{name: "fresh host installs", installed: "", run: true},
		{name: "same release re-installs", installed: "v1.36.2+k3s1", run: true},
		{name: "older patch upgrades", installed: "v1.36.1+k3s1", run: true},
		{name: "older k3s revision upgrades", installed: "v1.36.2+k3s0", run: true},
		{name: "one minor behind upgrades", installed: "v1.35.6+k3s1", run: true},
		{name: "one minor behind with higher patch upgrades", installed: "v1.35.14+k3s3", run: true},
		{name: "newer patch refused", installed: "v1.36.3+k3s1", skipReason: "newer"},
		{name: "newer minor refused", installed: "v1.37.0+k3s1", skipReason: "newer"},
		{name: "two minors behind refused", installed: "v1.34.9+k3s1", skipReason: "one minor at a time"},
		{name: "major behind refused", installed: "v0.36.2+k3s1", skipReason: "one minor at a time"},
		{name: "malformed installed version", installed: "garbage", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run, reason, err := decideK3sInstall(tt.installed, pin)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.run, run)
			if tt.run {
				assert.Empty(t, reason)
			} else {
				assert.Contains(t, reason, tt.skipReason)
				assert.Contains(t, reason, tt.installed)
				assert.Contains(t, reason, pin)
			}
		})
	}
}

func TestK3sVersionProbeCmdEmitsMarkers(t *testing.T) {
	// The parser's fail-closed behaviour depends on the probe always
	// producing a marker when k3s is absent or broken; pin the command so a
	// rewrite cannot silently drop one.
	assert.Contains(t, k3sVersionProbeCmd, "KIP_K3S_ABSENT")
	assert.Contains(t, k3sVersionProbeCmd, "KIP_K3S_VERSION_FAILED")
	assert.Contains(t, k3sVersionProbeCmd, "k3s --version")
}

func TestDecideK3sAgentJoin(t *testing.T) {
	const master = "v1.36.2+k3s1"
	tests := []struct {
		name      string
		installed string
		wantErr   string
	}{
		{name: "fresh worker joins", installed: ""},
		{name: "same release rejoins", installed: "v1.36.2+k3s1"},
		{name: "older patch converges", installed: "v1.36.1+k3s1"},
		{name: "older minor converges", installed: "v1.35.6+k3s1"},
		// Unlike the server, any forward distance is fine: the target is the
		// control plane's own version, so running skew ends at zero.
		{name: "several minors older converges", installed: "v1.33.4+k3s1"},
		{name: "newer worker refused", installed: "v1.37.0+k3s1", wantErr: "downgrading an agent is unsupported"},
		{name: "newer k3s revision refused", installed: "v1.36.2+k3s2", wantErr: "downgrading an agent is unsupported"},
		{name: "malformed worker version", installed: "garbage", wantErr: "invalid k3s version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := decideK3sAgentJoin(tt.installed, master)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestAgentKubeletConfigHasProtectKernelDefaults(t *testing.T) {
	// The worker's kubelet must get the same protect-kernel-defaults posture as
	// the server, or a mixed cluster hardens only the control plane.
	body := "kubelet-arg:\n  - \"protect-kernel-defaults=true\"\n"
	assert.Contains(t, body, "protect-kernel-defaults=true")
	// Guard the shared invariant: server config and the agent drop-in use the
	// same flag string.
	assert.Contains(t, fmt.Sprintf(k3sConfig, "h"), "protect-kernel-defaults=true")
}

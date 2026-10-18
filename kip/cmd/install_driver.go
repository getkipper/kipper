package cmd

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/installjob"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

type jobRemote interface {
	Run(command string) (string, error)
	RunStdin(command string, stdin io.Reader) (string, error)
	RunStreamTo(command string, stdout, stderr io.Writer) error
}

// followJob streams the log and returns the final phase. It retries dropped
// connections from the last received log byte, excluding SSH diagnostics.
func followJob(r jobRemote, kip, runID string, out io.Writer, retries int, backoff time.Duration) (string, error) {
	var offset int64
	for attempt := 0; ; attempt++ {
		w := &endWriter{out: out, base: offset}
		var sshErr bytes.Buffer
		err := r.RunStreamTo(fmt.Sprintf("%s install-job follow --run %s --offset %d", kip, runID, offset), w, &sshErr)
		w.finish()
		offset += w.written
		if w.done {
			return w.phase, nil
		}
		if attempt >= retries {
			if err == nil {
				return "", fmt.Errorf("the log stream ended without a final status; the install may still be running on the server. Run the same command to reconnect")
			}
			return "", fmt.Errorf("lost the connection to the install (%v); it may still be running on the server; run the same command again to reattach", err)
		}
		time.Sleep(backoff * time.Duration(attempt+1))
	}
}

var endLine = regexp.MustCompile(`^\n` + regexp.QuoteMeta(installjob.EndMarker) + ` offset=(\d+) phase=([a-z]+)\n$`)

var knownPhases = map[string]bool{
	installjob.PhaseSucceeded: true, installjob.PhaseFailed: true, installjob.PhaseInterrupted: true,
}

// endWriter buffers possible end markers across writes. A complete marker must
// name a known phase and match the received log offset to finish the job.
// An offset mismatch stops the stream so followJob can reconnect; malformed
// markers pass through as log text.
type endWriter struct {
	out     io.Writer
	base    int64
	pending []byte
	written int64
	done    bool
	stopped bool
	phase   string
}

var endMarkerBytes = []byte("\n" + installjob.EndMarker)

func (w *endWriter) Write(p []byte) (int, error) {
	if w.done || w.stopped {
		return len(p), nil
	}
	w.pending = append(w.pending, p...)
	for {
		i := bytes.Index(w.pending, endMarkerBytes)
		if i < 0 {
			break
		}
		if err := w.flush(w.pending[:i]); err != nil {
			return 0, err
		}
		w.pending = append([]byte(nil), w.pending[i:]...)
		nl := bytes.IndexByte(w.pending[1:], '\n')
		if nl < 0 {
			// Wait for the complete marker before interpreting it.
			return len(p), nil
		}
		line := w.pending[:nl+2]
		if phase, isEnd, ok := w.endOf(line); isEnd {
			// Retry from the received offset if the marker reports a mismatch.
			w.phase, w.done, w.stopped = phase, ok, !ok
			w.pending = nil
			return len(p), nil
		}
		if err := w.flush(line); err != nil {
			return 0, err
		}
		w.pending = append([]byte(nil), w.pending[nl+2:]...)
	}
	if keep := len(endMarkerBytes) - 1; len(w.pending) > keep {
		if err := w.flush(w.pending[:len(w.pending)-keep]); err != nil {
			return 0, err
		}
		w.pending = append([]byte(nil), w.pending[len(w.pending)-keep:]...)
	}
	return len(p), nil
}

// endOf reports whether line is an end line and, if so, whether it matches
// the log received so far, returning the phase it names.
func (w *endWriter) endOf(line []byte) (phase string, isEnd, ok bool) {
	m := endLine.FindSubmatch(line)
	if len(m) != 3 || !knownPhases[string(m[2])] {
		return "", false, false
	}
	offset, err := strconv.ParseInt(string(m[1]), 10, 64)
	if err != nil || offset != w.base+w.written {
		return "", true, false
	}
	return string(m[2]), true, true
}

// finish flushes buffered log bytes and discards any partial end marker so
// reconnecting resumes at the log offset.
func (w *endWriter) finish() {
	if w.done || w.stopped {
		return
	}
	held := w.pending
	w.pending = nil
	if bytes.HasPrefix(held, endMarkerBytes) {
		return
	}
	for n := len(held); n > 0; n-- {
		if bytes.HasPrefix(endMarkerBytes, held[len(held)-n:]) && held[len(held)-n] == '\n' {
			_ = w.flush(held[:len(held)-n])
			return
		}
	}
	_ = w.flush(held)
}

func (w *endWriter) flush(b []byte) error {
	n, err := w.out.Write(b)
	w.written += int64(n)
	return err
}

// lastJSONLine finds the last line beginning with {, skipping earlier SSH
// notices. The caller validates the JSON.
func lastJSONLine(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); strings.HasPrefix(line, "{") {
			return line
		}
	}
	return ""
}

func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

var releaseVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// releaseTag accepts plain major.minor.patch versions for release downloads.
func releaseTag(version string) (string, bool) {
	if !releaseVersion.MatchString(version) {
		return "", false
	}
	return "v" + version, true
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// checksumFor finds the sha256 of the Linux kip for arch in a release's
// checksums.txt.
func checksumFor(sums, arch string) (string, error) {
	want := "kip-linux-" + arch
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == want {
			if !sha256Hex.MatchString(fields[0]) {
				return "", fmt.Errorf("the release's checksum for %s is not a sha256", want)
			}
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("the release has no %s", want)
}

// serverKipFor names a server kip by its checksum, so concurrent installs can
// use different versions.
func serverKipFor(sum string) string { return installjob.KipDir + "/kip-" + sum }

// fetchKipCommand reuses a verified binary, refreshing its time so cleanup keeps
// it, or downloads and verifies a temporary file before publishing it at target.
// Both happen under the lock cleanup takes.
func fetchKipCommand(url, sum, target string) string {
	return fmt.Sprintf(`set -e; dir=$(dirname %[3]s); mkdir -p "$dir"; exec 9>>"$dir/%[4]s"; flock 9; `+
		`if echo "%[2]s  "%[3]s | sha256sum -c --quiet >/dev/null 2>&1; then touch %[3]s; exit 0; fi; `+
		`tmp=$(mktemp %[3]s.XXXXXX); trap 'rm -f "$tmp"' EXIT; `+
		`curl -fsSL --max-time 300 %[1]s -o "$tmp"; echo "%[2]s  $tmp" | sha256sum -c --quiet; chmod 755 "$tmp"; mv -f "$tmp" %[3]s`,
		ssh.Quote(url), sum, ssh.Quote(target), installjob.KipLockName)
}

// publishUploadCommand checks an uploaded kip against sum and renames it to
// target, removing it when the check or the rename fails.
func publishUploadCommand(staged, sum, target string) string {
	return fmt.Sprintf(`set -e; trap 'rm -f %[1]s' EXIT; exec 9>>"$(dirname %[3]s)/%[4]s"; flock 9; `+
		`echo "%[2]s  "%[1]s | sha256sum -c --quiet; chmod 755 %[1]s; mv -f %[1]s %[3]s`,
		ssh.Quote(staged), sum, ssh.Quote(target), installjob.KipLockName)
}

// linkKipCommand points link at target with a single rename, so kip on the
// server's PATH is always a whole binary.
func linkKipCommand(target, link string) string {
	return fmt.Sprintf(`set -e; tmp=%[2]s.link-$$; ln -sf %[1]s "$tmp"; mv -f "$tmp" %[2]s`, ssh.Quote(target), ssh.Quote(link))
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// serverArch restricts downloads to supported release architectures.
func serverArch(uname string) (string, error) {
	switch strings.TrimSpace(uname) {
	case "x86_64":
		return "amd64", nil
	case "aarch64":
		return "arm64", nil
	}
	return "", fmt.Errorf("the server reports architecture %q; Kipper releases kip for x86_64 and aarch64", strings.TrimSpace(uname))
}

// saveDrivenResult imports the server result with a locally rendered kubeconfig
// and the SSH key and login user. Host, cluster-name, and credential checks
// protect existing entries.
func saveDrivenResult(res installjob.Result, host, sshKey, sshUser string) error {
	if res.Cluster == nil {
		return fmt.Errorf("the install produced no cluster entry to import")
	}
	entry := *res.Cluster
	if err := validateClusterName(entry.Name); err != nil {
		return err
	}
	if !strings.EqualFold(entry.Host, host) {
		return fmt.Errorf("refusing the install result: it describes host %s, but this install was for %s", entry.Host, host)
	}
	kubeconfig, err := installer.RenderImportedKubeconfig(entry.Domain, res.Kubeconfig)
	if err != nil {
		return fmt.Errorf("preparing the cluster's kubeconfig for this machine: %w", err)
	}
	dir, err := config.Dir()
	if err != nil {
		return err
	}
	clusters := filepath.Join(dir, "clusters")
	if err := os.MkdirAll(clusters, 0o700); err != nil {
		return err
	}
	entry.Kubeconfig = filepath.Join(clusters, entry.Name+".yaml")
	entry.SSHKey = sshKey
	entry.SSHUser = sshUser
	entry.CurrentProject = ""
	entry.CurrentEnvironment = ""
	return config.Update(func(cfg *config.Config) error {
		if existing := cfg.GetCluster(entry.Name); existing != nil {
			if !strings.EqualFold(existing.Host, host) {
				return fmt.Errorf("refusing the install result: this machine already has a cluster %q on host %s. Rename it first with 'kip cluster rename', then run kip install again", entry.Name, existing.Host)
			}
			entry.CurrentProject = existing.CurrentProject
			entry.CurrentEnvironment = existing.CurrentEnvironment
		}
		if err := refuseAStolenKubeconfigPath(cfg, entry.Name, entry.Kubeconfig); err != nil {
			return err
		}
		if err := refuseToReplaceACredential(entry.Kubeconfig); err != nil {
			return err
		}
		if err := installer.WriteFileAtomic(entry.Kubeconfig, []byte(kubeconfig), 0o600); err != nil {
			return fmt.Errorf("writing kubeconfig: %w", err)
		}
		cfg.AddCluster(entry)
		cfg.CurrentCluster = entry.Name
		return nil
	})
}

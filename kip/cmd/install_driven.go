package cmd

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/installjob"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

const serverKipPath = "/usr/local/bin/kip"

// usesDriver defaults to server jobs on Windows, where OpenSSH lacks connection
// sharing. Explicit --via-server takes precedence over --local.
func usesDriver(cmd *cobra.Command, goos string) bool {
	if cmd.Flags().Changed("via-server") {
		v, _ := cmd.Flags().GetBool("via-server")
		return v
	}
	if local, _ := cmd.Flags().GetBool("local"); local {
		return false
	}
	return goos == "windows"
}

// runDrivenInstall starts or attaches to a server job, streams its log, and
// imports its result locally. The job survives client disconnections.
func runDrivenInstall(cmd *cobra.Command, credentialsFile string) error {
	host, _ := cmd.Flags().GetString("host")
	sshKeyFlag, _ := cmd.Flags().GetString("ssh-key")
	noLogin, _ := cmd.Flags().GetBool("no-login")
	cancel, _ := cmd.Flags().GetBool("cancel")
	serverKip, _ := cmd.Flags().GetString("server-kip")
	if local, _ := cmd.Flags().GetBool("local"); local {
		return fmt.Errorf("--via-server and --local cannot be combined: --local installs the machine kip runs on")
	}
	if admin, _ := cmd.Flags().GetBool("admin-kubeconfig"); admin {
		return fmt.Errorf("--admin-kubeconfig requires a direct install; run kip install --via-server=false from Linux or macOS")
	}
	if host == "" {
		return fmt.Errorf("--host is required")
	}
	isTTY := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))

	explicitKey, fallbackKey := resolveSSHKey(sshKeyFlag, nil)
	keyPath := explicitKey
	if keyPath == "" {
		keyPath = fallbackKey
	}
	user := installSSHUser(cmd, host)
	if err := ensureKeyAccess(user, host, keyPath, isTTY); err != nil {
		return err
	}

	client, err := ssh.Dial(ssh.Config{Host: host, User: user, KeyPath: keyPath, Options: []string{"BatchMode=yes"}})
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	if cancel {
		out, err := client.Run("if command -v kip >/dev/null; then kip install-job cancel; else echo " + installjob.CancelIdle + "; fi")
		if err != nil {
			return err
		}
		switch lastLine(out) {
		case installjob.CancelStopped:
			fmt.Printf("\n  ✔  The install on %s was stopped. Run kip install again to retry the installation.\n\n", host)
		case installjob.CancelDiscarded:
			fmt.Printf("\n  ✔  The previous install result on %s was discarded. Run kip install again to start a new install.\n", host)
			fmt.Printf("     If you need a new admin password, run kip auth reset-password.\n\n")
		default:
			fmt.Printf("\n  No install is running on %s.\n\n", host)
		}
		return nil
	}

	arch, err := preflightServer(client)
	if err != nil {
		return err
	}
	kip, err := ensureServerKip(client, arch, installingKipVersion(), serverKip)
	if err != nil {
		return err
	}
	kipCmd := ssh.Quote(kip)

	req, err := drivenRequest(cmd, host)
	if err != nil {
		return err
	}
	if err := askToAdoptFirewall(client, kipCmd, host, &req, adoptionQuestion(isTTY)); err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	out, err := client.RunStdin(kipCmd+" install-job claim --kip "+kipCmd, bytes.NewReader(body))
	if err != nil {
		return err
	}
	var outcome installjob.Outcome
	if err := json.Unmarshal([]byte(lastJSONLine(out)), &outcome); err != nil {
		return fmt.Errorf("reading the server's answer to the install request: %w", err)
	}

	switch outcome.Action {
	case installjob.ActionAttach:
		fmt.Printf("\n  An install is already running on %s; following it.\n", host)
		fmt.Printf("  The running install keeps its original options.\n%s\n", startedBy(outcome.KipVersion))
	case installjob.ActionCollect:
		fmt.Printf("\n  Collecting the completed install result from %s.\n%s\n", host, startedBy(outcome.KipVersion))
	default:
		if req.NoSSHRateLimit {
			fmt.Print(serverJobRateLimitNotice())
		}
		fmt.Printf("\n  Installing on %s. Ctrl-C disconnects; the install continues on the server.\n\n", host)
	}

	phase := installjob.PhaseSucceeded
	if outcome.Action != installjob.ActionCollect {
		interrupts := make(chan os.Signal, 1)
		signal.Notify(interrupts, os.Interrupt)
		go func() {
			<-interrupts
			fmt.Printf("\n\n  Disconnected. The install continues on %s. Run the same command to reconnect.\n\n", host)
			os.Exit(130)
		}()
		acks := &firewallAckWriter{out: os.Stdout, runID: outcome.RunID, ack: func(nonce string) error {
			_, err := client.RunFresh(kipCmd + " install-job firewall-ack --run " + outcome.RunID + " --nonce " + nonce)
			return err
		}}
		phase, err = followJob(client, kipCmd, outcome.RunID, acks, 5, 3*time.Second)
		_ = acks.Flush()
		signal.Stop(interrupts)
		if err != nil {
			return err
		}
	}

	resOut, err := client.Run(kipCmd + " install-job result --run " + outcome.RunID)
	if err != nil {
		if phase != installjob.PhaseSucceeded {
			return stoppedJobError(host, outcome.RunID, phase)
		}
		// Omit the Run error because its captured output may contain the admin password.
		return fmt.Errorf("collecting the install result failed. Run kip install --via-server again on %s to collect it", host)
	}
	var res installjob.Result
	if err := json.Unmarshal([]byte(lastJSONLine(resOut)), &res); err != nil {
		return fmt.Errorf("reading the install result: %w", err)
	}
	if outcome.Action == installjob.ActionCollect {
		phase = res.Outcome
	}
	if res.Cluster != nil {
		if err := saveDrivenResult(res, host, keyPath, sshUserEntry(user)); err != nil {
			return fmt.Errorf("%w\n\n  The result stays on the server. To discard it and start again: kip install --host %s --cancel", err, host)
		}
	}
	if _, err := client.Run(kipCmd + " install-job ack --run " + outcome.RunID); err != nil {
		fmt.Printf("  ⚠  The result was saved here but not cleared on the server: %v\n", err)
	}
	if res.Collected && res.Outcome == installjob.PhaseSucceeded {
		fmt.Printf("\n  Another machine collected this install's admin password first. kip auth reset-password issues a new one.\n")
	}
	announceCollectedCredentials(os.Stdout, credentialsFile, res)
	if phase != installjob.PhaseSucceeded || res.Outcome != installjob.PhaseSucceeded {
		reportInstallFailure(os.Stdout, res.IssueLink, res.ErrorReport)
		return fmt.Errorf("the install on %s did not finish (%s); run kip install again to retry", host, phase)
	}
	if res.Cluster != nil {
		fmt.Printf("\n  ✔  Cluster %s added to this machine as the active cluster\n", res.Cluster.Name)
		if installer.KipPinned() {
			fmt.Printf("  %s\n", installer.PinnedKipWarning)
		}
	}
	// Worker upgrades use this machine's SSH credentials after the server job finishes.
	workerSSH := ssh.Config{User: user, KeyPath: keyPath, Options: []string{"BatchMode=yes"}}
	if err := upgradeAgentsAfterInstall(os.Stdout, client, sshDialer(workerSSH), "kip install"); err != nil {
		return err
	}
	if noLogin {
		fmt.Printf("\n  Sign in later with: kip auth login\n\n")
		return nil
	}
	return runAuthLogin(authLoginCmd, nil)
}

func announceCollectedCredentials(out io.Writer, credentialsFile string, res installjob.Result) {
	if res.AdminPassword == "" {
		return
	}
	_, _ = fmt.Fprintln(out)
	if installer.SaveAdminCredentials(out, credentialsFile, res.AdminEmail, res.AdminPassword) {
		return
	}
	_, _ = fmt.Fprintf(out, "  Admin email: %s\n  Password:    %s\n  Save the password now; it is cleared from the server result after collection.\n", res.AdminEmail, res.AdminPassword)
}

func startedBy(version string) string {
	if version == "" || version == installingKipVersion() {
		return ""
	}
	return fmt.Sprintf("  It was started by kip %s; this is kip %s.\n", version, installingKipVersion())
}

// drivenRequest resolves local settings and credentials before sending the
// install request to the server.
func drivenRequest(cmd *cobra.Command, host string) (installjob.Request, error) {
	flags := cmd.Flags()
	str := func(name string) string { v, _ := flags.GetString(name); return v }
	boolean := func(name string) bool { v, _ := flags.GetBool(name); return v }
	slice := func(name string) []string { v, _ := flags.GetStringSlice(name); return v }

	domain := installer.NormaliseDomain(str("domain"))
	_, wantsGatewayName, err := installer.KipperRunLabelFor(domain, host)
	if err != nil {
		return installjob.Request{}, err
	}
	adminEmail := str("admin-email")
	if domain != "" && !wantsGatewayName && !flags.Changed("admin-email") {
		adminEmail = "admin@" + domain
	}
	req := installjob.Request{
		Version:          installjob.RequestVersion,
		RunID:            "run-" + time.Now().UTC().Format("20060102-150405") + "-" + randomSuffix(),
		KipVersion:       installingKipVersion(),
		Host:             host,
		Domain:           domain,
		ConsoleDomain:    str("console-domain"),
		ConsoleAPIDomain: str("console-api-domain"),
		DexDomain:        str("dex-domain"),
		AdminEmail:       adminEmail,
		Org:              str("org"),
		OrgDisplayName:   str("org-display-name"),
		Harden:           boolean("harden"),
		Firewall:         boolean("firewall"),
		AdoptFirewall:    boolean("adopt-firewall"),
		ChangeNetwork:    boolean("yes"),
		// Allow maintenance bursts from Windows, which opens a connection per command.
		NoSSHRateLimit:  !flags.Changed("no-ssh-rate-limit") || boolean("no-ssh-rate-limit"),
		DNSResolvers:    slice("dns-resolver"),
		TrustedProxies:  slice("trusted-proxy"),
		Profile:         str("profile"),
		Storage:         str("storage"),
		SkipMemoryFloor: boolean(skipMemoryFloorFlag),
	}
	backup, err := resolveBackupStorage(cmd)
	if err != nil {
		return installjob.Request{}, err
	}
	if backup != nil {
		req.Backup = &installjob.Backup{Bucket: backup.Bucket, Region: backup.Region, Endpoint: backup.Endpoint,
			AccessKeyID: backup.AccessKeyID, SecretAccessKey: backup.SecretAccessKey, SSEC: backup.SSEC, SSECKey: backup.SSECKey}
	}
	if path := str("backup-repository-key-file"); path != "" {
		key, err := installer.ReadRepositoryKeyFile(path)
		if err != nil {
			return installjob.Request{}, err
		}
		req.BackupRepositoryKey = string(key)
	}
	if cfg, err := config.Load(); err == nil {
		if existing := cfg.GetClusterByHost(host); existing != nil {
			entry := *existing
			req.Entry = &entry
		}
	}
	return req, nil
}

// preflightServer checks root access and required tools and returns the architecture.
func preflightServer(client *ssh.Client) (string, error) {
	out, err := client.Run(`echo "uid:$(id -u)"; echo "arch:$(uname -m)"; for t in bash curl systemd-run sha256sum mktemp flock; do command -v "$t" >/dev/null || echo "missing:$t"; done; test -d /run/systemd/system || echo no-systemd`)
	if err != nil {
		return "", fmt.Errorf("checking the server: %w", err)
	}
	return parsePreflight(out)
}

// parsePreflight reads labelled results so SSH notices can be ignored.
func parsePreflight(out string) (string, error) {
	var uid, arch string
	var missing []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "uid:"):
			uid = strings.TrimPrefix(line, "uid:")
		case strings.HasPrefix(line, "arch:"):
			arch = strings.TrimPrefix(line, "arch:")
		case strings.HasPrefix(line, "missing:"):
			missing = append(missing, strings.TrimPrefix(line, "missing:"))
		case line == "no-systemd":
			return "", fmt.Errorf("the install requires systemd on the server to continue after this machine disconnects")
		}
	}
	if uid != "0" {
		return "", fmt.Errorf("the install must connect as root; the server reported UID %q", uid)
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("install the required tools on the server: %s", strings.Join(missing, ", "))
	}
	return serverArch(arch)
}

// ensureServerKip installs a checksum-verified release or the --server-kip binary
// and links it from the server's PATH. It returns the checksum-specific path
// so later installs can update the link without changing this job's binary.
func ensureServerKip(client *ssh.Client, arch, version, serverKip string) (string, error) {
	var target string
	if serverKip != "" {
		data, err := os.ReadFile(serverKip) //nolint:gosec // the operator names the binary to upload
		if err != nil {
			return "", err
		}
		if err := checkLinuxBinary(data, arch); err != nil {
			return "", fmt.Errorf("--server-kip %s: %w", serverKip, err)
		}
		sum := fmt.Sprintf("%x", sha256.Sum256(data))
		target = serverKipFor(sum)
		staged := target + ".upload-" + randomSuffix()
		if _, err := client.Run("mkdir -p " + installjob.KipDir); err != nil {
			return "", err
		}
		if err := client.Upload(bytes.NewReader(data), staged, 0o755); err != nil {
			return "", err
		}
		if _, err := client.Run(publishUploadCommand(staged, sum, target)); err != nil {
			return "", fmt.Errorf("putting --server-kip in place: %w", err)
		}
	} else {
		tag, ok := releaseTag(version)
		if !ok {
			return "", fmt.Errorf("this kip (%s) has no release for the server to download; build kip for linux/%s and pass it with --server-kip", version, arch)
		}
		base := "https://github.com/getkipper/kipper/releases/download/" + tag
		sums, err := httpGetText(base + "/checksums.txt")
		if err != nil {
			return "", fmt.Errorf("fetching the release checksums: %w; build kip for linux/%s and pass it with --server-kip", err, arch)
		}
		sum, err := checksumFor(sums, arch)
		if err != nil {
			return "", err
		}
		target = serverKipFor(sum)
		// Verify the server's download against checksums fetched by this client.
		if _, err := client.Run(fetchKipCommand(fmt.Sprintf("%s/kip-linux-%s", base, arch), sum, target)); err != nil {
			return "", fmt.Errorf("putting kip %s on the server: %w", version, err)
		}
	}
	if _, err := client.Run(linkKipCommand(target, serverKipPath)); err != nil {
		return "", fmt.Errorf("linking %s: %w", serverKipPath, err)
	}
	return target, nil
}

func httpGetText(url string) (string, error) {
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Get(url) //nolint:gosec,noctx // a fixed GitHub release URL built from this kip's own version
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(body), err
}

// checkLinuxBinary checks that --server-kip is an ELF file for the target architecture.
func checkLinuxBinary(data []byte, arch string) error {
	f, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("invalid ELF binary")
	}
	want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]
	if f.Machine != want {
		return fmt.Errorf("built for %s, the server is %s", f.Machine, arch)
	}
	return nil
}

// ensureKeyAccess verifies key access to user@host. Interactive sessions can
// create a key and authorize it through a separate SSH login.
func ensureKeyAccess(user, host, keyPath string, isTTY bool) error {
	login := user + "@" + host
	if keyPath == "" {
		return fmt.Errorf("no SSH key: pass --ssh-key or set KIP_SSH_KEY")
	}
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		if !isTTY || !confirm(fmt.Sprintf("No SSH key at %s. Create one now?", keyPath)) {
			return fmt.Errorf("no SSH key at %s; create one with ssh-keygen or pass --ssh-key", keyPath)
		}
		// Ensure the key directory exists on fresh user profiles.
		if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
			return err
		}
		gen := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", keyPath) //nolint:gosec // the operator's own key path
		gen.Stdout, gen.Stderr = os.Stdout, os.Stderr
		if err := gen.Run(); err != nil {
			return fmt.Errorf("creating the SSH key: %w", err)
		}
	}
	locked, err := keyHasPassphrase(keyPath)
	if err != nil {
		return err
	}
	if locked && !agentHoldsKey(keyPath) {
		return fmt.Errorf("the key %s has a passphrase, and kip's connections cannot ask for it. Start ssh-agent and add the key with: ssh-add %s", keyPath, keyPath)
	}
	probe := func() (bool, string) {
		var output bytes.Buffer
		c := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=10", //nolint:gosec // the operator's host and key
			"-i", keyPath, "-o", "IdentitiesOnly=yes", login, "true")
		c.Env = ssh.EnvironWithoutLocale()
		// Capture stdout too because cloud images can print root-login refusals there.
		c.Stdout, c.Stderr = &output, &output
		return c.Run() == nil, output.String()
	}
	ok, stderr := probe()
	if ok {
		return nil
	}
	if cloudUser, named := ssh.CloudImageUser(stderr); named && user == "root" {
		return ssh.RootRefused(cloudUser)
	}
	if offer, err := probeFailure(user, host, stderr); !offer {
		return err
	}
	if !isTTY || !confirm(fmt.Sprintf("Add the key %s to %s? SSH may prompt for %s.", keyPath, login, passwordOf(user))) {
		return fmt.Errorf("key login to %s failed. Add %s.pub to %s on the server", login, keyPath, authorizedKeysOf(user))
	}
	pub, err := os.ReadFile(keyPath + ".pub") //nolint:gosec // the operator's own public key
	if errors.Is(err, os.ErrNotExist) {
		pub, err = exec.Command("ssh-keygen", "-y", "-f", keyPath).Output() //nolint:gosec // the operator's own key path
	}
	if err != nil {
		return fmt.Errorf("reading the public key for %s: %w", keyPath, err)
	}
	var placeErr bytes.Buffer
	place := exec.Command("ssh", "-o", "StrictHostKeyChecking=accept-new", login, //nolint:gosec // the operator's host
		"umask 077 && mkdir -p ~/.ssh && cat >> ~/.ssh/authorized_keys")
	place.Env = ssh.EnvironWithoutLocale()
	place.Stdin = bytes.NewReader(pub)
	place.Stdout = os.Stdout
	place.Stderr = io.MultiWriter(os.Stderr, &placeErr)
	if err := place.Run(); err != nil {
		if msg := strings.ToLower(placeErr.String()); strings.Contains(msg, "password change") || strings.Contains(msg, "expired") {
			return fmt.Errorf("the server requires a change to %s first. Run ssh %s, change the password, then retry this command", passwordOf(user), login)
		}
		return placementError(user, host, placeErr.String(), err)
	}
	if ok, _ := probe(); !ok {
		return fmt.Errorf("the key was added but key login to %s still fails", login)
	}
	return nil
}

// keyHasPassphrase reports whether the private key at path needs a
// passphrase, and an error when the key cannot be read for another reason.
func keyHasPassphrase(path string) (bool, error) {
	var stderr bytes.Buffer
	c := exec.Command("ssh-keygen", "-y", "-P", "", "-f", path) //nolint:gosec // the operator's own key path
	c.Stderr = &stderr
	if c.Run() == nil {
		return false, nil
	}
	return keyCheckFailure(path, stderr.String())
}

func keyCheckFailure(path, stderr string) (bool, error) {
	if strings.Contains(stderr, "passphrase") {
		return true, nil
	}
	return false, fmt.Errorf("cannot use the SSH key %s: %s", path, lastLine(stderr))
}

// sshAuthDenied captures the authentication methods listed in an SSH login failure.
var sshAuthDenied = regexp.MustCompile(`Permission denied \(([^)]*)\)`)

// placementError adds recovery steps for SSH authentication failures.
func placementError(user, host, stderr string, err error) error {
	m := sshAuthDenied.FindStringSubmatch(stderr)
	switch {
	case strings.Contains(stderr, "Permission denied, please try again") || (len(m) == 2 && strings.Contains(m[1], "password")):
		return fmt.Errorf("adding the key to the server: %s@%s refused %s. Use your current password, including any change made at first login. If you have lost it, reset it in your provider's console and retry", user, host, passwordOf(user))
	case len(m) == 2:
		return fmt.Errorf("adding the key to the server: %s@%s accepts no password logins (%s). Add your public key to %s through your provider's console and retry", user, host, strings.TrimSpace(m[0]), authorizedKeysOf(user))
	}
	if reason := lastLine(stderr); reason != "" {
		return fmt.Errorf("adding the key to the server: %s", reason)
	}
	return fmt.Errorf("adding the key to the server: %w", err)
}

// agentHoldsKey reports whether ssh-agent holds the key at path.
func agentHoldsKey(path string) bool {
	// Older ssh-keygen cannot fingerprint an encrypted private key, so the
	// public half is read when it is there.
	if _, err := os.Stat(path + ".pub"); err == nil {
		path += ".pub"
	}
	key, err := exec.Command("ssh-keygen", "-l", "-f", path).Output() //nolint:gosec // the operator's own key path
	if err != nil {
		return false
	}
	agent, _ := exec.Command("ssh-add", "-l").Output()
	return agentHolds(string(key), string(agent))
}

// agentHolds reports whether the fingerprint in ssh-keygen -l's output for a
// key appears in ssh-add -l's listing.
func agentHolds(keyFingerprint, agentList string) bool {
	fields := strings.Fields(keyFingerprint)
	if len(fields) < 2 {
		return false
	}
	for _, line := range strings.Split(agentList, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[1] == fields[1] {
			return true
		}
	}
	return false
}

// probeFailure offers key placement for permission failures and reports host-key
// or connection failures directly.
func probeFailure(user, host, stderr string) (offerPlacement bool, err error) {
	switch {
	case strings.Contains(stderr, "REMOTE HOST IDENTIFICATION HAS CHANGED"), strings.Contains(stderr, "Host key verification failed"):
		knownHosts := "~/.ssh/known_hosts"
		if home, err := os.UserHomeDir(); err == nil {
			knownHosts = filepath.Join(home, ".ssh", "known_hosts")
		}
		return false, fmt.Errorf("%s presents a different host key from the one recorded for it. If the server was reinstalled, remove its line from %s and run this again", host, knownHosts)
	case strings.Contains(stderr, "Permission denied"):
		return true, nil
	}
	return false, fmt.Errorf("cannot reach %s@%s: %s", user, host, lastLine(stderr))
}

// passwordOf names the password SSH asks for when it logs in as user.
func passwordOf(user string) string {
	if user == "root" {
		return "the root password"
	}
	return "the password of " + user
}

// authorizedKeysOf names user's authorized_keys file in shell notation.
func authorizedKeysOf(user string) string {
	if user == "root" {
		return "/root/.ssh/authorized_keys"
	}
	return "~" + user + "/.ssh/authorized_keys"
}

func confirm(question string) bool {
	fmt.Printf("\n  %s [y/N] ", question)
	var answer string
	_, _ = fmt.Scanln(&answer)
	return isYes(answer)
}

// askToAdoptFirewall collects consent on the client because server jobs have no
// terminal. Existing jobs keep their original options.
func askToAdoptFirewall(client *ssh.Client, kipCmd, host string, req *installjob.Request, ask func(string) bool) error {
	if req.AdoptFirewall || !req.Firewall || ask == nil {
		return nil
	}
	out, err := client.Run(kipCmd + " install-job status")
	if err != nil {
		return fmt.Errorf("checking for an existing install on %s: %w", host, err)
	}
	if existingJob(lastJSONLine(out)) {
		return nil
	}
	audit, err := installer.AuditFirewall(client)
	if err != nil {
		return fmt.Errorf("checking the firewall on %s: %w", host, err)
	}
	// Bind consent to the peers shown here so the job can detect changes.
	consent, err := installer.AdoptionConsent(client, host, audit, req.Firewall, false, ask, installer.ServerFirewallMesh(client))
	if err != nil {
		return err
	}
	req.AdoptFirewall, req.AdoptFingerprint = consent.Adopt, consent.Fingerprint
	return nil
}

// existingJob reports whether install-job claim would attach to or collect a job.
func existingJob(statusJSON string) bool {
	var st installjob.Status
	if err := json.Unmarshal([]byte(statusJSON), &st); err != nil || st.RunID == "" {
		return false
	}
	return st.Phase == installjob.PhaseStarting || st.Phase == installjob.PhaseRunning || st.Collectable
}

func serverJobRateLimitNotice() string {
	return "\n  SSH rate limiting is off by default for server jobs to support Windows SSH clients.\n" +
		"  On Ubuntu and Debian, enable it with --no-ssh-rate-limit=false.\n"
}

func stoppedJobError(host, runID, phase string) error {
	return fmt.Errorf("the install on %s stopped before it produced a result (%s); check the log above for details. If the log is empty, run journalctl -u %s on the server. Running kip install again retries it",
		host, phase, installjob.UnitName(runID))
}

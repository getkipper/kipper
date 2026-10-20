package installer

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"regexp"
	"strings"
)

const (
	accessDir = "/etc/kipper/access"
	// accessLock is outside accessDir so the per-host removal can delete the
	// directory while waiters keep the same lock inode.
	accessLock         = "/etc/kipper/access.lock"
	accessUnitName     = "kipper-access.service"
	accessUnitPath     = "/etc/systemd/system/" + accessUnitName
	accessRollbackUnit = "kipper-access-rollback"
	accessLoadUnit     = "kipper-access-load"
	accessK3sBinary    = "/usr/local/bin/k3s"
	// accessRollbackSeconds is the rollback delay renewed by every locked
	// command of a run.
	accessRollbackSeconds = 180
	// accessTimeout bounds command execution and sets the C locale so scripts
	// can identify an absent table by nft's untranslated error text.
	accessTimeout = "LC_ALL=C LANGUAGE=C timeout -k 10 60 "
	// accessRefusedMarker identifies commands refused after a rollback or
	// loss of the reservation. Commands print it in two halves so an echoed
	// command line never matches.
	accessRefusedMarker = "kip-access-change-refused"
	accessRefusedEcho   = "printf '%s%s\\n' 'kip-access-change' '-refused' >&2"
)

// errAccessChangeRefused reports a command refused under the access lock.
var errAccessChangeRefused = errors.New("the access change was rolled back or taken over by another run")

// accessUnit is the boot unit that converges the kernel to the committed
// files before sshd, the SSH socket and k3s start.
func accessUnit() string {
	return `[Unit]
Description=Kipper operator access table
DefaultDependencies=no
ConditionPathExists=` + accessDir + `/table.nft
After=local-fs.target systemd-modules-load.service
Before=sysinit.target network-pre.target shutdown.target
Wants=network-pre.target
Conflicts=shutdown.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/sh ` + accessDir + `/access.sh load

[Install]
WantedBy=sysinit.target
`
}

// accessScript is access.sh. load converges the kernel to the committed
// files, candidate loads a run's candidate files, and rollback restores the
// committed files once a run's deadline has passed. Tests use root to
// redirect host paths into a temporary directory.
func accessScript(root string) string {
	d := root + accessDir
	return `export LC_ALL=C LANGUAGE=C
d=` + d + `
umask 077
pair() {
  printf 'include "%s"\ninclude "%s"\n' "$1" "$2" | ` + accessTimeout + `nft -f -
}
load() {
  if [ -s $d/operators.nft ]; then
    pair $d/table.nft $d/operators.nft && { rm -f $d/closed $d/failed; return 0; }
  fi
  ` + accessTimeout + `nft -f $d/table.nft && { touch $d/closed; rm -f $d/failed; return 0; }
  return 1
}
exec 9>>` + root + accessLock + ` || exit 1
case "$1" in
load)
  flock 9 || exit 1
  load || exit 1
  rm -f $d/run $d/next-table.nft $d/next-operators.nft
  ;;
candidate)
  id=$2
  flock 9 || exit 1
  if [ -e $d/$id.rolledback ] || [ "$(cat $d/run 2>/dev/null)" != "$id" ]; then
    ` + accessRefusedEcho + `; exit 1
  fi
  echo $(( $(date +%s) + ` + fmt.Sprint(accessRollbackSeconds) + ` )) > $d/$id.deadline
  pair $d/next-table.nft $d/next-operators.nft
  ;;
rollback)
  id=$2
  while :; do
    flock 9 || exit 1
    [ -e $d/$id.committed ] && exit 0
    [ "$(cat $d/run 2>/dev/null)" = "$id" ] || exit 0
    now=$(date +%s); dl=$(cat $d/$id.deadline 2>/dev/null); dl=${dl:-0}
    [ "$now" -ge "$dl" ] && break
    flock -u 9; sleep $((dl - now))
  done
  touch $d/$id.rolledback; rm -f $d/run
  if [ -s $d/table.nft ]; then
    if load; then
      if [ -e $d/closed ]; then echo closed > $d/$id.outcome; else echo restored > $d/$id.outcome; fi
    elif ` + accessTimeout + `nft -f $d/next-table.nft; then
      touch $d/closed; echo closed > $d/$id.outcome
    else
      echo failed > $d/$id.outcome; touch $d/failed; rm -f $d/next-table.nft $d/next-operators.nft; exit 1
    fi
  elif out=$(` + accessTimeout + `nft list table ` + accessTable + ` 2>&1 >/dev/null); then
    if ` + accessTimeout + `nft delete table ` + accessTable + `; then
      echo removed > $d/$id.outcome
    else
      echo failed > $d/$id.outcome; touch $d/failed; exit 1
    fi
  else
    case "$out" in
      *"No such file or directory"*) echo removed > $d/$id.outcome ;;
      *) printf '%s\n' "$out" >&2; echo failed > $d/$id.outcome; touch $d/failed; exit 1 ;;
    esac
  fi
  rm -f $d/next-table.nft $d/next-operators.nft
  ;;
esac
`
}

// accessRun is one access change on one host: a reservation, a rollback
// timer and candidate files coordinated through the access lock.
type accessRun struct {
	id, root string
}

func newAccessRun() (accessRun, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return accessRun{}, fmt.Errorf("naming the access change: %w", err)
	}
	return accessRun{id: hex.EncodeToString(b)}, nil
}

func (r accessRun) dir() string             { return r.root + accessDir }
func (r accessRun) file(name string) string { return r.dir() + "/" + name }
func (r accessRun) lock() string            { return r.root + accessLock }
func (r accessRun) script() string          { return r.file("access.sh") }
func (r accessRun) timer() string           { return accessRollbackUnit + "-" + r.id }
func (r accessRun) loader() string          { return accessLoadUnit + "-" + r.id }

func (r accessRun) owns() string {
	return fmt.Sprintf(`[ "$(cat %s 2>/dev/null)" = %s ]`, r.file("run"), r.id)
}

func (r accessRun) renew() string {
	return fmt.Sprintf("echo $(( $(date +%%s) + %d )) > %s", accessRollbackSeconds, r.file(r.id+".deadline"))
}

// locked wraps command so it runs only while this run owns the reservation
// and no rollback has started, renewing the deadline first. The command is
// bounded by its own timeout while the shell holds the lock.
func (r accessRun) locked(command string) string {
	return fmt.Sprintf("(exec 9>>%s && flock 9 || exit 1; if [ -e %s ] || ! %s; then %s; exit 1; fi; %s; %s/bin/sh -c %s)",
		r.lock(), r.file(r.id+".rolledback"), r.owns(), accessRefusedEcho, r.renew(), accessTimeout, shellQuote(command))
}

// writeFile builds a command that writes body to path and exits when the
// write fails or leaves fewer bytes than body holds.
func writeFile(path, body string) string {
	return fmt.Sprintf("cat > %[1]s <<'KIPEOF' || exit 1\n%[2]sKIPEOF\n[ \"$(wc -c < %[1]s)\" -eq %[3]d ] || exit 1\n", path, body, len(body))
}

// writeIfChanged builds a command that replaces path with body through a
// temporary file and runs onChange only when the bytes differ. If mode is set,
// it is applied before replacement or to the existing file if the bytes match.
// A failed write leaves path unchanged and exits the shell.
func writeIfChanged(path, body, mode, onChange string) string {
	tmp := path + ".kip-new"
	cmd := writeFile(tmp, body)
	keep := "rm -f " + tmp
	if mode != "" {
		cmd += fmt.Sprintf("chmod %s %s || exit 1\n", mode, tmp)
		keep += fmt.Sprintf("; chmod %s %s || exit 1", mode, path)
	}
	cmd += fmt.Sprintf("if cmp -s %[1]s %[2]s; then %[3]s; else mv -f %[1]s %[2]s || exit 1", tmp, path, keep)
	if onChange != "" {
		cmd += "; " + onChange
	}
	return cmd + "; fi\n"
}

// reserveAndArmCommand builds the one locked shell that takes the
// reservation, installs the unit and script, writes both candidates and arms
// the rollback timer. It refuses while another run's timer, rollback or load
// unit is active, on a host without k3s, and when a committed file is newer
// than generation.
func (r accessRun) reserveAndArmCommand(table, operators string, generation int) string {
	d := r.dir()
	var b strings.Builder
	b.WriteString("(umask 077\n")
	fmt.Fprintf(&b, "mkdir -p %s && mkdir -p -m 0700 %s && chmod 0700 %s || exit 1\n", r.root+"/etc/kipper", d, d)
	fmt.Fprintf(&b, "exec 9>>%s && flock 9 || exit 1\n", r.lock())
	fmt.Fprintf(&b, "find %s -maxdepth 1 -type f \\( -name '*.committed' -o -name '*.rolledback' -o -name '*.deadline' -o -name '*.outcome' \\) -mmin +1440 -exec rm -f {} + || exit 1\n", d)
	fmt.Fprintf(&b, "[ -e %s ] || { echo 'k3s is not installed on this host, so it is no longer a cluster node' >&2; exit 1; }\n", r.root+accessK3sBinary)
	fmt.Fprintf(&b, "units=$(systemctl list-units --all --state=active,activating --plain --no-legend '%s-*' '%s-*') || exit 1\n", accessRollbackUnit, accessLoadUnit)
	fmt.Fprintf(&b, "if [ -n \"$units\" ]; then echo \"another operator access change is in progress on this host ($(echo $units | cut -d' ' -f1)). Run the command again in %d seconds\" >&2; exit 1; fi\n", accessRollbackSeconds)
	for _, f := range []string{"table.nft", "operators.nft"} {
		fmt.Fprintf(&b, "g=$(sed -n '1s/^# kipper-access generation \\([0-9][0-9]*\\)$/\\1/p' %s 2>/dev/null)\n", r.file(f))
		fmt.Fprintf(&b, "if [ -n \"$g\" ] && [ \"$g\" -gt %d ]; then echo \"this host already holds access generation $g, newer than %d, so an older change cannot replace it\" >&2; exit 1; fi\n", generation, generation)
	}
	b.WriteString(writeIfChanged(r.script(), accessScript(r.root), "", ""))
	// The shell's umask keeps access files private. This unit contains no secrets,
	// so make it readable by everyone to avoid systemd warnings on each reload.
	b.WriteString(writeIfChanged(r.root+accessUnitPath, accessUnit(), "0644", "systemctl daemon-reload || exit 1"))
	fmt.Fprintf(&b, "systemctl enable %s >/dev/null 2>&1; systemctl is-enabled --quiet %s || { echo 'systemd did not enable %s' >&2; exit 1; }\n", accessUnitName, accessUnitName, accessUnitName)
	for _, c := range []struct{ name, body string }{{"next-table.nft", table}, {"next-operators.nft", operators}} {
		b.WriteString(writeFile(r.file(c.name)+".kip-new", c.body))
		fmt.Fprintf(&b, "mv -f %s.kip-new %s || exit 1\n", r.file(c.name), r.file(c.name))
	}
	fmt.Fprintf(&b, "printf '%%s\\n' %s > %s || exit 1\n", r.id, r.file("run"))
	fmt.Fprintf(&b, "%ssync -f %s || exit 1\n", accessTimeout, d)
	fmt.Fprintf(&b, "command -v restorecon >/dev/null 2>&1 && restorecon -R %s %s\n", d, r.root+accessUnitPath)
	fmt.Fprintf(&b, "%s || exit 1\n", r.renew())
	fmt.Fprintf(&b, "systemd-run --quiet --unit=%s --on-active=%d --timer-property=RemainAfterElapse=no --timer-property=AccuracySec=1s /bin/sh %s rollback %s)",
		r.timer(), accessRollbackSeconds, r.script(), r.id)
	return b.String()
}

// loadCommand loads the candidate pair through a named transient unit that
// takes the access lock itself, so it is not wrapped in locked.
func (r accessRun) loadCommand() string {
	return fmt.Sprintf("systemd-run --wait --pipe --quiet --collect --unit=%s /bin/sh %s candidate %s", r.loader(), r.script(), r.id)
}

// commitCommand renames the candidates over the committed files, operators
// first, then stops the timer. The committed marker stops a late rollback
// when stopping the timer fails.
func (r accessRun) commitCommand() string {
	return r.locked(fmt.Sprintf("mv -f %[1]s/next-operators.nft %[1]s/operators.nft && mv -f %[1]s/next-table.nft %[1]s/table.nft && sync -f %[1]s && touch %[1]s/%[2]s.committed && rm -f %[1]s/run %[1]s/%[2]s.deadline %[1]s/closed %[1]s/failed", r.dir(), r.id)) +
		fmt.Sprintf(" && { systemctl stop %s.timer >/dev/null 2>&1 || true; command -v restorecon >/dev/null 2>&1 && restorecon -R %s; true; }", r.timer(), r.dir())
}

// rollBackNowCommand requests the restore at once and prints its outcome,
// or "unknown" when none was recorded.
func (r accessRun) rollBackNowCommand() string {
	return "{ " + r.locked("echo 0 > "+r.file(r.id+".deadline")) + fmt.Sprintf(" && sh %s rollback %s; }; cat %s 2>/dev/null || echo unknown", r.script(), r.id, r.file(r.id+".outcome"))
}

// accessRefused maps the refusal marker in a failed command's output to
// errAccessChangeRefused.
func accessRefused(out string, err error) error {
	if err != nil && strings.Contains(out, accessRefusedMarker) {
		return fmt.Errorf("%w: %w", errAccessChangeRefused, err)
	}
	return err
}

// AccessApply configures an access change on one node. Node must use the node
// lease so a lost lease stops mutations. Base bypasses the lease for rollback.
// Families and ReadyzOn contain "4" or "6". Readyz must be set when ReadyzOn
// is non-empty. A nil Out uses os.Stdout.
type AccessApply struct {
	Node     FirewallRunner
	Base     FirewallRunner
	Label    string
	Intent   AccessIntent
	Families []string
	Readyz   func(family string) error
	ReadyzOn []string
	Out      io.Writer
	root     string
}

// Outcome strings recorded in the access history.
const (
	AccessCommitted = "committed"
	AccessRestored  = "restored"
	AccessClosed    = "closed"
	AccessRemoved   = "removed"
	AccessFailed    = "failed"
	AccessRefused   = "refused"
)

// ApplyAccessGuard loads a node's candidate table and commits it once new
// SSH connections in every family of a.Families arrive from an admitted
// address and, on the server, /readyz answers in a.ReadyzOn. A failed load or
// check requests immediate rollback. It returns AccessCommitted on success
// and an empty outcome for preparation errors. AccessRefused means the run
// lost its reservation before committing. Other failures return the rollback
// outcome or AccessFailed when the resulting state cannot be confirmed.
// Preparation may install nftables and write service and candidate files.
func ApplyAccessGuard(a AccessApply) (string, error) {
	out := a.Out
	if out == nil {
		out = os.Stdout
	}
	table, err := RenderAccessTable(a.Intent)
	if err != nil {
		return "", err
	}
	operators, err := RenderAccessOperators(a.Intent)
	if err != nil {
		return "", err
	}
	if err := prepareAccessHost(a.Node, out); err != nil {
		return "", fmt.Errorf("%s: %w", a.Label, err)
	}
	if _, err := a.Node.RunStdin(accessTimeout+"nft -c -f -", strings.NewReader(table+operators)); err != nil {
		return "", fmt.Errorf("%s: nft refused the rendered access table, so the live access table was left unchanged: %w", a.Label, err)
	}
	r, err := newAccessRun()
	if err != nil {
		return "", err
	}
	r.root = a.root
	if _, err := a.Node.Run(r.reserveAndArmCommand(table, operators, a.Intent.Generation)); err != nil {
		return "", fmt.Errorf("%s: kip could not prepare the access change, so it left the live access table unchanged: %w", a.Label, err)
	}
	if err := r.loadAndVerify(a); err != nil {
		if errors.Is(err, errAccessChangeRefused) {
			return AccessRefused, fmt.Errorf("%s: %w", a.Label, err)
		}
		outcome, rbErr := r.rollBackNow(a.Base)
		return outcome, fmt.Errorf("%s: %w. %w", a.Label, err, rbErr)
	}
	if err := accessRefused(a.Node.Run(r.commitCommand())); err != nil {
		return AccessFailed, fmt.Errorf("%s: kip could not commit the access change (%w). If the rollback is still armed, the host restores its committed access table about %d seconds after kip's last step. Run 'kip cluster access show'", a.Label, err, accessRollbackSeconds)
	}
	return AccessCommitted, nil
}

func (r accessRun) loadAndVerify(a AccessApply) error {
	if err := accessRefused(a.Node.Run(r.loadCommand())); err != nil {
		if errors.Is(err, errAccessChangeRefused) {
			return fmt.Errorf("the access service was restarted during the change or another run took the host. Nothing was committed. Run the command again: %w", err)
		}
		return fmt.Errorf("loading the new access table failed: %w", err)
	}
	for _, family := range a.Families {
		if err := accessRefused(a.Node.Run(r.locked("true"))); err != nil {
			return err
		}
		observed, err := a.Node.ObservedAddress(family)
		if err != nil {
			return fmt.Errorf("new IPv%s SSH connections failed with the new access table. The access table admits this workstation's address. Something else refuses the connection (an operator firewall or the provider's firewall), or your network uses more than one outgoing address. List the whole range with 'kip cluster access allow' first: %w", family, err)
		}
		addr, err := netip.ParseAddr(observed)
		if err != nil || !admits(a.Intent.Admin, addr) {
			return fmt.Errorf("the host saw this workstation as %s over IPv%s, which the new access table does not admit", observed, family)
		}
	}
	for _, family := range a.ReadyzOn {
		if err := accessRefused(a.Node.Run(r.locked("true"))); err != nil {
			return err
		}
		if err := a.Readyz(family); err != nil {
			return fmt.Errorf("the Kubernetes API did not answer over IPv%s with the new access table: %w", family, err)
		}
	}
	return nil
}

// rollBackNow requests the restore at once on the unfenced runner and
// returns the recorded outcome and an error describing it.
func (r accessRun) rollBackNow(client CommandRunner) (string, error) {
	out, err := client.Run(r.rollBackNowCommand())
	switch strings.TrimSpace(out) {
	case AccessRestored:
		return AccessRestored, errors.New("kip restored the previous access table")
	case AccessClosed:
		return AccessClosed, errors.New("kip could not load the previous operator list, so the host now refuses every operator address and still admits the cluster. Run 'kip cluster access apply', or add your address from the provider console as 'kip cluster access show' describes")
	case AccessRemoved:
		return AccessRemoved, errors.New("kip removed the new access table. The host is open as it was before")
	case AccessFailed:
		return AccessFailed, errors.New("kip could not load any access table, so the new list stays live on the host. It admits only the operator prefixes you asked for. Run 'kip cluster access show'")
	}
	if err == nil {
		err = errors.New("the restore recorded no outcome")
	}
	return AccessFailed, fmt.Errorf("kip could not confirm that the previous access table was restored (%v). If the rollback is still armed, the host restores it about %d seconds after kip's last step. Run 'kip cluster access show'", err, accessRollbackSeconds)
}

// nftablesConflict is the refusal for a host where nftables.service is
// enabled or active.
const nftablesConflict = "nftables.service is enabled or running on this host, and its configuration starts with 'flush ruleset', which also removes the firewall and the access table. Disable it with 'systemctl disable --now nftables'. Kipper loads its table from kipper-access.service"

// prepareAccessHost installs nft when it is missing and refuses while
// nftables.service is enabled or active.
func prepareAccessHost(client CommandRunner, out io.Writer) error {
	host, err := DetectHostOS(client)
	if err != nil {
		return err
	}
	install := host.InstallPackagesCommand(true, "nftables")
	if install == "" {
		return fmt.Errorf("installing nftables: kip cannot install packages on %s", host.ID)
	}
	if err := ensureInstalled(client, "nft", "nftables", install, out); err != nil {
		return err
	}
	state, err := client.Run("systemctl is-enabled nftables.service 2>/dev/null; systemctl is-active nftables.service 2>/dev/null; true")
	if err != nil {
		return fmt.Errorf("checking nftables.service: %w", err)
	}
	for _, s := range strings.Fields(state) {
		if s == "enabled" || s == "active" || s == "activating" {
			return errors.New(nftablesConflict)
		}
	}
	return nil
}

// AccessListenerPorts returns every port SSH listens on: sshd's configured
// ports and the stream ports of ssh.socket where it exists, sorted without
// duplicates. Invalid ports and ports 80 and 443 return an error.
func AccessListenerPorts(client CommandRunner) ([]int, error) {
	sshd, err := readSSHPorts(client)
	if err != nil {
		return nil, err
	}
	socket, err := client.Run("systemctl show -p Listen ssh.socket 2>/dev/null || true")
	if err != nil {
		return nil, fmt.Errorf("reading the ports of ssh.socket: %w", err)
	}
	return accessListenerPorts(sshd, socket)
}

// AccessPodNetwork returns the IPv4 pod network after checking the server
// pod ranges and Kubernetes Service address against the configured networks.
// Read failures and inconsistent or unsupported networks return an error.
func AccessPodNetwork(server CommandRunner) (netip.Prefix, error) {
	nets, err := clusterNetworks(server)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%w (the access list supports IPv4 cluster networks. Operator prefixes may be IPv6)", err)
	}
	return nets["pod"], nil
}

// AccessIntentFor builds one node's intent from the operator prefixes, the
// node's listener ports, the pod network and the node records. exclude is
// the machine ID of a node being removed, or empty. It rejects missing or
// invalid peer addresses and requires a server peer address for workers.
func AccessIntentFor(generation int, admin []netip.Prefix, ports []int, pod netip.Prefix, records []NodeRecord, self, exclude string) (AccessIntent, error) {
	peers, err := accessPeers(records, self, exclude)
	if err != nil {
		return AccessIntent{}, err
	}
	in := AccessIntent{Generation: generation, Admin: admin, Ports: ports, Cluster4: []netip.Prefix{pod}}
	for _, p := range peers {
		in.Cluster4 = append(in.Cluster4, netip.PrefixFrom(p, 32))
	}
	for _, r := range records {
		if r.MachineID == self && r.Role == NodeRoleServer {
			return in, nil
		}
	}
	server, ok := ServerRecord(records)
	if !ok || server.PeerAddress == "" {
		return AccessIntent{}, errors.New("the server's record has no peer address, so kip cannot admit it on this worker's SSH ports")
	}
	in.Server4, err = netip.ParseAddr(server.PeerAddress)
	if err != nil {
		return AccessIntent{}, fmt.Errorf("the server's peer address %q is invalid", server.PeerAddress)
	}
	return in, nil
}

// JoinerAddresses returns a joining host's IPv4 interface addresses
// followed by external when it parses as an IP address, for AddJoiningAddresses.
// An invalid external address is skipped. Interface lookup errors are returned.
func JoinerAddresses(joiner CommandRunner, external string) ([]netip.Addr, error) {
	addrs, err := hostAddresses(joiner)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range addrs {
		out = append(out, a.prefix.Addr())
	}
	if a, err := netip.ParseAddr(external); err == nil {
		out = append(out, a)
	}
	return out, nil
}

// AddJoiningAddresses admits a joining host's addresses on 6443 in the
// server's live table until the next load. It skips IPv6 addresses and duplicates.
// An empty IPv4 list is a no-op. Changes use the access lock and are not persisted.
func AddJoiningAddresses(server CommandRunner, addrs []netip.Addr) error {
	seen := map[netip.Addr]bool{}
	var parts []string
	for _, a := range addrs {
		if !a.Is4() || seen[a] {
			continue
		}
		seen[a] = true
		parts = append(parts, a.String())
	}
	if len(parts) == 0 {
		return nil
	}
	cmd := fmt.Sprintf("exec 9>>%s && flock -w 120 9 || exit 1; %snft add element %s joining4 { %s }", accessLock, accessTimeout, accessTable, strings.Join(parts, ", "))
	if _, err := server.Run(cmd); err != nil {
		return fmt.Errorf("admitting the joining host on the server's API port: %w", err)
	}
	return nil
}

// removeAccessCommand builds the per-host removal routine. Every step
// tolerates absence, and the lock file stays. A host without /etc/kipper
// is handled without creating a lock directory. Without nft it
// refuses while guard files remain, because it cannot see the kernel table.
func removeAccessCommand(root string) string {
	d := root + accessDir
	return fmt.Sprintf(`export LC_ALL=C LANGUAGE=C
if [ -d %[9]s ]; then exec 9>>%[1]s && flock -w 120 9 || exit 1; fi
if ! command -v nft >/dev/null 2>&1 && { [ -e %[4]s ] || [ -e %[8]s ]; }; then
  echo 'nft is not installed, so kip cannot confirm that the operator access table is gone. Install nftables and run the command again' >&2; exit 1
fi
for u in $(systemctl list-units --all --plain --no-legend '%[2]s-*' '%[3]s-*' 2>/dev/null | awk '{print $1}'); do systemctl stop "$u" >/dev/null 2>&1; done
rm -f %[4]s/run
systemctl disable %[5]s >/dev/null 2>&1
systemctl stop %[5]s >/dev/null 2>&1
if command -v nft >/dev/null 2>&1; then
  if ! out=$(%[7]snft list table %[6]s 2>&1 >/dev/null); then
    case "$out" in *"No such file or directory"*) ;; *) printf '%%s\n' "$out" >&2; exit 1 ;; esac
  else
    %[7]snft delete table %[6]s || exit 1
  fi
fi
rm -f %[8]s && systemctl daemon-reload || exit 1
systemctl reset-failed %[5]s >/dev/null 2>&1
rm -rf %[4]s`, root+accessLock, accessRollbackUnit, accessLoadUnit, d, accessUnitName, accessTable, accessTimeout, root+accessUnitPath, root+"/etc/kipper")
}

// RemoveAccessGuard removes the access table, its unit and its files from a
// host. The host is open on SSH and 6443 afterwards, as far as the guard is
// concerned.
func RemoveAccessGuard(client CommandRunner) error {
	if _, err := client.Run(removeAccessCommand("")); err != nil {
		return fmt.Errorf("removing the operator access table: %w", err)
	}
	return nil
}

// LeftoverAccessWarning returns a warning when the host holds an access
// table but its cluster has no access record, as on a host reused without
// an uninstall, or "" otherwise. A check that cannot run returns "".
func LeftoverAccessWarning(client CommandRunner) string {
	st, err := ReadAccessGuardState(client)
	if err != nil || !st.HasTable() {
		return ""
	}
	if _, present, err := ReadAccessRecord(client); err != nil || present {
		return ""
	}
	return fmt.Sprintf("this host still restricts SSH and 6443 to %s from an earlier operator access list. Run 'kip cluster access reset --yes' to open them", plural(st.Sources(), "one source", fmt.Sprintf("%d sources", st.Sources())))
}

// AccessHostState is what one host reports about its access guard.
type AccessHostState struct {
	Table, Operators string
	Closed, Failed   bool
	UnitEnabled      string
	UnitActive       string
	Live             bool
	NftablesService  bool
}

// Generation is the committed table's generation, or false without one.
func (s AccessHostState) Generation() (int, bool) { return AccessHeaderGeneration(s.Table) }

// HasTable reports whether the host holds a committed table.
func (s AccessHostState) HasTable() bool { return s.Table != "" }

// Sources counts the committed operator prefixes.
func (s AccessHostState) Sources() int {
	return len(fileElements(s.Operators, "admin4")) + len(fileElements(s.Operators, "admin6"))
}

// Header returns the highest valid generation from either committed file
// for stale-record and orphaned-table checks.
func (s AccessHostState) Header(name string) AccessNodeHeader {
	h := AccessNodeHeader{Name: name, Present: s.HasTable(), Sources: s.Sources()}
	if g, ok := s.Generation(); ok {
		h.Generation = g
	}
	if g, ok := AccessHeaderGeneration(s.Operators); ok && g > h.Generation {
		h.Generation, h.Present = g, true
	}
	return h
}

// Problem returns the first problem visible in the saved files, failure markers,
// service failure state and live table presence, or "" if none is found.
// Use AccessDrift to compare live contents with the committed files.
func (s AccessHostState) Problem() string {
	switch {
	case s.Failed:
		return "last change could not be restored. Run kip cluster access show"
	case !s.HasTable():
		return "access table missing. Run kip cluster access apply"
	case s.UnitActive == "failed":
		return "access service failed at boot. Run kip cluster access apply"
	case !s.Live:
		return "access table missing from the kernel. Run kip cluster access apply"
	case s.Closed:
		return "booted closed and refuses every operator address. Admit yours from the provider console as 'kip cluster access show' describes, then run kip cluster access apply"
	}
	return ""
}

const accessStateSeparator = "----kip-access----"

// accessStateCommand prints the committed files and the unit and table
// state, separated by accessStateSeparator.
func accessStateCommand(root string) string {
	d := root + accessDir
	return fmt.Sprintf(`cat %[1]s/table.nft 2>/dev/null; echo %[2]s
cat %[1]s/operators.nft 2>/dev/null; echo %[2]s
[ -e %[1]s/closed ] && echo closed
[ -e %[1]s/failed ] && echo failed
echo "enabled $(systemctl is-enabled %[3]s 2>/dev/null)"
echo "active $(systemctl is-active %[3]s 2>/dev/null)"
command -v nft >/dev/null 2>&1 && nft list table %[4]s >/dev/null 2>&1 && echo live
echo "nftables $(systemctl is-enabled nftables.service 2>/dev/null) $(systemctl is-active nftables.service 2>/dev/null)"
true`, d, accessStateSeparator, accessUnitName, accessTable)
}

// ReadAccessGuardState reads a host's access guard without changing it.
func ReadAccessGuardState(client CommandRunner) (AccessHostState, error) {
	out, err := client.Run(accessStateCommand(""))
	if err != nil {
		return AccessHostState{}, fmt.Errorf("reading the operator access table: %w", err)
	}
	return parseAccessState(out)
}

func parseAccessState(out string) (AccessHostState, error) {
	parts := strings.SplitN(out, accessStateSeparator+"\n", 3)
	if len(parts) != 3 {
		return AccessHostState{}, errors.New("reading the operator access table: unexpected output")
	}
	s := AccessHostState{Table: parts[0], Operators: parts[1]}
	for _, line := range strings.Split(parts[2], "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "closed":
			s.Closed = true
		case "failed":
			s.Failed = true
		case "live":
			s.Live = true
		case "enabled":
			if len(f) > 1 {
				s.UnitEnabled = f[1]
			}
		case "active":
			if len(f) > 1 {
				s.UnitActive = f[1]
			}
		case "nftables":
			for _, v := range f[1:] {
				if v == "enabled" || v == "active" || v == "activating" {
					s.NftablesService = true
				}
			}
		}
	}
	return s, nil
}

var fileElementsPattern = regexp.MustCompile(`(?m)^add element inet kipper_access (\w+) \{ ([^}]*) \}$`)
var setElementsPattern = regexp.MustCompile(`(?m)^\s*set (\w+) \{[^}]*elements = \{ ([^}]*) \} \}$`)

// fileElements returns the prefixes a committed file lists for set name.
func fileElements(file, set string) []netip.Prefix {
	var out []netip.Prefix
	for _, re := range []*regexp.Regexp{fileElementsPattern, setElementsPattern} {
		for _, m := range re.FindAllStringSubmatch(file, -1) {
			if m[1] != set {
				continue
			}
			for _, e := range strings.Split(m[2], ",") {
				e = strings.TrimSpace(e)
				if p, err := netip.ParsePrefix(e); err == nil {
					out = append(out, p)
				} else if a, err := netip.ParseAddr(e); err == nil {
					out = append(out, netip.PrefixFrom(a, a.BitLen()))
				}
			}
		}
	}
	return out
}

// AccessFilePrefixes returns the operator prefixes a committed
// operators.nft lists, IPv4 first.
func AccessFilePrefixes(operators string) []netip.Prefix {
	return append(fileElements(operators, "admin4"), fileElements(operators, "admin6")...)
}

var accessSets = []string{"admin4", "admin6", "cluster4", "joining4", "server4"}

// AccessLive is what the kernel holds for a host's access table.
// CommittedCheck is nft's complaint about the committed files, or empty
// when the check emits no output, including when table.nft is absent.
type AccessLive struct {
	Sets           map[string][]netip.Prefix
	SetErrors      map[string]string
	Chain          accessChainShape
	ChainError     string
	CommittedCheck string
}

const accessLiveMark = "@@kip-access "

// accessLiveCommand lists the five sets and the input chain as JSON and
// checks the committed pair with the installed nft, without loading it.
func accessLiveCommand(root string) string {
	d := root + accessDir
	var b strings.Builder
	for _, set := range accessSets {
		fmt.Fprintf(&b, "echo '%sset %s'; nft -j list set %s %s 2>&1; echo\n", accessLiveMark, set, accessTable, set)
	}
	fmt.Fprintf(&b, "echo '%schain'; nft -j list chain %s input 2>&1; echo\n", accessLiveMark, accessTable)
	fmt.Fprintf(&b, "echo '%scheck'; if [ -e %[2]s/table.nft ]; then if [ -s %[2]s/operators.nft ]; then printf 'include \"%%s\"\\ninclude \"%%s\"\\n' %[2]s/table.nft %[2]s/operators.nft; else printf 'include \"%%s\"\\n' %[2]s/table.nft; fi | %[3]snft -c -f - 2>&1; fi\ntrue", accessLiveMark, d, accessTimeout)
	return b.String()
}

// ReadAccessLive reads the live sets and chain and checks the committed
// files with the installed nft without loading them. Set, chain and syntax
// failures are stored in the result. Command or output-format failures return
// an error.
func ReadAccessLive(client CommandRunner) (AccessLive, error) {
	out, err := client.Run(accessLiveCommand(""))
	if err != nil {
		return AccessLive{}, fmt.Errorf("reading the live access table: %w", err)
	}
	return parseAccessLive(out)
}

func parseAccessLive(out string) (AccessLive, error) {
	live := AccessLive{Sets: map[string][]netip.Prefix{}, SetErrors: map[string]string{}}
	sections := strings.Split(out, accessLiveMark)
	if len(sections) < 2 {
		return AccessLive{}, errors.New("reading the live access table: unexpected output")
	}
	for _, section := range sections[1:] {
		head, body, _ := strings.Cut(section, "\n")
		body = strings.TrimSpace(body)
		switch {
		case strings.HasPrefix(head, "set "):
			name := strings.TrimPrefix(head, "set ")
			ps, err := parseSetElements(body)
			if err != nil {
				live.SetErrors[name] = firstLine(body)
				continue
			}
			live.Sets[name] = ps
		case head == "chain":
			shape, err := parseChainShape(body)
			if err != nil {
				live.ChainError = firstLine(body)
				continue
			}
			live.Chain = shape
		case head == "check":
			live.CommittedCheck = body
		}
	}
	return live, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// AccessDrift compares live address sets and chain metadata with committed
// files, and checks failure markers, boot enablement and nft compatibility.
// An empty result means these checks passed. It checks the chain
// hook, priority, policy and rule count, rather than individual rule expressions.
func AccessDrift(st AccessHostState, live AccessLive) []string {
	if !st.HasTable() {
		return []string{"no access table is committed on this host"}
	}
	if !st.Live {
		return []string{"the access table is missing from the kernel. 'systemctl restart kipper-access' or 'kip cluster access apply' restores it"}
	}
	var drift []string
	if st.Failed {
		drift = append(drift, "the last change could not be restored, so the host may hold a candidate list")
	}
	if st.Closed {
		drift = append(drift, "booted closed (operators refused): the committed operator file did not load. Admit your address from the provider console, then 'kip cluster access apply' restores it")
	}
	want := map[string][]netip.Prefix{
		"cluster4": fileElements(st.Table, "cluster4"),
		"server4":  fileElements(st.Table, "server4"),
	}
	if !st.Closed {
		want["admin4"] = fileElements(st.Operators, "admin4")
		want["admin6"] = fileElements(st.Operators, "admin6")
	}
	for _, set := range []string{"admin4", "admin6", "cluster4", "server4"} {
		expected, compared := want[set]
		if !compared {
			continue
		}
		if msg, bad := live.SetErrors[set]; bad {
			drift = append(drift, fmt.Sprintf("set %s could not be read: %s", set, msg))
			continue
		}
		if !sameAddressSet(live.Sets[set], expected) {
			drift = append(drift, fmt.Sprintf("live %s holds { %s } but the committed file lists { %s }", set, prefixList(collapse(live.Sets[set])), prefixList(collapse(expected))))
		}
	}
	if n := len(live.Sets["joining4"]); n > 0 {
		drift = append(drift, fmt.Sprintf("live joining4 holds %d addresses from a join that did not finish. 'kip cluster access apply' or a reboot clears them (%s)", n, prefixList(live.Sets["joining4"])))
	}
	switch {
	case live.ChainError != "":
		drift = append(drift, "the input chain could not be read: "+live.ChainError)
	case !live.Chain.matches():
		drift = append(drift, fmt.Sprintf("the input chain has hook %s, priority %d, policy %s and %d rules. kip expects input, -10, accept and %d", live.Chain.Hook, live.Chain.Priority, live.Chain.Policy, live.Chain.Rules, accessChainRules))
	}
	if live.CommittedCheck != "" {
		drift = append(drift, "the installed nft rejects the committed files, so the next boot would not load them: "+firstLine(live.CommittedCheck))
	}
	if st.UnitEnabled != "enabled" {
		drift = append(drift, "kipper-access.service is not enabled, so the next boot starts without the table")
	}
	if st.NftablesService {
		drift = append(drift, nftablesConflict)
	}
	return drift
}

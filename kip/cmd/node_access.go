package cmd

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"time"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

// nodeAccess carries the operator access list through kip node add and
// node remove. It changes hosts only when the cluster has an access record.
type nodeAccess struct {
	c          *accessCluster
	action     string
	on         bool
	record     installer.AccessRecord
	admin      []netip.Prefix
	generation int
	pod        netip.Prefix
	baselines  map[string]accessBaseline
	apiOn      []string
	skip       map[string]bool
	next       installer.AccessRecord
	begun      bool
	written    bool
}

// newNodeAccessCluster connects the access verbs' defaults and this
// workstation's device to a node command's server.
func newNodeAccessCluster(out io.Writer, server installer.FirewallRunner, cluster *config.Cluster, keyFlag string) (*accessCluster, error) {
	c := &accessCluster{out: out, server: server, families: hostFamilies}
	c.useAPI(cluster)
	if err := c.useDevice(cluster, keyFlag); err != nil {
		return nil, err
	}
	return c, nil
}

// prepareNodeAccess reads the access record and the committed tables of
// nodes before a node command changes anything. inspect adds the headers
// of a host the command touches later, such as a recorded worker that
// joins again, to the stale, orphan and generation rules. Without a record
// it refuses only a leftover table. With one it refuses a stale record and
// a workstation address outside the list. A node without a table is
// refused when requireTables is set, and otherwise skipped with a note.
// With requireTables, any problem reported by AccessHostState.Problem also
// blocks the command, including a table missing from the kernel, because a
// join adds the joining host's addresses to the live table.
func prepareNodeAccess(c *accessCluster, nodes []accessNode, inspect []installer.AccessNodeHeader, action string, requireTables bool) (*nodeAccess, []string, error) {
	c.defaults()
	a := &nodeAccess{c: c, action: action, baselines: map[string]accessBaseline{}, skip: map[string]bool{}}
	headers := append([]installer.AccessNodeHeader(nil), inspect...)
	var missing []accessNode
	var broken error
	for _, n := range nodes {
		st, err := c.state(n.node)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", n.rec.Name, err)
		}
		headers = append(headers, st.Header(n.rec.Name))
		if !st.HasTable() {
			missing = append(missing, n)
		} else if p := st.Problem(); p != "" && broken == nil {
			broken = fmt.Errorf("%s: %s", n.rec.Name, p)
		}
	}
	rec, present, err := installer.ReadAccessRecord(c.server)
	if err != nil {
		return nil, nil, err
	}
	if !present {
		return a, nil, installer.OrphanedAccessTable(headers)
	}
	if err := installer.StaleAccessRecord(rec.Generation, headers); err != nil {
		return nil, nil, err
	}
	if requireTables && broken != nil {
		return nil, nil, broken
	}
	// A joining node has no revocation list of its own yet, so it would admit
	// a device the other nodes revoked from the console.
	if requireTables {
		if err := c.refuseRevokedDevices(nodes, rec, action); err != nil {
			return nil, nil, err
		}
	}
	var notes []string
	for _, n := range missing {
		if requireTables && n.rec.State == installer.NodeStateJoining {
			return nil, nil, fmt.Errorf("%s (%s) has not finished joining and has no access table yet. Run 'kip node add %s' again to finish it, or remove it with 'kip node remove', before adding another node", n.rec.Name, n.rec.Host, n.rec.Host)
		}
		if requireTables {
			return nil, nil, fmt.Errorf("%s has no access table although the access record restricts SSH and the API. Run 'kip cluster access apply' first", n.rec.Name)
		}
		a.skip[n.rec.MachineID] = true
		notes = append(notes, fmt.Sprintf("%s has no access table. Run 'kip cluster access apply'", n.rec.Name))
	}
	if a.admin, err = rec.Prefixes(); err != nil {
		return nil, nil, err
	}
	for _, n := range nodes {
		if a.skip[n.rec.MachineID] {
			continue
		}
		b, err := c.baseline(n)
		if err != nil {
			return nil, nil, err
		}
		a.baselines[n.rec.MachineID] = b
	}
	if err := c.lockoutGuard(a.admin, a.baselines, nodes, rec); err != nil {
		return nil, nil, fmt.Errorf("%w. This workstation's address is not in the operator access list. Run 'kip cluster access allow --here' first", err)
	}
	for _, n := range nodes {
		if n.rec.Role == installer.NodeRoleServer && !a.skip[n.rec.MachineID] {
			if a.apiOn, err = c.apiBaseline(a.baselines[n.rec.MachineID]); err != nil {
				return nil, nil, err
			}
		}
	}
	if a.pod, err = c.pod(); err != nil {
		return nil, nil, err
	}
	a.on, a.record = true, rec
	a.generation = installer.NextAccessGeneration(rec.Generation, headers)
	return a, notes, nil
}

// load renders n's table from records and loads it, unless the cluster
// has no access list or n was skipped. It records the new generation if
// prepareJoiner has not already done so. exclude is the machine ID of a node
// being removed, or empty.
func (a *nodeAccess) load(n accessNode, records []installer.NodeRecord, exclude string) error {
	if a == nil || !a.on || a.skip[n.rec.MachineID] {
		return nil
	}
	b, ok := a.baselines[n.rec.MachineID]
	if !ok {
		var err error
		if b, err = a.c.baseline(n); err != nil {
			return err
		}
		if err := a.c.lockoutGuard(a.admin, map[string]accessBaseline{n.rec.MachineID: b}, []accessNode{n}, a.record); err != nil {
			return fmt.Errorf("%w. This workstation's address is not in the operator access list. Run 'kip cluster access allow --here' first", err)
		}
		a.baselines[n.rec.MachineID] = b
	}
	ports, err := a.c.ports(n.node)
	if err != nil {
		return fmt.Errorf("%s: %w", n.rec.Name, err)
	}
	in, err := installer.AccessIntentFor(a.generation, a.admin, ports, a.pod, records, n.rec.MachineID, exclude)
	if err != nil {
		return fmt.Errorf("%s: %w", n.rec.Name, err)
	}
	in.Devices = len(a.record.Devices) > 0
	a.begin(b)
	if err := a.recordOps(n, records, exclude, ports); err != nil {
		return err
	}
	outcome, err := a.c.applyOn(n, b, in, a.apiOn, a.next, ports)
	if err != nil {
		outcome = accessOutcome(n.rec.Name, outcome, err)
	}
	a.next.History[len(a.next.History)-1].Nodes[n.rec.Name] = outcome
	if werr := installer.WriteAccessRecord(a.c.server, a.next); werr != nil {
		say(a.c.out, "  ⚠  could not record the outcome on %s: %v\n", n.rec.Name, werr)
	}
	if err != nil {
		if strings.Contains(err.Error(), "does not admit") {
			return fmt.Errorf("%w. This workstation's address is not in the operator access list. Run 'kip cluster access allow --here' first", err)
		}
		return err
	}
	return nil
}

// prepareJoiner verifies access to n before it joins and saves the baseline
// for its later load. When operator state exists, it first records n's operator
// address and key, refreshes this workstation's device node list if present,
// and loads n's operator interface. This lets the check use the tunnel and
// rejects nodes that cannot support devices before they join.
// It does nothing when a is nil or the cluster has no access record.
func (a *nodeAccess) prepareJoiner(n accessNode, records []installer.NodeRecord) error {
	if a == nil || !a.on {
		return nil
	}
	if a.record.Ops != nil {
		ports, err := a.c.ports(n.node)
		if err != nil {
			return fmt.Errorf("%s: %w", n.rec.Name, err)
		}
		srv, _ := installer.ServerRecord(records)
		a.begin(a.baselines[srv.MachineID])
		if err := a.recordOps(n, records, "", ports); err != nil {
			return err
		}
		if err := a.c.applyOpsOn(n, a.next, ports); err != nil {
			return err
		}
	}
	b, err := a.c.baseline(n)
	tunnelFailed := a.record.Ops != nil && a.c.useTunnel && !b.tunnel
	if err != nil {
		if tunnelFailed {
			return fmt.Errorf("kip could not reach %s through device %s. Check that the provider's firewall lets UDP port %d reach it", n.rec.Name, a.c.device.name, a.record.Ops.Port)
		}
		return err
	}
	if err := a.c.lockoutGuard(a.admin, map[string]accessBaseline{n.rec.MachineID: b}, []accessNode{n}, a.record); err != nil {
		switch {
		case tunnelFailed:
			return fmt.Errorf("%w. kip could not reach %s through device %s either. Check that the provider's firewall lets UDP port %d reach it", err, n.rec.Name, a.c.device.name, a.record.Ops.Port)
		case a.record.Ops != nil && a.c.deviceActiveIn(a.record):
			return fmt.Errorf("%w. Run the command with '--via auto' so kip reaches %s through device %s, or list this workstation's address with 'kip cluster access allow --here'", err, n.rec.Name, a.c.device.name)
		}
		return fmt.Errorf("%w. This workstation's address is not in the operator access list. Run 'kip cluster access allow --here' first", err)
	}
	a.baselines[n.rec.MachineID] = b
	return nil
}

// tunnelled reports whether this workstation reached the node with machine
// ID id through its device tunnel during the baseline check.
func (a *nodeAccess) tunnelled(id string) bool {
	if a == nil {
		return false
	}
	return a.baselines[id].tunnel
}

// begin starts the next record from the current one with this change's
// history entry, once per command. b names the actor.
func (a *nodeAccess) begin(b accessBaseline) {
	if a.begun {
		return
	}
	a.next = a.record
	if a.record.Ops != nil {
		ops := *a.record.Ops
		ops.Nodes = append([]installer.AccessOpsNode(nil), a.record.Ops.Nodes...)
		a.next.Ops = &ops
	}
	a.next.Generation = a.generation
	a.next.History = append(a.next.History, installer.AccessChange{At: a.c.now().UTC().Format(time.RFC3339), By: a.c.actor(b), Action: a.action, Generation: a.generation, Nodes: map[string]string{}})
	a.begun = true
}

// recordOps updates the operator state for n, writes the record the first
// time and whenever the operator state changes, and refreshes this
// workstation's device node list, if present, with n's SSH ports.
func (a *nodeAccess) recordOps(n accessNode, records []installer.NodeRecord, exclude string, ports []int) error {
	opsChanged, err := a.updateOps(n, exclude)
	if err != nil {
		return err
	}
	if !a.written || opsChanged {
		if err := installer.WriteAccessRecord(a.c.server, a.next); err != nil {
			return err
		}
		a.written = true
	}
	a.c.records = records
	return a.c.refreshManifest(a.next, map[string][]int{n.rec.MachineID: ports})
}

// updateOps drops the removed node exclude from the operator state and gives
// n an operator address and key when the cluster has devices and n has
// none. It reports whether the record changed.
func (a *nodeAccess) updateOps(n accessNode, exclude string) (bool, error) {
	ops := a.next.Ops
	if ops == nil {
		return false, nil
	}
	changed := false
	if _, ok := ops.Node(exclude); exclude != "" && ok {
		var kept []installer.AccessOpsNode
		for _, on := range ops.Nodes {
			if on.MachineID != exclude {
				kept = append(kept, on)
			}
		}
		ops.Nodes, changed = kept, true
	}
	if _, ok := ops.Node(n.rec.MachineID); ok {
		return changed, nil
	}
	if err := a.c.addOpsNode(ops, n); err != nil {
		return false, err
	}
	return true, nil
}

// checkRemovalTargetAccess refuses to remove a worker whose committed
// access list is newer than the record, or that holds one while the record
// is gone, before the wipe destroys the evidence that the record lost a
// change.
func checkRemovalTargetAccess(server, target installer.CommandRunner, name string) error {
	rec, present, err := installer.ReadAccessRecord(server)
	if err != nil {
		return err
	}
	st, err := installer.ReadAccessGuardState(target)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	header := []installer.AccessNodeHeader{st.Header(name)}
	if !present {
		return installer.OrphanedAccessTable(header)
	}
	return installer.StaleAccessRecord(rec.Generation, header)
}

// reportAccessGuard prints the host's operator access state after kip node reboot.
func reportAccessGuard(out io.Writer, recordFrom, host installer.CommandRunner) {
	reportAccessGuards(out, recordFrom, []hostTarget{{runner: host}})
}

// reportAccessGuards reports each target's operator access state using the cluster's
// access record, read once from recordFrom. Targets without runners are reported
// as not checked. A record read failure skips all targets, while an invalid record
// is reported for each target whose host state can be read.
func reportAccessGuards(out io.Writer, recordFrom installer.CommandRunner, targets []hostTarget) {
	say(out, "  Operator access:\n")
	rec, present, err := installer.ReadAccessRecord(recordFrom)
	if err != nil && !errors.Is(err, installer.ErrInvalidAccessRecord) {
		say(out, "    ⚠  not checked (%v)\n\n", err)
		return
	}
	for _, t := range targets {
		if t.notChecked(out, false) {
			continue
		}
		reportHostAccessGuard(out, t.prefix, rec, present, err, t.runner)
	}
	say(out, "\n")
}

func reportHostAccessGuard(out io.Writer, prefix string, rec installer.AccessRecord, present bool, recErr error, host installer.CommandRunner) {
	st, stErr := installer.ReadAccessGuardState(host)
	mismatches := accessRecordMismatches(st, rec)
	switch {
	case stErr != nil:
		say(out, "    ⚠  %snot checked (%v)\n", prefix, stErr)
	case recErr != nil:
		say(out, "    ⚠  %s%v. Run 'kip cluster access show'\n", prefix, recErr)
	case !present && st.HasTable():
		say(out, "    ⚠  %sthe host restricts SSH and 6443 to %s although the cluster has no access record. Run 'kip cluster access reset'\n", prefix, plural(st.Sources(), "one source", fmt.Sprintf("%d sources", st.Sources())))
	case !present:
		say(out, "    ℹ  %sopen to every address ('kip cluster access allow --here' restricts SSH and the API)\n", prefix)
	case st.Problem() != "":
		say(out, "    ⚠  %s%s\n", prefix, st.Problem())
	case len(mismatches) > 0:
		for _, m := range mismatches {
			say(out, "    ⚠  %s%s. Run 'kip cluster access show'\n", prefix, m)
		}
	default:
		g, _ := st.Generation()
		what := plural(st.Sources(), "one source", fmt.Sprintf("%d sources", st.Sources()))
		if d := rec.ActiveDevices(); d > 0 {
			devices := plural(d, "one device", fmt.Sprintf("%d devices", d))
			if st.Sources() == 0 {
				what = devices
			} else {
				what += " and " + devices
			}
		}
		say(out, "    ✔  %srestricted to %s (generation %d)\n", prefix, what, g)
		if len(rec.Sources) == 1 && rec.ActiveDevices() == 0 {
			say(out, "    ⚠  %sthe list has one entry. Add a stable second prefix so a changed address cannot lock you out\n", prefix)
		}
	}
	if stErr == nil && recErr == nil {
		reportOpsInterface(out, prefix, rec, host)
	}
}

// reportOpsInterface warns about each difference between host's operator
// interface and rec, or a leftover interface without devices.
func reportOpsInterface(out io.Writer, prefix string, rec installer.AccessRecord, host installer.CommandRunner) {
	ops, err := installer.ReadOpsState(host)
	if err != nil {
		say(out, "    ⚠  %soperator interface not checked (%v)\n", prefix, err)
		return
	}
	id := ""
	if rec.Ops != nil {
		if id, err = installer.ReadMachineID(host); err != nil {
			say(out, "    ⚠  %soperator interface not checked (%v)\n", prefix, err)
			return
		}
	}
	for _, d := range installer.OpsDrift(ops, rec, id) {
		if !strings.Contains(d, "Run '") {
			d += ". Run 'kip cluster access show'"
		}
		say(out, "    ⚠  %s%s\n", prefix, d)
	}
}

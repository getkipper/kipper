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
	written    bool
}

// newNodeAccessCluster connects the access verbs' defaults to a node
// command's server.
func newNodeAccessCluster(out io.Writer, server installer.FirewallRunner, cluster *config.Cluster) *accessCluster {
	c := &accessCluster{out: out, server: server, families: hostFamilies}
	c.useAPI(cluster)
	return c
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
	if err := c.lockoutGuard(a.admin, a.baselines, nodes); err != nil {
		return nil, nil, fmt.Errorf("%w. This workstation's address is not in the operator access list. Run 'kip cluster access allow --here' first", err)
	}
	for _, n := range nodes {
		if n.rec.Role == installer.NodeRoleServer && !a.skip[n.rec.MachineID] {
			if a.apiOn, err = c.apiBaseline(a.baselines[n.rec.MachineID].families()); err != nil {
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
// has no access list or n was skipped. The first load records the new
// generation. exclude is the machine ID of a node being removed, or empty.
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
		if err := a.c.lockoutGuard(a.admin, map[string]accessBaseline{n.rec.MachineID: b}, []accessNode{n}); err != nil {
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
	if !a.written {
		a.next = a.record
		a.next.Generation = a.generation
		a.next.History = append(a.next.History, installer.AccessChange{At: a.c.now().UTC().Format(time.RFC3339), By: a.c.actor(b), Action: a.action, Generation: a.generation, Nodes: map[string]string{}})
		if err := installer.WriteAccessRecord(a.c.server, a.next); err != nil {
			return err
		}
		a.written = true
	}
	outcome, err := a.c.applyOn(n, b, in, a.apiOn)
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
		say(out, "    ✔  %srestricted to %s (generation %d)\n", prefix, plural(st.Sources(), "one source", fmt.Sprintf("%d sources", st.Sources())), g)
		if len(rec.Sources) == 1 {
			say(out, "    ⚠  %sthe list has one entry. Add a stable second prefix so a changed address cannot lock you out\n", prefix)
		}
	}
}

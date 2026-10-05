package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/capacity"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/kip/internal/deployer"
)

// upgradeDataChecks are the read-only checks 'kip upgrade --check' runs. Each
// prints its findings and returns how many need a decision.
var upgradeDataChecks = []func(context.Context, kubernetes.Interface, dynamic.Interface, io.Writer, string) (int, error){
	checkAutoscalingData,
	checkRouteNames,
}

// runUpgradeChecks runs every data check the upgrade runs, without changing
// anything, and fails when a finding needs a decision. It reads through the
// Kubernetes API only, so it needs no SSH key.
func runUpgradeChecks(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, out io.Writer, version string) error {
	_, _ = fmt.Fprintf(out, "\n  Checking this cluster's data against the rules of kip %s...\n\n", version)
	decisions := 0
	for _, check := range upgradeDataChecks {
		n, err := check(ctx, clientset, dyn, out, version)
		if err != nil {
			return err
		}
		decisions += n
	}
	if decisions > 0 {
		return fmt.Errorf("%d finding(s) need a decision before 'kip upgrade' can run", decisions)
	}
	return nil
}

func checkAutoscalingData(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, out io.Writer, version string) (int, error) {
	findings, err := assessClusterAutoscaling(ctx, clientset, dyn, version)
	if err != nil {
		return 0, err
	}
	if len(findings) == 0 {
		_, _ = fmt.Fprintf(out, "  ✔  Autoscaling settings: nothing for the upgrade to fix.\n")
		return 0, nil
	}
	printAutoscalingFindings(out, findings)
	return countDecisions(findings), nil
}

// appState is what the autoscaling repair reads about one app.
type appState struct {
	Namespace, Name string
	// Command is the app as kip commands address it, with its project and
	// environment flags when the namespace records them.
	Command string
	Policy  *capacity.Policy
	// Replicas is the stored spec.replicas, where an absent value counts as 1.
	Replicas int32
	Stopped  bool
	// Live is the Deployment's spec.replicas, where a nil value counts as 1.
	Live         int32
	LiveErr      error
	NoDeployment bool
}

func (s appState) command() string {
	if s.Command != "" {
		return s.Command
	}
	return s.Name
}

// appCapacity is the App fields the autoscaling repair shows and writes.
// Min is the effective minimum, so an absent one reads as 1.
type appCapacity struct {
	Replicas   int32
	Min, Max   int32
	Stopped    bool
	StopReason string
}

// autoscalingFinding is one row of the repair's decision table for one app.
type autoscalingFinding struct {
	Namespace, Name string
	Row             int
	Problem, Advice string
	// Decision means the upgrade changes nothing for this app and the
	// operator has to.
	Decision bool
	// StartsPods marks an autoscaled app at 0 that the new console-api starts
	// as soon as it runs, whatever the repair does.
	StartsPods    bool
	HasFix        bool
	Before, After appCapacity
}

// rule reports whether consent covers the rule rather than the exact count:
// the stored count becomes the running count at write time, within the
// bounds. The autoscaler can move the running count between consent and
// write, and every count within the bounds is a valid stored one.
func (f autoscalingFinding) rule() bool {
	return f.HasFix && (f.Row == 5 || f.Row == 6)
}

// covers reports whether consent to f also covers the newly proposed fix.
func (f autoscalingFinding) covers(now autoscalingFinding) bool {
	if !now.HasFix || f.Before != now.Before {
		return false
	}
	if f.rule() && now.rule() {
		agreed, proposed := f.After, now.After
		agreed.Replicas, proposed.Replicas = 0, 0
		return agreed == proposed
	}
	return f.After == now.After
}

// assessAutoscaling validates the policy before assessing count repairs, so
// invalid policies without bounds are caught too. A nil finding needs no action.
func assessAutoscaling(s appState, version string) *autoscalingFinding {
	p := s.Policy
	if p == nil {
		return nil
	}
	f := &autoscalingFinding{Namespace: s.Namespace, Name: s.Name}
	if err := capacity.Validate(p, nil); err != nil {
		f.Row, f.Decision = 2, true
		f.Problem = "its autoscaling settings are invalid: " + oneLine(err) + "."
		f.Advice = invalidPolicyAdvice(s)
		return f
	}
	lo, hi, ok := p.Bounds()
	if !ok {
		return nil
	}
	switch {
	case s.LiveErr != nil:
		f.Row, f.Decision = 1, true
		f.Problem = fmt.Sprintf("its Deployment could not be read (%v), so its running count is unknown.", s.LiveErr)
		f.Advice = "Run 'kip upgrade --check' again once the cluster answers."
		return f
	case s.NoDeployment && !s.Stopped:
		f.Row, f.Decision = 1, true
		f.Problem = "it is not stopped and has no Deployment, so its running count is unknown."
		f.Advice = fmt.Sprintf("Run 'kip upgrade --check' again in a minute. If the Deployment stays missing, stop the app with 'kip app stop %s' or delete it.", s.command())
		return f
	}

	within := func(n int32) bool { return n >= lo && n <= hi }
	f.Before = appCapacity{Replicas: s.Replicas, Min: lo, Max: hi, Stopped: s.Stopped}
	f.After = f.Before
	bounds := fmt.Sprintf("%d to %d", lo, hi)

	switch {
	case s.Stopped:
		// A stopped app's Deployment count says nothing about its restart
		// count, so use the stored count to plan its repair.
		if within(s.Replicas) {
			return nil
		}
		f.Row = 3
		f.Problem = fmt.Sprintf("it is stopped and stores replicas %d, outside its bounds of %s.", s.Replicas, bounds)
		if p.Enabled {
			f.After.Replicas, _ = p.IntoBounds(s.Replicas)
		} else {
			f.After.Min, f.After.Max = min(lo, s.Replicas), max(hi, s.Replicas)
			f.Problem += " Autoscaling is off, so the bounds widen to the stored count it restarts at."
		}
	case s.Live == 0 && s.Replicas == 0 && !p.Enabled:
		f.Row = 4
		f.After.Replicas = lo
		f.After.Stopped = true
		f.After.StopReason = "scaled to 0 before Kipper " + version
		f.Problem = fmt.Sprintf("it runs no pods and stores replicas 0 with autoscaling off, which is how an app was taken to 0 before stops existed. "+
			"The upgrade records a stop instead, so its route serves the stopped page and listings show it as Stopped. "+
			"'kip app start %s' then starts it at %d.", s.command(), lo)
	case s.Live == 0 && p.Enabled:
		f.Row, f.StartsPods = 5, true
		f.Problem = fmt.Sprintf("autoscaling is on and its Deployment runs no pods, so the new console-api starts %d pod(s) as soon as it runs.", lo)
		f.Advice = fmt.Sprintf("To keep it at 0, run 'kip app stop %s' before upgrading. A cluster older than v0.23.0 has no stop, "+
			"so there the upgrade starts it at %d pod(s); run 'kip app stop %s' once the upgrade finishes.", s.command(), lo, s.command())
		if within(s.Replicas) {
			return f
		}
		f.After.Replicas = lo
		f.Problem += fmt.Sprintf(" It stores replicas %d, outside its bounds of %s, so the stored count becomes the running count when the fix is written.", s.Replicas, bounds)
	case p.Enabled && within(s.Live) && !within(s.Replicas):
		f.Row = 6
		f.After.Replicas = s.Live
		f.Problem = fmt.Sprintf("autoscaling is on and it stores replicas %d, outside its bounds of %s, so the stored count becomes the running count when the fix is written.", s.Replicas, bounds)
	case p.Enabled && !within(s.Live):
		f.Row, f.Decision = 7, true
		f.Problem = fmt.Sprintf("autoscaling is on but its Deployment is set to %d, outside its bounds of %s, so a stale or foreign autoscaler is setting the count.", s.Live, bounds)
		f.Advice = fmt.Sprintf("See what is scaling it with 'kip app autoscale %s --status'. 'kip app autoscale %s --off' keeps the running count, moved into the bounds.", s.command(), s.command())
	case !p.Enabled && s.Live != s.Replicas:
		f.Row, f.Decision = 8, true
		f.Problem = fmt.Sprintf("autoscaling is off, but its Deployment is set to %d while it stores %d.", s.Live, s.Replicas)
		if within(s.Replicas) {
			f.Problem += fmt.Sprintf(" Any reconcile restores %d.", s.Replicas)
		} else {
			f.Problem += fmt.Sprintf(" The old console-api restores %d, and the new one keeps %d and reports ReplicasOutsideBounds.", s.Replicas, s.Live)
		}
		f.Advice = fmt.Sprintf("Set the count you want with 'kip app scale %s --replicas N', with N from %s.", s.command(), bounds)
	case !p.Enabled && !within(s.Replicas):
		f.Row = 9
		f.After.Min, f.After.Max = min(lo, s.Replicas), max(hi, s.Replicas)
		f.Problem = fmt.Sprintf("autoscaling is off and it runs its stored count of %d, outside its bounds of %s. "+
			"The bounds widen to include it, because moving the count into them would change what runs.", s.Replicas, bounds)
	default:
		return nil
	}
	if f.After == f.Before {
		return f
	}

	result := *p
	result.MinReplicas, result.MaxReplicas = &f.After.Min, &f.After.Max
	if err := capacity.Validate(&result, &f.After.Replicas); err != nil {
		f.Decision, f.After = true, f.Before
		f.Problem += " No change the upgrade could make satisfies the rules: " + oneLine(err) + "."
		f.Advice = fmt.Sprintf("Set a count within the bounds with 'kip app scale %s --replicas N', or remove the bounds with 'kip app autoscale %s --remove'.", s.command(), s.command())
		return f
	}
	f.HasFix = true
	return f
}

// invalidPolicyAdvice names the command that makes an invalid block valid.
// A disabled block is never fixed with 'kip app autoscale' flags, because
// those switch autoscaling on.
func invalidPolicyAdvice(s appState) string {
	p := s.Policy
	if !p.Enabled {
		return disabledBoundsAdvice(s.command())
	}
	var flags []string
	lo := int32(1)
	if p.MinReplicas != nil {
		if *p.MinReplicas < 1 {
			flags = append(flags, "--min 1")
		} else {
			lo = *p.MinReplicas
		}
	}
	switch {
	case p.MaxReplicas == nil || *p.MaxReplicas < 1:
		flags = append(flags, fmt.Sprintf("--max %d", max(lo, 5)))
	case lo > *p.MaxReplicas:
		flags = append(flags, fmt.Sprintf("--max %d", lo))
	}
	// The command keeps a target it is not given, so a negative one is cleared
	// with 0 and at least one positive target has to remain.
	switch {
	case !positiveTarget(p.CPUTarget) && !positiveTarget(p.MemoryTarget):
		flags = append(flags, "--cpu 70")
	case negativeTarget(p.CPUTarget):
		flags = append(flags, "--cpu 0")
	}
	if negativeTarget(p.MemoryTarget) {
		flags = append(flags, "--memory 0")
	}
	return fmt.Sprintf("Fix it with 'kip app autoscale %s %s'.", s.command(), strings.Join(flags, " "))
}

// disabledBoundsAdvice names the ways to fix the bounds of a disabled block
// that leave autoscaling off.
func disabledBoundsAdvice(command string) string {
	return fmt.Sprintf("Remove the bounds with 'kip app autoscale %s --remove', or correct them in kipper.yaml and run kip apply.", command)
}

func positiveTarget(v *int32) bool { return v != nil && *v > 0 }

func negativeTarget(v *int32) bool { return v != nil && *v < 0 }

func oneLine(err error) string {
	return strings.ReplaceAll(err.Error(), "\n", "; ")
}

func countDecisions(findings []autoscalingFinding) int {
	n := 0
	for _, f := range findings {
		if f.Decision {
			n++
		}
	}
	return n
}

// assessClusterAutoscaling reads every app with an autoscaling block and its
// Deployment, and returns the findings in namespace and name order.
func assessClusterAutoscaling(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, version string) ([]autoscalingFinding, error) {
	list, err := dyn.Resource(deployer.AppGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing apps to check their autoscaling settings: %w", err)
	}
	commands := map[string]string{}
	var findings []autoscalingFinding
	for i := range list.Items {
		app := &list.Items[i]
		if _, found, _ := unstructured.NestedMap(app.Object, "spec", "autoscale"); !found {
			continue
		}
		if f := assessAutoscaling(readAppState(ctx, clientset, app, commandFor(ctx, clientset, app, commands)), version); f != nil {
			findings = append(findings, *f)
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Namespace != findings[j].Namespace {
			return findings[i].Namespace < findings[j].Namespace
		}
		return findings[i].Name < findings[j].Name
	})
	return findings, nil
}

// commandFor adds project and environment flags from namespace labels,
// caching each namespace lookup.
func commandFor(ctx context.Context, clientset kubernetes.Interface, app *unstructured.Unstructured, cache map[string]string) string {
	ns := app.GetNamespace()
	flags, seen := cache[ns]
	if !seen {
		if obj, err := clientset.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err == nil {
			project, environment := obj.Labels[labels.Project], obj.Labels[labels.Environment]
			if project != "" && environment != "" {
				flags = fmt.Sprintf(" --project %s --environment %s", project, environment)
			}
		}
		cache[ns] = flags
	}
	return app.GetName() + flags
}

func readAppState(ctx context.Context, clientset kubernetes.Interface, app *unstructured.Unstructured, command string) appState {
	spec, _, _ := unstructured.NestedMap(app.Object, "spec")
	s := appState{
		Namespace: app.GetNamespace(),
		Name:      app.GetName(),
		Command:   command,
		Policy:    deployer.SpecPolicy(spec),
		Replicas:  1,
	}
	if v, found, _ := unstructured.NestedInt64(app.Object, "spec", "replicas"); found {
		s.Replicas = int32(v) //nolint:gosec // replica counts are bounded by K8s validation
	}
	_, s.Stopped, _ = unstructured.NestedMap(app.Object, "spec", "stopped")
	dep, err := clientset.AppsV1().Deployments(s.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		s.NoDeployment = true
	case err != nil:
		s.LiveErr = err
	default:
		s.Live = 1
		if dep.Spec.Replicas != nil {
			s.Live = *dep.Spec.Replicas
		}
	}
	return s
}

func printAutoscalingFindings(out io.Writer, findings []autoscalingFinding) {
	var fixes, starts, decisions []autoscalingFinding
	for _, f := range findings {
		switch {
		case f.Decision:
			decisions = append(decisions, f)
		case f.StartsPods:
			starts = append(starts, f)
		default:
			fixes = append(fixes, f)
		}
	}
	if len(fixes) > 0 {
		_, _ = fmt.Fprintf(out, "  !   The new rules refuse these apps' autoscaling settings. Once the new console-api\n"+
			"      is running, kip upgrade changes them as shown, and nothing running changes:\n")
		printFindingList(out, fixes)
	}
	if len(starts) > 0 {
		_, _ = fmt.Fprintf(out, "  !   These autoscaled apps run no pods, and the new console-api starts them at their\n"+
			"      minimum as soon as it runs:\n")
		printFindingList(out, starts)
	}
	if len(decisions) > 0 {
		_, _ = fmt.Fprintf(out, "  ✗   These apps need a decision, and kip upgrade does not change them:\n")
		printFindingList(out, decisions)
	}
}

func printFindingList(out io.Writer, findings []autoscalingFinding) {
	for _, f := range findings {
		_, _ = fmt.Fprintf(out, "      - %s/%s: %s\n", f.Namespace, f.Name, f.Problem)
		if f.Advice != "" {
			_, _ = fmt.Fprintf(out, "          %s\n", f.Advice)
		}
		if f.HasFix {
			for _, line := range fixLines(f) {
				_, _ = fmt.Fprintf(out, "          %s\n", line)
			}
		}
	}
}

// fixLines is the before and after of every field the fix writes.
func fixLines(f autoscalingFinding) []string {
	b, a := f.Before, f.After
	var lines []string
	if a.Replicas != b.Replicas {
		line := fmt.Sprintf("replicas: %d → %d", b.Replicas, a.Replicas)
		if f.rule() {
			line += " (the running count when the fix is written)"
		}
		lines = append(lines, line)
	}
	if a.Min != b.Min {
		lines = append(lines, fmt.Sprintf("minReplicas: %d → %d", b.Min, a.Min))
	}
	if a.Max != b.Max {
		lines = append(lines, fmt.Sprintf("maxReplicas: %d → %d", b.Max, a.Max))
	}
	if a.Stopped && !b.Stopped {
		lines = append(lines, fmt.Sprintf("stopped: no → yes (%q)", a.StopReason))
	}
	return lines
}

// autoscalingConsentOptions carries the flags and terminal state the consent
// depends on.
type autoscalingConsentOptions struct {
	// Repair is --repair-autoscaling, which answers yes.
	Repair bool
	// SkipDecisions allows the upgrade to proceed while leaving
	// apps that need a decision unrepaired.
	SkipDecisions bool
	IsTTY         bool
}

// autoscalingRepair is the fixes the operator agreed to before the upgrade
// changed anything.
type autoscalingRepair struct {
	consented []autoscalingFinding
}

// autoscalingRepairConsent runs the autoscaling check before the upgrade's
// first change and decides what the closing pass may write. It returns an
// error, with nothing changed, for a decision-needed app without
// --skip-autoscaling-check, for fixes on a run with no terminal and no
// --repair-autoscaling, and for declined fixes. A clean cluster asks nothing.
func autoscalingRepairConsent(
	ctx context.Context,
	clientset kubernetes.Interface,
	dyn dynamic.Interface,
	out io.Writer,
	version string,
	opts autoscalingConsentOptions,
	confirm func() (bool, error),
) (autoscalingRepair, error) {
	findings, err := assessClusterAutoscaling(ctx, clientset, dyn, version)
	if err != nil {
		return autoscalingRepair{}, fmt.Errorf("%w. Nothing was changed", err)
	}
	if len(findings) == 0 {
		return autoscalingRepair{}, nil
	}
	printAutoscalingFindings(out, findings)
	if n := countDecisions(findings); n > 0 {
		if !opts.SkipDecisions {
			return autoscalingRepair{}, fmt.Errorf("%d app(s) need a decision on their autoscaling settings before this upgrade, listed above. "+
				"Nothing was changed. Make the change shown for each, or pass --skip-autoscaling-check to upgrade and leave them as they are", n)
		}
		_, _ = fmt.Fprintf(out, "  !   --skip-autoscaling-check: the apps that need a decision are left as they are.\n")
	}
	var fixes []autoscalingFinding
	for _, f := range findings {
		if f.HasFix {
			fixes = append(fixes, f)
		}
	}
	if len(fixes) == 0 {
		return autoscalingRepair{}, nil
	}
	if !opts.Repair {
		if !opts.IsTTY {
			return autoscalingRepair{}, fmt.Errorf("this upgrade would change the autoscaling settings listed above, and there is no terminal to ask. " +
				"Nothing was changed. Pass --repair-autoscaling to apply them")
		}
		ok, err := confirm()
		if err != nil {
			return autoscalingRepair{}, err
		}
		if !ok {
			return autoscalingRepair{}, fmt.Errorf("the autoscaling fixes were declined. Nothing was changed. " +
				"The new rules refuse these settings, so change them yourself or run kip upgrade again and agree")
		}
	}
	return autoscalingRepair{consented: fixes}, nil
}

// confirmAutoscalingRepair asks on the terminal. The caller has already
// stopped a run without one, so a missing terminal never reaches here.
func confirmAutoscalingRepair() (bool, error) {
	fmt.Print("  Apply these fixes? [y/N] ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}
	return parseYesNo(line), nil
}

// autoscalingOldWriter is what a console-api from before the upgrade can
// still do to the apps this repair writes.
const autoscalingOldWriter = "run pods for an app this repair records as stopped, since it does not know about stops"

// repairAutoscaling writes agreed fixes after confirming the pinned
// console-api rollout has no old writers. If that check fails, fixes wait
// for a later upgrade run while the rest of this upgrade continues.
func repairAutoscaling(
	ctx context.Context,
	clientset kubernetes.Interface,
	dyn dynamic.Interface,
	out io.Writer,
	version string,
	repair autoscalingRepair,
	rolled consoleAPIRollout,
	rolledConsoleAPI bool,
) {
	if len(repair.consented) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "\n  ...  Autoscaling settings\n")
	skipped := func(why string) {
		_, _ = fmt.Fprintf(out, "  !   %s\n"+
			"      No autoscaling setting was changed. Run kip upgrade again to apply the fixes.\n", why)
	}
	pinned := rolled
	if pinned.hash == "" {
		if rolledConsoleAPI {
			skipped("This upgrade could not record which console-api it rolled, so it cannot tell whether the one it replaced has stopped.")
			return
		}
		var err error
		if pinned, err = pinConsoleAPIRollout(ctx, clientset); err != nil {
			skipped(err.Error())
			return
		}
	}
	if err := waitForConsoleAPIQuiescence(ctx, clientset, pinned, autoscalingOldWriter); err != nil {
		skipped(err.Error())
		return
	}
	commands := map[string]string{}
	for _, agreed := range repair.consented {
		if err := writeAutoscalingFix(ctx, clientset, dyn, out, version, agreed, commands); err != nil {
			_, _ = fmt.Fprintf(out, "  ✗  Could not write the fix for %s/%s: %v\n"+
				"      The fixes after it were not written. Run kip upgrade again to resume.\n", agreed.Namespace, agreed.Name, err)
			return
		}
	}
	// What is left covers apps that changed after the consent, apps that
	// arrived since, and the decisions --skip-autoscaling-check passed over.
	left, err := assessClusterAutoscaling(ctx, clientset, dyn, version)
	if err != nil {
		_, _ = fmt.Fprintf(out, "  !   Could not check what is left after the fixes: %v\n", err)
		return
	}
	open := 0
	for _, f := range left {
		if f.HasFix || f.Decision {
			open++
		}
	}
	if open > 0 {
		_, _ = fmt.Fprintf(out, "  !   %d app(s) still have autoscaling settings the new rules refuse.\n"+
			"      Run 'kip upgrade --check' to see them, then kip upgrade again.\n", open)
	}
}

// writeAutoscalingFix re-reads the app, recomputes its row and writes the fix
// only while consent still covers it. Each conflict retry decides again from
// what the competing write left.
func writeAutoscalingFix(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, out io.Writer, version string, agreed autoscalingFinding, commands map[string]string) error {
	var report string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		report = ""
		app, err := dyn.Resource(deployer.AppGVR).Namespace(agreed.Namespace).Get(ctx, agreed.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			report = fmt.Sprintf("  ✔  %s/%s: the app is gone, so there was nothing to fix.\n", agreed.Namespace, agreed.Name)
			return nil
		}
		if err != nil {
			return err
		}
		now := assessAutoscaling(readAppState(ctx, clientset, app, commandFor(ctx, clientset, app, commands)), version)
		switch {
		case now == nil:
			report = fmt.Sprintf("  ✔  %s/%s: already fixed.\n", agreed.Namespace, agreed.Name)
			return nil
		case !agreed.covers(*now):
			report = fmt.Sprintf("  !   %s/%s changed after you agreed, and nothing was written for it: %s\n", agreed.Namespace, agreed.Name, now.Problem)
			return nil
		}
		if err := applyCapacity(app, *now, time.Now()); err != nil {
			return err
		}
		written, err := dyn.Resource(deployer.AppGVR).Namespace(agreed.Namespace).Update(ctx, app, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
		if err := deployer.RequireStopStored(written, now.After.Stopped && !now.Before.Stopped); err != nil {
			return err
		}
		report = fmt.Sprintf("  ✔  %s/%s: %s\n", agreed.Namespace, agreed.Name, strings.Join(fixLines(*now), ", "))
		return nil
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprint(out, report)
	return nil
}

// applyCapacity sets only the fields the fix changes, leaving the policy's
// state and targets as they are.
func applyCapacity(app *unstructured.Unstructured, f autoscalingFinding, at time.Time) error {
	b, a := f.Before, f.After
	set := func(v int32, path ...string) error {
		if err := unstructured.SetNestedField(app.Object, int64(v), path...); err != nil {
			return fmt.Errorf("setting %s: %w", strings.Join(path, "."), err)
		}
		return nil
	}
	if a.Replicas != b.Replicas {
		if err := set(a.Replicas, "spec", "replicas"); err != nil {
			return err
		}
	}
	if a.Min != b.Min {
		if err := set(a.Min, "spec", "autoscale", "minReplicas"); err != nil {
			return err
		}
	}
	if a.Max != b.Max {
		if err := set(a.Max, "spec", "autoscale", "maxReplicas"); err != nil {
			return err
		}
	}
	if a.Stopped && !b.Stopped {
		stop := map[string]interface{}{"at": at.UTC().Format(time.RFC3339), "by": "kip upgrade", "reason": a.StopReason}
		if err := unstructured.SetNestedMap(app.Object, stop, "spec", "stopped"); err != nil {
			return fmt.Errorf("setting the stop: %w", err)
		}
	}
	return nil
}

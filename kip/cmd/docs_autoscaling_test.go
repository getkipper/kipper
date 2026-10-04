package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/capacity"
	"github.com/getkipper/kipper/kip/internal/deployer"
)

// docSection returns the part of a docs page from the heading line that starts
// with heading up to the next heading of the same or a higher level. Lines in
// fenced code blocks are not headings, so a shell comment does not end it.
func docSection(t *testing.T, page, heading string) string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/en/" + page)
	require.NoError(t, err)

	level := len(strings.SplitN(heading, " ", 2)[0])
	var section []string
	inFence := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
		}
		if !inFence {
			if section == nil && strings.HasPrefix(line, heading) {
				section = []string{}
			} else if section != nil {
				hashes := len(line) - len(strings.TrimLeft(line, "#"))
				if hashes > 0 && hashes <= level && strings.HasPrefix(line[hashes:], " ") {
					break
				}
			}
		}
		if section != nil {
			section = append(section, line)
		}
	}
	require.NotNil(t, section, "%s has no heading %q", page, heading)
	return strings.Join(section, "\n")
}

func localFlagNames(cmd *cobra.Command) []string {
	var names []string
	cmd.LocalFlags().VisitAll(func(f *pflag.Flag) { names = append(names, "--"+f.Name) })
	return names
}

func TestCLIReferenceDocumentsEveryAutoscaleAndScaleFlag(t *testing.T) {
	autoscale := docSection(t, "cli-reference.md", "## kip app autoscale")
	for _, name := range localFlagNames(appAutoscaleCmd) {
		assert.Contains(t, autoscale, "`"+name+"`", "the kip app autoscale reference does not document %s", name)
	}

	scale := docSection(t, "cli-reference.md", "## kip app scale")
	for _, name := range localFlagNames(appScaleCmd) {
		assert.Contains(t, scale, "`"+name+"`", "the kip app scale reference does not document %s", name)
	}
	assert.Contains(t, scale, "kip app deploy --replicas", "the reference does not say that deploy's replica count follows the same bounds")
}

func TestMaintenanceDocumentsEveryUpgradeFlag(t *testing.T) {
	flags := docSection(t, "maintenance.md", "### Flags")
	for _, name := range localFlagNames(upgradeCmd) {
		assert.Contains(t, flags, "`"+name+"`", "the kip upgrade flag table does not document %s", name)
	}
}

func TestMaintenanceExplainsTheUpgradeCheck(t *testing.T) {
	check := docSection(t, "maintenance.md", "### Checking before an upgrade")
	for _, want := range []string{
		"kip upgrade --check",
		"--repair-autoscaling",
		"--skip-autoscaling-check",
		"Apply these fixes? [y/N]",
		"kip app stop",
		"scaled to 0 before Kipper",
	} {
		assert.Contains(t, check, want)
	}
}

// Keep the legacy-zero stop reason recognizable across release versions.
func TestRowFourStopReasonMatchesTheDocs(t *testing.T) {
	f := assessAutoscaling(stateOf(bounded(false), 0, 0), "v9.9.9")
	require.NotNil(t, f)
	require.Equal(t, 4, f.Row)
	assert.True(t, strings.HasPrefix(f.After.StopReason, "scaled to 0 before Kipper "))
}

// An omitted replicas keeps the count stored in the App spec, which can differ
// from the running count while autoscaling is on, so the docs must say which.
func TestDocsDescribeOmittedReplicasAsTheStoredCount(t *testing.T) {
	apply := docSection(t, "gitops.md", "## Applying a manifest")
	assert.Contains(t, apply, "stored replica count")
	assert.Contains(t, apply, "/en/deploying-apps#switching-autoscaling-off")
	assert.NotContains(t, apply, "current count")

	inKipperYAML := docSection(t, "deploying-apps.md", "### In kipper.yaml")
	assert.Contains(t, inKipperYAML, "stored replica count")
	assert.NotContains(t, inKipperYAML, "current count")

	skill, err := os.ReadFile("../../skills/kipper/SKILL.md")
	require.NoError(t, err)
	assert.NotContains(t, string(skill), "keeps the current count")
	assert.Contains(t, string(skill), "keeps the stored replica count")
}

// Every bold label in the console instructions must exist in the console code
// that renders the Scale tab, so the docs cannot name a control that is gone.
func TestConsoleDocsNameOnlyLabelsTheCapacityPanelShows(t *testing.T) {
	inConsole := docSection(t, "deploying-apps.md", "### In the console")
	var source strings.Builder
	for _, file := range []string{
		"../../console/src/components/AppCapacityPanel.vue",
		"../../console/src/utils/capacity.ts",
		"../../console/src/components/AppDetail.vue",
	} {
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		source.Write(raw)
	}

	labels := regexp.MustCompile(`\*\*([^*]+)\*\*`).FindAllStringSubmatch(inConsole, -1)
	require.NotEmpty(t, labels, "the console instructions name no controls")
	for _, m := range labels {
		assert.True(t, strings.Contains(source.String(), m[1]), "the docs name the console label %q, which the code does not show", m[1])
	}
	for _, want := range []string{"Desired", "Minimum", "Maximum", "Current", "Scaling policy", "None (fixed count)", "Target tracking", "Save capacity", "Raise maximum to"} {
		assert.Contains(t, inConsole, "**"+want, "the console instructions do not describe %q", want)
	}
}

// The Scale tab's toggle and replica buttons were replaced by the capacity
// panel, so no page may still send a reader to them.
func TestNoPageDescribesTheRemovedAutoscalingControls(t *testing.T) {
	pages, err := filepath.Glob("../../docs/en/*.md")
	require.NoError(t, err)
	pages = append(pages, "../../skills/kipper/SKILL.md")
	for _, page := range pages {
		raw, err := os.ReadFile(page)
		require.NoError(t, err)
		for _, stale := range []string{"Save autoscaling", "replica buttons", "toggle **Autoscaling**", "Toggle autoscaling off"} {
			assert.False(t, strings.Contains(string(raw), stale), "%s still describes a removed control: %q", filepath.Base(page), stale)
		}
	}
}

func TestDeployingAppsMapsTheAutoScalingGroupTerms(t *testing.T) {
	autoscaling := docSection(t, "deploying-apps.md", "## Autoscaling")
	for _, row := range []string{"| Desired capacity", "| Minimum capacity", "| Maximum capacity", "| Target tracking", "| Current", "| Activity history"} {
		assert.Contains(t, autoscaling, row, "the ASG table has no %q row", row)
	}
	assert.Contains(t, autoscaling, "existing nodes")
}

// Switching off a stored block whose minimum exceeds its maximum changes the
// block, so the API server refuses it; the docs quote the CRD's message.
func TestDocsQuoteTheAPIServersBoundsRefusal(t *testing.T) {
	const refusal = "minReplicas must not exceed maxReplicas"
	crd, err := os.ReadFile("../../deploy/crds/kipper.run_apps.yaml")
	require.NoError(t, err)
	require.Contains(t, string(crd), refusal)

	assert.Contains(t, docSection(t, "deploying-apps.md", "### When settings do not add up"), refusal)
	assert.Contains(t, docSection(t, "cli-reference.md", "## kip app autoscale"), refusal)
	skill, err := os.ReadFile("../../skills/kipper/SKILL.md")
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(skill), refusal), "SKILL.md does not quote %q", refusal)
}

// Turning autoscaling off in a manifest resolves an omitted replicas the same
// way as any bounded manifest, so a stored count outside the bounds gives way to
// the minimum.
func TestDocsSayAnOmittedReplicasFallsBackToTheMinimumWhenSwitchingOff(t *testing.T) {
	switchingOff := docSection(t, "deploying-apps.md", "### Switching autoscaling off")
	assert.Contains(t, switchingOff, "`minReplicas`")

	apply := docSection(t, "gitops.md", "## Applying a manifest")
	assert.Contains(t, apply, "switches autoscaling off and\nruns the stored count, or `minReplicas`")
}

// A minimum above 1 without a maximum is refused by kip apply and the API
// server alike, and the docs quote the shared message.
func TestDocsQuoteTheMinimumWithoutMaximumRefusal(t *testing.T) {
	const refusal = "set maxReplicas with minReplicas, or leave both out to remove the bounds"
	source, err := os.ReadFile("../../controller/pkg/capacity/capacity.go")
	require.NoError(t, err)
	require.Contains(t, string(source), refusal)

	assert.Contains(t, docSection(t, "deploying-apps.md", "### In kipper.yaml"), refusal)
	assert.Contains(t, docSection(t, "maintenance.md", "#### Apps that need a decision"), "a minimum above 1 without a maximum")
	skill, err := os.ReadFile("../../skills/kipper/SKILL.md")
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(skill), "`minReplicas` above 1 without `maxReplicas`"), "SKILL.md does not describe the minimum-without-maximum refusal")
}

// --status prints the AutoscalingReady condition when it is not True, and the
// docs quote the start of that line.
func TestDocsQuoteTheStatusConditionLine(t *testing.T) {
	lines := autoscaleStatusLines("api", deployer.AutoscaleStatus{
		Replicas:  1,
		Condition: &deployer.AutoscalingCondition{Status: "False", Reason: "InvalidPolicy", Message: "x"},
	})
	const quoted = "Autoscaling is not ready ("
	require.Contains(t, strings.Join(lines, "\n"), quoted)

	assert.Contains(t, docSection(t, "deploying-apps.md", "## Autoscaling"), quoted)
	assert.Contains(t, docSection(t, "cli-reference.md", "## kip app autoscale"), quoted)
	skill, err := os.ReadFile("../../skills/kipper/SKILL.md")
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(skill), "`--status` also prints the `AutoscalingReady` condition"), "SKILL.md does not mention the --status condition line")
	assert.Contains(t, appAutoscaleCmd.Flags().Lookup("status").Usage, "autoscaling problem")
}

// --off and --remove keep ignoring the setting flags, and say so; the docs and
// the help must give the same two-step order.
func TestDocsExplainThatOffIgnoresTheSettingFlags(t *testing.T) {
	notice := ignoredEditFlagsNotice("api", modeOff, []string{"--max 8"})
	require.Contains(t, notice, "Ignored --max 8")

	for _, section := range []string{
		docSection(t, "deploying-apps.md", "## Autoscaling"),
		docSection(t, "cli-reference.md", "## kip app autoscale"),
	} {
		assert.Contains(t, section, "ignore `--min`, `--max`, `--cpu` and `--memory`")
	}
	skill, err := os.ReadFile("../../skills/kipper/SKILL.md")
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(skill), "`--off` and `--remove` ignore the setting flags"), "SKILL.md does not say that --off ignores the setting flags")
	assert.Contains(t, appAutoscaleCmd.Long, "--off and --remove ignore --min, --max, --cpu and --memory")
}

// --yes confirms the component upgrade only; a non-interactive autoscaling
// repair needs --repair-autoscaling.
func TestDocsSayYesDoesNotConfirmAutoscalingFixes(t *testing.T) {
	flags := docSection(t, "maintenance.md", "### Flags")
	var yesRow string
	for _, line := range strings.Split(flags, "\n") {
		if strings.HasPrefix(line, "| `--yes`") {
			yesRow = line
		}
	}
	require.NotEmpty(t, yesRow)
	assert.Contains(t, yesRow, "`--repair-autoscaling`")

	skill, err := os.ReadFile("../../skills/kipper/SKILL.md")
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(skill), "`--yes` skips the component confirm only"), "SKILL.md does not say what --yes leaves out")
}

// Every badge label the capacity panel can show appears in the console docs,
// along with the refusal of Desired 0 and the panel's refresh.
func TestConsoleDocsListEveryCapacityBadge(t *testing.T) {
	raw, err := os.ReadFile("../../console/src/utils/capacity.ts")
	require.NoError(t, err)
	labels := regexp.MustCompile(`label: '([^']+)'`).FindAllStringSubmatch(string(raw), -1)
	require.NotEmpty(t, labels)

	inConsole := docSection(t, "deploying-apps.md", "### In the console")
	for _, m := range labels {
		assert.Contains(t, inConsole, "**"+m[1]+"**", "the console docs do not list the badge %q", m[1])
	}
	assert.Contains(t, inConsole, "A desired count of 0 is refused with or without bounds")
	assert.Contains(t, inConsole, "every 15 seconds")
}

// The migration plan's blockers say how to fix each kind of problem, and the
// target's CRD rules check the autoscaling block but not the replica count.
func TestDocsMigrationExplainsHowToFixAnAutoscaledCountOutsideItsBounds(t *testing.T) {
	section := docSection(t, "migration.md", "### 3. Freeze writes on the source")

	assert.NotContains(t, section, "against the rules the target applies")
	assert.Contains(t, section, "`kip app autoscale <app>` to save the policy again")
	assert.Contains(t, section, "`kip app autoscale <app> --off`")
}

// A block stored before the rules with a minimum above 1 and no maximum is
// refused when switched off, and kip names the command that fixes it first.
func TestDocsExplainSwitchingOffAMinimumWithoutMaximum(t *testing.T) {
	const refusal = "set maxReplicas with minReplicas, or leave both out to remove the bounds"
	const fix = "kip app autoscale api --max 5"
	advice := switchOffError("api", &deployer.InvalidPolicyRefusal{
		Policy: capacity.Policy{Enabled: true, MinReplicas: i32p(3), CPUTarget: i32p(70)},
		Err:    errors.New(refusal),
	}).Error()
	require.Contains(t, advice, "'"+fix+"'")

	for _, section := range []string{
		docSection(t, "deploying-apps.md", "### When settings do not add up"),
		docSection(t, "cli-reference.md", "## kip app autoscale"),
	} {
		assert.Contains(t, section, "`"+refusal+"`")
		assert.Contains(t, section, "`"+fix+"`")
	}
}

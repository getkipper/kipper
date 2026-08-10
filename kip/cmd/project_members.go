package cmd

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

var validProjectRoles = map[string]bool{"owner": true, "deployer": true, "viewer": true}

var projectMembersCmd = &cobra.Command{
	Use:   "members",
	Short: "Manage who can access a project",
}

var projectMembersListCmd = &cobra.Command{
	Use:   "list [project]",
	Short: "List a project's members and their roles",
	Args:  cobra.ExactArgs(1),
	RunE:  runProjectMembersList,
}

var projectMembersAddCmd = &cobra.Command{
	Use:   "add [project] [email] [role]",
	Short: "Add a member to a project, or change their role",
	Long: `Add a member to a project with a role of owner, deployer, or viewer.
Owners manage the project's members, deployers can deploy, viewers are read-only.

The person must already have a Kipper account, because membership is recorded
as an address and nothing later checks that it belongs to anyone. Invite them
first with 'kip user invite', and add them once they have accepted.`,
	Args: cobra.ExactArgs(3),
	RunE: runProjectMembersAdd,
}

var projectMembersRemoveCmd = &cobra.Command{
	Use:   "remove [project] [email]",
	Short: "Remove a member from a project",
	Long: `Remove a member from a project.

A project keeps at least one owner, so removing the last one is refused. The
ordinary way past that is to add a replacement owner first and then remove the
old one, which needs no flag and leaves the project owned.

Pass --force to remove a last owner outright, leaving the project with none. It
is for the case where a mistyped owner should go before a replacement has been
chosen.`,
	Args: cobra.ExactArgs(2),
	RunE: runProjectMembersRemove,
}

func init() {
	projectMembersCmd.AddCommand(projectMembersListCmd)
	projectMembersCmd.AddCommand(projectMembersAddCmd)
	projectMembersRemoveCmd.Flags().Bool("force", false, "remove even the last owner, leaving the project with none")
	projectMembersCmd.AddCommand(projectMembersRemoveCmd)
	projectCmd.AddCommand(projectMembersCmd)
}

func runProjectMembersList(cmd *cobra.Command, args []string) error {
	project := args[0]
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	cr, err := k8sClient.Dynamic().Resource(manifest.ProjectGVR).Get(ctx, project, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting project %q: %w", project, err)
	}

	members, _, _ := unstructured.NestedSlice(cr.Object, "spec", "members")
	if len(members) == 0 {
		fmt.Printf("\n  %s has no members\n\n", project)
		return nil
	}

	fmt.Printf("\n  %-40s %s\n", "EMAIL", "ROLE")
	for _, m := range members {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		email, _ := mm["email"].(string)
		role, _ := mm["role"].(string)
		fmt.Printf("  %-40s %s\n", email, role)
	}
	fmt.Println()
	return nil
}

// accountExists reports whether the cluster knows an account for email.
//
// A read that fails is an error rather than "no account". Treating one as the
// other would refuse a legitimate add with a message saying the person does not
// exist, which is a worse answer than saying the check could not be made.
func accountExists(ctx context.Context, cs kubernetes.Interface, email string) (bool, error) {
	cm, err := cs.CoreV1().ConfigMaps("kipper-system").Get(ctx, "kipper-users", metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("reading the cluster's user list: %w", err)
	}
	var roles map[string]string
	if err := json.Unmarshal([]byte(cm.Data["users"]), &roles); err != nil {
		return false, fmt.Errorf("reading the cluster's user list: %w", err)
	}
	_, ok := roles[email]
	return ok, nil
}

func runProjectMembersAdd(cmd *cobra.Command, args []string) error {
	project, email, role := args[0], args[1], args[2]
	if !validProjectRoles[role] {
		return fmt.Errorf("role must be owner, deployer, or viewer")
	}

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()

	// Membership is an address, and nothing downstream ever checks that the
	// address belongs to anyone — so a typo becomes a member who cannot sign in,
	// looks right in `members list`, and counts as an owner in the guard that
	// keeps a project from being orphaned. The console has always refused this;
	// the CLI wrote it through, which is the only way a project ends up owned by
	// nobody.
	known, err := accountExists(ctx, k8sClient.Clientset(), email)
	if err != nil {
		return err
	}
	if !known {
		return fmt.Errorf(
			"no account exists for %s\n"+
				"  if they have an invite, they need to accept it first: the account is created then\n"+
				"  to invite them:            kip user invite --email %s --role viewer\n"+
				"  if they already sign in through an identity provider: kip user role %s viewer",
			email, email, email)
	}
	client := k8sClient.Dynamic().Resource(manifest.ProjectGVR)
	cr, err := client.Get(ctx, project, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting project %q: %w", project, err)
	}

	members, _, _ := unstructured.NestedSlice(cr.Object, "spec", "members")
	owners := 0
	for _, m := range members {
		if mm, ok := m.(map[string]interface{}); ok && mm["role"] == "owner" {
			owners++
		}
	}
	updated := false
	for _, m := range members {
		if mm, ok := m.(map[string]interface{}); ok && mm["email"] == email {
			if mm["role"] == "owner" && role != "owner" && owners == 1 {
				return fmt.Errorf("cannot demote the last owner of %q", project)
			}
			mm["role"] = role
			updated = true
			break
		}
	}
	if !updated {
		members = append(members, map[string]interface{}{"email": email, "role": role})
	}
	if err := unstructured.SetNestedSlice(cr.Object, members, "spec", "members"); err != nil {
		return fmt.Errorf("setting members: %w", err)
	}
	if _, err := client.Update(ctx, cr, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating project: %w", err)
	}

	fmt.Printf("\n  ✔  %s is now %s on %s\n\n", email, role, project)
	return nil
}

// memberRemoval is what taking one address out of a member list comes to: the
// list that survives, whether the address was there at all, whether an owner is
// going, and how many owners are left behind.
type memberRemoval struct {
	kept            []interface{}
	matched         bool
	removingOwner   bool
	remainingOwners int
}

// withoutMember removes every entry for email from members.
//
// matched is what stops the command reporting a removal it did not perform: an
// address that is in no entry leaves the list identical, and saying "removed"
// about it is how the console's version of this bug stayed invisible.
func withoutMember(members []interface{}, email string) memberRemoval {
	removal := memberRemoval{kept: make([]interface{}, 0, len(members))}
	for _, m := range members {
		entry, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		if entry["email"] == email {
			removal.matched = true
			// Or, not assign: a second entry for the same address must not
			// clear the owner the first one found, or the rule that keeps a
			// project owned is skipped for the pair.
			removal.removingOwner = removal.removingOwner || entry["role"] == "owner"
			continue
		}
		if entry["role"] == "owner" {
			removal.remainingOwners++
		}
		removal.kept = append(removal.kept, m)
	}
	return removal
}

// refusesLastOwner reports whether this removal would leave the project with no
// owner and must therefore be refused.
//
// Pulled out of the command so both halves can be tested: the rule that keeps a
// project owned, and the flag that steps past it. remainingOwners counts the
// owners that would survive the removal.
func refusesLastOwner(removingOwner bool, remainingOwners int, force bool) bool {
	return removingOwner && remainingOwners == 0 && !force
}

func runProjectMembersRemove(cmd *cobra.Command, args []string) error {
	project, email := args[0], args[1]

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	client := k8sClient.Dynamic().Resource(manifest.ProjectGVR)
	cr, err := client.Get(ctx, project, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting project %q: %w", project, err)
	}

	members, _, _ := unstructured.NestedSlice(cr.Object, "spec", "members")
	removal := withoutMember(members, email)
	if !removal.matched {
		return fmt.Errorf(
			"%s is not a member of %q\n"+
				"  kip project members list %s shows who is",
			email, project, project)
	}
	force, _ := cmd.Flags().GetBool("force")
	if refusesLastOwner(removal.removingOwner, removal.remainingOwners, force) {
		return fmt.Errorf(
			"cannot remove the last owner of %q\n"+
				"  add a replacement owner first, which needs no flag and leaves the project owned:\n"+
				"    kip project members add %s <email> owner\n"+
				"  or pass --force to remove them and leave the project with no owner",
			project, project)
	}
	if err := unstructured.SetNestedSlice(cr.Object, removal.kept, "spec", "members"); err != nil {
		return fmt.Errorf("setting members: %w", err)
	}
	if _, err := client.Update(ctx, cr, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating project: %w", err)
	}

	fmt.Printf("\n  ✔  removed %s from %s\n\n", email, project)
	return nil
}

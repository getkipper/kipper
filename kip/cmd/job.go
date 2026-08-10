package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/job"
)

var jobCmd = &cobra.Command{
	Use:   "job",
	Short: "Manage jobs and scheduled tasks",
}

var jobRunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run a one-off job",
	RunE:  runJobRun,
}

var jobScheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Create a scheduled job (cron)",
	RunE:  runJobSchedule,
}

var jobListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all jobs and scheduled tasks",
	RunE:  runJobList,
}

var jobHistoryCmd = &cobra.Command{
	Use:   "history [name]",
	Short: "Show execution history for a job",
	Args:  cobra.ExactArgs(1),
	RunE:  runJobHistory,
}

var jobDeleteCmd = &cobra.Command{
	Use:   "delete [name]",
	Short: "Delete a scheduled job",
	Args:  cobra.ExactArgs(1),
	RunE:  runJobDelete,
}

func init() {
	jobRunCmd.Flags().String("name", "", "job name")
	jobRunCmd.Flags().String("image", "", "container image")
	jobRunCmd.Flags().String("command", "", "command to run (e.g. \"python migrate.py\")")
	jobRunCmd.Flags().String("project", "", "project name")
	jobRunCmd.Flags().String("environment", "", "target environment")
	_ = jobRunCmd.MarkFlagRequired("name")
	_ = jobRunCmd.MarkFlagRequired("image")
	_ = jobRunCmd.MarkFlagRequired("command")

	jobScheduleCmd.Flags().String("name", "", "job name")
	jobScheduleCmd.Flags().String("image", "", "container image")
	jobScheduleCmd.Flags().String("command", "", "command to run")
	jobScheduleCmd.Flags().String("cron", "", "cron expression (e.g. \"0 3 * * *\" for 3am daily)")
	jobScheduleCmd.Flags().String("project", "", "project name")
	jobScheduleCmd.Flags().String("environment", "", "target environment")
	_ = jobScheduleCmd.MarkFlagRequired("name")
	_ = jobScheduleCmd.MarkFlagRequired("image")
	_ = jobScheduleCmd.MarkFlagRequired("command")
	_ = jobScheduleCmd.MarkFlagRequired("cron")

	jobListCmd.Flags().String("project", "", "project name")
	jobListCmd.Flags().String("environment", "", "target environment")

	jobHistoryCmd.Flags().String("project", "", "project name")
	jobHistoryCmd.Flags().String("environment", "", "target environment")

	jobDeleteCmd.Flags().String("project", "", "project name")
	jobDeleteCmd.Flags().String("environment", "", "target environment")

	jobCmd.AddCommand(jobRunCmd)
	jobCmd.AddCommand(jobScheduleCmd)
	jobCmd.AddCommand(jobListCmd)
	jobCmd.AddCommand(jobHistoryCmd)
	jobCmd.AddCommand(jobDeleteCmd)
	rootCmd.AddCommand(jobCmd)
}

func resolveJobNamespace(cmd *cobra.Command) string {
	cluster, _, err := loadCurrentCluster()
	if err != nil {
		project, _ := cmd.Flags().GetString("project")
		environment, _ := cmd.Flags().GetString("environment")
		if environment != "" {
			return fmt.Sprintf("%s-%s", project, environment)
		}
		return project
	}
	project, environment := resolveProjectAndEnvironment(cmd, cluster)
	return cluster.ResolveNamespace(project, environment)
}

func runJobRun(cmd *cobra.Command, args []string) error {
	name, _ := cmd.Flags().GetString("name")
	image, _ := cmd.Flags().GetString("image")
	command, _ := cmd.Flags().GetString("command")
	namespace := resolveJobNamespace(cmd)

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	mgr := &job.Manager{Dynamic: k8sClient.Dynamic(), Client: k8sClient.Clientset()}
	ctx := context.Background()

	fmt.Printf("\n  Running job %q...\n", name)

	runName, err := mgr.Run(ctx, job.Options{
		Name:      name,
		Namespace: namespace,
		Image:     image,
		Command:   strings.Fields(command),
	})
	if err != nil {
		return fmt.Errorf("creating job: %w", err)
	}

	fmt.Printf("  ✔  Job %s started\n", runName)
	fmt.Printf("  Run 'kip job history %s' to check the result\n\n", name)

	return nil
}

func runJobSchedule(cmd *cobra.Command, args []string) error {
	name, _ := cmd.Flags().GetString("name")
	image, _ := cmd.Flags().GetString("image")
	command, _ := cmd.Flags().GetString("command")
	cron, _ := cmd.Flags().GetString("cron")
	namespace := resolveJobNamespace(cmd)

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	mgr := &job.Manager{Dynamic: k8sClient.Dynamic(), Client: k8sClient.Clientset()}
	ctx := context.Background()

	fmt.Printf("\n  Scheduling job %q (%s)...\n", name, cron)

	err = mgr.Schedule(ctx, job.Options{
		Name:      name,
		Namespace: namespace,
		Image:     image,
		Command:   strings.Fields(command),
		Schedule:  cron,
	})
	if err != nil {
		return fmt.Errorf("creating scheduled job: %w", err)
	}

	fmt.Printf("  ✔  Scheduled\n\n")

	return nil
}

func runJobList(cmd *cobra.Command, args []string) error {
	namespace := resolveJobNamespace(cmd)

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	mgr := &job.Manager{Dynamic: k8sClient.Dynamic(), Client: k8sClient.Clientset()}
	ctx := context.Background()

	jobs, err := mgr.List(ctx, namespace)
	if err != nil {
		return err
	}

	if len(jobs) == 0 {
		fmt.Printf("\n  No jobs in %q\n\n", namespace)
		return nil
	}

	fmt.Printf("\n  %-20s %-10s %-20s %-15s %s\n", "NAME", "TYPE", "SCHEDULE", "LAST RUN", "STATUS")
	for _, j := range jobs {
		fmt.Printf("  %-20s %-10s %-20s %-15s %s\n", j.Name, j.Type, j.Schedule, j.Last, j.Status)
	}
	fmt.Println()

	return nil
}

func runJobHistory(cmd *cobra.Command, args []string) error {
	name := args[0]
	namespace := resolveJobNamespace(cmd)

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	mgr := &job.Manager{Dynamic: k8sClient.Dynamic(), Client: k8sClient.Clientset()}
	ctx := context.Background()

	history, err := mgr.History(ctx, namespace, name)
	if err != nil {
		return err
	}

	if len(history) == 0 {
		fmt.Printf("\n  No history for %q\n\n", name)
		return nil
	}

	fmt.Printf("\n  %-40s %-15s %s\n", "JOB", "STARTED", "STATUS")
	for _, h := range history {
		fmt.Printf("  %-40s %-15s %s\n", h.Name, h.Last, h.Status)
	}
	fmt.Println()

	return nil
}

func runJobDelete(cmd *cobra.Command, args []string) error {
	name := args[0]
	namespace := resolveJobNamespace(cmd)

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	mgr := &job.Manager{Dynamic: k8sClient.Dynamic(), Client: k8sClient.Clientset()}
	ctx := context.Background()

	if err := mgr.Delete(ctx, namespace, name); err != nil {
		return err
	}

	fmt.Printf("\n  ✔  Scheduled job %q deleted\n\n", name)
	return nil
}

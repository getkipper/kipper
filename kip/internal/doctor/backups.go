package doctor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const PartBackups Part = "backups"

var (
	veleroBackupGVR   = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "backups"}
	veleroScheduleGVR = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "schedules"}
)

// BackupState holds the cluster's Velero backups and schedules. Configured
// reports whether the Velero backup resource is available.
type BackupState struct {
	Configured bool
	Runs       []BackupRun
	Schedules  []BackupSchedule
}

// BackupRun is one Velero backup. Schedule is empty for a manual backup.
type BackupRun struct {
	Name     string
	Schedule string
	Phase    string
	Created  time.Time
	Errors   int64
	Warnings int64
}

// BackupSchedule is one Velero schedule. LastBackup is zero when its last run
// time is missing or invalid.
type BackupSchedule struct {
	Name       string
	Cron       string
	Paused     bool
	Created    time.Time
	LastBackup time.Time
}

// finishedBackupPhases identifies terminal backup phases.
var finishedBackupPhases = map[string]bool{"Completed": true, "PartiallyFailed": true, "Failed": true, "FailedValidation": true}

// CheckBackups checks each unpaused schedule's newest terminal backup by creation
// time. Failed and FailedValidation backups fail the check, and PartiallyFailed
// backups warn. Recognised daily or faster schedules warn after two days without
// a run, and weekly schedules warn after two weeks. Schedules with no recorded
// run use their creation time for this grace period. Other cadences are checked
// only for backup failures. Manual backups are excluded.
//
// Missing backup data or an unavailable Velero backup resource skips the check.
// Otherwise, the result uses the highest severity, with problems ordered by
// schedule name. No active schedules or no detected problems returns Pass, even
// if no backup has finished yet.
func CheckBackups(snap Snapshot, now time.Time) Result {
	const id, label = "backups", "Backups"
	if err, ok := snap.Missing[PartBackups]; ok {
		return notChecked(id, label, PartBackups, err)
	}
	state := snap.Backups
	if !state.Configured {
		return Result{ID: id, Label: label, Status: Skip, Message: "not checked (backups are not set up on this cluster)"}
	}

	status := Pass
	var problems []string
	active := 0
	raise := func(s Status, problem string) {
		if s > status {
			status = s
		}
		problems = append(problems, problem)
	}
	schedules := append([]BackupSchedule(nil), state.Schedules...)
	sort.Slice(schedules, func(i, j int) bool { return schedules[i].Name < schedules[j].Name })
	for _, s := range schedules {
		if s.Paused {
			continue
		}
		active++
		interval := scheduleInterval(s.Cron)
		grace := 2 * interval
		if latest, ok := latestFinishedRun(state.Runs, s.Name); ok {
			switch latest.Phase {
			case "Failed", "FailedValidation":
				raise(Fail, fmt.Sprintf("the last %s backup %s failed. Run kip backup describe %s", s.Name, latest.Name, latest.Name))
			case "PartiallyFailed":
				raise(Warn, fmt.Sprintf("the last %s backup %s partially failed with %s. Run kip backup describe %s",
					s.Name, latest.Name, fmt.Sprintf("%d %s", latest.Errors, plural(int(latest.Errors), "error", "errors")), latest.Name))
			}
		}
		switch {
		case interval == 0:
		case s.LastBackup.IsZero() && now.Sub(s.Created) > grace:
			raise(Warn, fmt.Sprintf("%s has never run", s.Name))
		case !s.LastBackup.IsZero() && now.Sub(s.LastBackup) > grace:
			raise(Warn, fmt.Sprintf("%s has not run since %s", s.Name, s.LastBackup.UTC().Format("2006-01-02 15:04 UTC")))
		}
	}
	if active == 0 {
		return Result{ID: id, Label: label, Status: Pass, Message: "no active backup schedules"}
	}
	if len(problems) == 0 {
		return Result{ID: id, Label: label, Status: Pass, Message: "no problems found in scheduled backups"}
	}
	return Result{ID: id, Label: label, Status: status, Message: strings.Join(problems, ". ")}
}

// latestFinishedRun selects the named schedule's terminal backup with the latest
// creation time. It returns false when none exists.
func latestFinishedRun(runs []BackupRun, schedule string) (BackupRun, bool) {
	var latest BackupRun
	found := false
	for _, r := range runs {
		if r.Schedule != schedule || !finishedBackupPhases[r.Phase] {
			continue
		}
		if !found || r.Created.After(latest.Created) {
			latest, found = r, true
		}
	}
	return latest, found
}

// scheduleInterval returns a day for a daily or faster cron expression and a
// week for one that runs on chosen weekdays, which covers Kipper's own
// schedules. It returns zero for a schedule it cannot judge, such as a monthly
// one, which is then not checked for staleness.
func scheduleInterval(cron string) time.Duration {
	switch cron {
	case "@daily", "@midnight", "@hourly":
		return 24 * time.Hour
	case "@weekly":
		return 7 * 24 * time.Hour
	}
	f := strings.Fields(cron)
	switch {
	case len(f) != 5 || f[2] != "*" || f[3] != "*":
		return 0
	case f[4] != "*":
		return 7 * 24 * time.Hour
	}
	return 24 * time.Hour
}

// fetchBackupsOverSSH reads Velero's backups and schedules with kubectl.
func fetchBackupsOverSSH(r Runner) (BackupState, error) {
	out, err := r.Run("kubectl get crd backups.velero.io -o name --ignore-not-found " + requestTimeout)
	if err != nil {
		return BackupState{}, commandError(err)
	}
	if strings.TrimSpace(out) == "" {
		return BackupState{}, nil
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := runJSON(r, "kubectl get backups.velero.io,schedules.velero.io -n velero -o json "+requestTimeout, &list); err != nil {
		return BackupState{}, err
	}
	state := BackupState{Configured: true}
	for _, item := range list.Items {
		u := unstructured.Unstructured{Object: item}
		switch u.GetKind() {
		case "Backup":
			state.Runs = append(state.Runs, backupRunFrom(u))
		case "Schedule":
			state.Schedules = append(state.Schedules, backupScheduleFrom(u))
		}
	}
	return state, nil
}

// ReadBackupsViaAPI reads backups and schedules from the velero namespace.
// A missing backup resource returns an unconfigured state and no error. A missing
// schedule resource leaves the schedules empty and the state configured. Other
// API errors return an empty state and the error.
func ReadBackupsViaAPI(ctx context.Context, dyn dynamic.Interface) (BackupState, error) {
	backups, err := dyn.Resource(veleroBackupGVR).Namespace("velero").List(ctx, metav1.ListOptions{})
	if isNotFound(err) {
		return BackupState{}, nil
	}
	if err != nil {
		return BackupState{}, err
	}
	schedules, err := dyn.Resource(veleroScheduleGVR).Namespace("velero").List(ctx, metav1.ListOptions{})
	if err != nil && !isNotFound(err) {
		return BackupState{}, err
	}
	state := BackupState{Configured: true}
	for _, b := range backups.Items {
		state.Runs = append(state.Runs, backupRunFrom(b))
	}
	if schedules != nil {
		for _, s := range schedules.Items {
			state.Schedules = append(state.Schedules, backupScheduleFrom(s))
		}
	}
	return state, nil
}

func backupRunFrom(u unstructured.Unstructured) BackupRun {
	phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
	return BackupRun{
		Name:     u.GetName(),
		Schedule: u.GetLabels()["velero.io/schedule-name"],
		Phase:    phase,
		Created:  u.GetCreationTimestamp().UTC(),
		Errors:   statusCount(u, "errors"),
		Warnings: statusCount(u, "warnings"),
	}
}

// statusCount accepts float64 values from JSON map decoding and int64 values
// from the API client. Missing or unsupported values return zero.
func statusCount(u unstructured.Unstructured, field string) int64 {
	v, _, _ := unstructured.NestedFieldNoCopy(u.Object, "status", field)
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

func backupScheduleFrom(u unstructured.Unstructured) BackupSchedule {
	cron, _, _ := unstructured.NestedString(u.Object, "spec", "schedule")
	paused, _, _ := unstructured.NestedBool(u.Object, "spec", "paused")
	s := BackupSchedule{Name: u.GetName(), Cron: cron, Paused: paused, Created: u.GetCreationTimestamp().UTC()}
	if last, ok, _ := unstructured.NestedString(u.Object, "status", "lastBackup"); ok {
		if t, err := time.Parse(time.RFC3339, last); err == nil {
			s.LastBackup = t.UTC()
		}
	}
	return s
}

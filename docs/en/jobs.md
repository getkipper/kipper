# Jobs & Scheduled Tasks

Kipper wraps Kubernetes Jobs and CronJobs so you can run one-off tasks and scheduled jobs without writing YAML.

## Running a one-off job

```bash
kip job run --name db-migrate --image myapp:latest --command "npm run migrate" --project blog --environment test
```

```
  Running job "db-migrate"...
  ✔  Job started
  Run 'kip job history db-migrate' to check the result
```

The job runs to completion in a new pod, then stops. It carries its own environment variables, from the `env:` block on the job in `kipper.yaml`, and reads nothing belonging to an app of the same name on a cluster old enough to hold both. New ones cannot share a name: see [names are shared across workload kinds](/en/functions#names-are-shared-across-workload-kinds).

## Scheduling a recurring job

```bash
kip job schedule --name nightly-cleanup --image myapp:latest --command "python cleanup.py" --cron "0 3 * * *" --project blog --environment prod
```

Common cron expressions:

| Expression | Meaning |
|---|---|
| `0 3 * * *` | Every day at 3am |
| `*/15 * * * *` | Every 15 minutes |
| `0 0 * * 0` | Every Sunday at midnight |
| `0 9 * * 1-5` | Weekdays at 9am |

## Listing jobs

```bash
kip job list --project blog --environment test
```

```
  NAME                 TYPE       SCHEDULE             LAST RUN        STATUS
  nightly-cleanup      cronjob    0 3 * * *            8h ago          scheduled
  db-migrate           job                             2m ago          completed
```

## Viewing history

```bash
kip job history nightly-cleanup --project blog --environment prod
```

Shows all past executions with timestamps and status (completed/failed/running).

## Deleting a scheduled job

```bash
kip job delete nightly-cleanup --project blog --environment prod
```

## Resource limits

Configure CPU and memory limits for your jobs from the **Resources** tab in the job detail panel. Click a job in the web console, switch to the **Resources** tab, and adjust the CPU and memory requests and limits.

Resource limits control how much CPU and memory each job pod is allowed to consume. Jobs that process large datasets or run memory-intensive tasks (database migrations, batch imports) may need higher limits than the defaults.

Changes apply to the next job run. They do not affect any execution that is already in progress.

## How it works

- **One-off jobs** create a `Job` Custom Resource (`kipper.run/v1alpha1`). The reconciler creates a Kubernetes `batchv1.Job` that runs to completion then stops, with `TTLSecondsAfterFinished = 86400` so the underlying pod auto-cleans.
- **Scheduled jobs** create a `Job` CR with a cron schedule. The reconciler creates a Kubernetes CronJob that spawns a pod on each run.
- The CLI and the web console write the same Job CRs, so a job created with `kip job schedule` shows up immediately in the Jobs view and vice versa. The CR is the source of truth.
- A job's `env:` block is published as an immutable Secret named for a digest of its contents, `job-<name>-env-<digest>`, which its pods read through `envFrom`. The name carries the kind, so a job and an app of one name keep separate configuration on a cluster old enough to hold both. New ones cannot share a name: see [names are shared across workload kinds](/en/functions#names-are-shared-across-workload-kinds).
- A value may reference another by name, the same [`${NAME}` syntax apps use](/en/secrets#referencing-another-variable). A job binds no services, so its own `env:` block is all there is to reference: a `${DB_PASSWORD}` in a job reaches the process as written.
- A retry runs the environment its job started with. The pod template names one exact published Secret and a Kubernetes Job's template cannot be changed once created, so editing `env:` while a job is retrying has no effect on that run. The edit applies to the next one.
- No additional infrastructure needed. Kubernetes handles the scheduling natively.

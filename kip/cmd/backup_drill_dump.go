package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// drillDumpDir is the expected location of the backup hook's consistent dumps
// on the dump volume restored with the Postgres pod.
const drillDumpDir = "/var/lib/kipper-dump/current"

// dumpCountSQL prints "schema.table|rows" for each user table when run by psql,
// matching the backup hook's row-count files.
const dumpCountSQL = `SELECT format('SELECT %L || ''|'' || count(*) FROM %I.%I', schemaname || '.' || tablename, schemaname, tablename) FROM pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema') ORDER BY 1 \gexec`

const (
	dumpCheckNone       = "KIPPER_DUMP_NONE"
	dumpCheckIncomplete = "KIPPER_DUMP_INCOMPLETE"
	dumpCheckOK         = "KIPPER_DUMP_OK"
	dumpCheckBad        = "KIPPER_DUMP_BAD"
)

// drillDumpScript checks a restored consistent dump inside the drill. The
// manifest must end with tab-separated "complete" and "true" and contain at
// least one database entry. Each entry must have six tab-separated fields,
// a numeric N.dump filename and exit code 0. Each dump must match its recorded
// SHA-256. Dumps are restored into scratch databases created from template0
// with unused names. Exact per-table row counts must match the recorded counts,
// which are expected to come from the dump's snapshot. Only scratch databases
// created by this script are dropped. Each database gets one result line,
// and checks continue after a database fails. A missing manifest skips the check.
const drillDumpScript = `D=` + drillDumpDir + `
BAD=` + dumpCheckBad + ` OK=` + dumpCheckOK + `
[ -f "$D/manifest" ] || { echo ` + dumpCheckNone + `; exit 0; }
tab=$(printf '\t')
[ "$(tail -n 1 "$D/manifest")" = "complete${tab}true" ] || { echo ` + dumpCheckIncomplete + `; exit 0; }
U="${POSTGRES_USER:-postgres}"
W=$(mktemp -d) || exit 1
cat > "$W/count.sql" <<'SQL'
` + dumpCountSQL + `
SQL
grep "^database${tab}" "$D/manifest" > "$W/databases"
[ -s "$W/databases" ] || { echo ` + dumpCheckIncomplete + `; rm -rf "$W"; exit 0; }
n=0
while IFS= read -r line; do
  n=$((n + 1))
  db=$(printf '%s' "$line" | cut -f2); file=$(printf '%s' "$line" | cut -f3); rc=$(printf '%s' "$line" | cut -f4); sum=$(printf '%s' "$line" | cut -f6)
  if [ "$(printf '%s' "$line" | awk -F'\t' '{print NF}')" != 6 ] || ! printf '%s' "$file" | grep -qx '[0-9][0-9]*\.dump' || [ "$rc" != 0 ]; then
    printf '%s\t%s\t%s\n' ` + dumpCheckBad + ` "${db:-line $n}" "the manifest records no usable dump for this database"; continue
  fi
  i=${file%.dump}
  if [ "$(sha256sum "$D/$file" 2>/dev/null | cut -d' ' -f1)" != "$sum" ]; then
    printf '%s\t%s\t%s\n' ` + dumpCheckBad + ` "$db" "the dump does not match its recorded checksum"; continue
  fi
  scratch="kipper_drill_dump_$$_$i"
  if [ "$(psql -U "$U" -d postgres -XAtq -c "SELECT count(*) FROM pg_database WHERE datname = '$scratch'")" != 0 ]; then
    printf '%s\t%s\t%s\n' ` + dumpCheckBad + ` "$db" "a database named $scratch already exists"; continue
  fi
  psql -U "$U" -d postgres -XAtq -c "CREATE DATABASE $scratch TEMPLATE template0" >/dev/null 2>"$W/err" || {
    printf '%s\t%s\t%s\n' ` + dumpCheckBad + ` "$db" "creating a scratch database failed: $(tail -n 1 "$W/err")"; continue; }
  result=
  if ! pg_restore -U "$U" --no-owner -d "$scratch" "$D/$file" 2>"$W/err"; then
    result="$BAD:pg_restore failed: $(tail -n 1 "$W/err")"
  elif ! PGDATABASE="$scratch" psql -U "$U" -XAtq -v ON_ERROR_STOP=1 -f "$W/count.sql" > "$W/restored" 2>"$W/err"; then
    result="$BAD:counting the restored rows failed: $(tail -n 1 "$W/err")"
  else
    if ! { [ -f "$D/$i.counts" ] && sort "$D/$i.counts" > "$W/want" && sort "$W/restored" > "$W/got"; }; then
      result="$BAD:the row counts recorded with the dump are missing"
    elif cmp -s "$W/want" "$W/got"; then
      result="$OK:$(grep -c . "$W/want")"
    else
      result="$BAD:the restored row counts differ from the counts taken with the dump"
    fi
  fi
  if ! psql -U "$U" -d postgres -XAtq -c "DROP DATABASE $scratch" >/dev/null 2>"$W/err"; then
    result="$BAD:dropping scratch database $scratch failed: $(tail -n 1 "$W/err")"
  fi
  printf '%s\t%s\t%s\n' "${result%%:*}" "$db" "${result#*:}"
done < "$W/databases"
rm -rf "$W"
`

// dumpCheck is one database's result from drillDumpScript.
type dumpCheck struct {
	Database string
	OK       bool
	Detail   string
}

// parseDumpCheck reads drillDumpScript's output. present is false when the
// script reports a missing manifest. An incomplete manifest or unexpected
// output returns an error. Individual database failures are returned in results.
func parseDumpCheck(out string) (results []dumpCheck, present bool, err error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch line {
		case "":
		case dumpCheckNone:
			return nil, false, nil
		case dumpCheckIncomplete:
			return nil, true, errors.New("the consistent dump in this backup is incomplete")
		default:
			fields := strings.SplitN(line, "\t", 3)
			if len(fields) != 3 || (fields[0] != dumpCheckOK && fields[0] != dumpCheckBad) {
				return nil, true, fmt.Errorf("unexpected dump check output: %q", line)
			}
			results = append(results, dumpCheck{Database: fields[1], OK: fields[0] == dumpCheckOK, Detail: fields[2]})
		}
	}
	return results, true, nil
}

// checkConsistentDump restores and verifies the service's consistent dump,
// prints one line per database, and returns an error if execution, output parsing
// or any database check fails. A missing manifest skips verification.
func checkConsistentDump(ctx context.Context, exec podExecFunc, podName string) error {
	var stdout, stderr bytes.Buffer
	if err := exec(ctx, []string{"sh", "-c", drillDumpScript}, nil, &stdout, &stderr); err != nil {
		return fmt.Errorf("checking the consistent dump: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	results, present, err := parseDumpCheck(stdout.String())
	if err != nil || !present {
		return err
	}
	failed := false
	for _, r := range results {
		if r.OK {
			n, _ := strconv.Atoi(r.Detail)
			fmt.Printf("  ✔  %s: dump of %s restored. Row counts match the dump's snapshot (%d %s)\n", podName, r.Database, n, plural(n, "table", "tables"))
			continue
		}
		fmt.Printf("  ✘  %s: dump of %s: %s\n", podName, r.Database, r.Detail)
		failed = true
	}
	if failed {
		return errors.New("a consistent dump did not restore cleanly")
	}
	return nil
}

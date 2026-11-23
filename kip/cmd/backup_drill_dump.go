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
	dumpCheckSkipped    = "KIPPER_DUMP_SKIPPED"
)

// drillDumpScript checks a restored consistent dump inside the drill. The
// manifest must end with tab-separated "complete" and "true" and contain at
// least one database entry. Each entry must have six tab-separated fields,
// a numeric N.dump filename and exit code 0. Each dump must match its recorded
// SHA-256. Dumps are restored into scratch databases created from template0
// with unused names. Exact per-table row counts must match the recorded counts,
// which are expected to come from the dump's snapshot.
//
// To avoid filling the volume and stopping Postgres, the script budgets the
// larger of the restored database's size and the dump file's recorded size for
// the copy. It adds the same amount for the write-ahead log, capped at twice
// max_wal_size when that setting is readable. The cap accounts for checkpoints
// recycling the log. It then adds 64 MiB. The restored database can be smaller
// than the dump if tables shrank after the dump was taken. This budget estimates
// peak usage. If space is insufficient, it checks the dump's table of contents
// with pg_restore and skips the restore. If both sizes are
// unavailable or free space is unknown, it attempts the restore. Only scratch
// databases created by this script are dropped. Each database entry gets one
// result line, and checks continue after a database fails. A missing manifest
// skips the check.
const drillDumpScript = `D=` + drillDumpDir + `
BAD=` + dumpCheckBad + ` OK=` + dumpCheckOK + `
[ -f "$D/manifest" ] || { echo ` + dumpCheckNone + `; exit 0; }
tab=$(printf '\t')
[ "$(tail -n 1 "$D/manifest")" = "complete${tab}true" ] || { echo ` + dumpCheckIncomplete + `; exit 0; }
U="${POSTGRES_USER:-postgres}"
dir=$(psql -U "$U" -d postgres -XAtq -c "SHOW data_directory" 2>/dev/null)
wal=$(psql -U "$U" -d postgres -XAtq -c "SELECT pg_size_bytes(current_setting('max_wal_size'))" 2>/dev/null)
case "$wal" in *[!0-9]* | '') wal= ;; esac
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
  size=$(printf '%s\n' "SELECT pg_database_size(datname) FROM pg_database WHERE datname = :'db'" | psql -U "$U" -d postgres -XAtq -v db="$db" 2>/dev/null)
  dsize=$(printf '%s' "$line" | cut -f5)
  case "$dsize" in *[!0-9]* | '') dsize=0 ;; esac
  case "$size" in
  *[!0-9]* | '') if [ "$dsize" -gt 0 ]; then size=$dsize; else size=; fi ;;
  *) if [ "$dsize" -gt "$size" ]; then size=$dsize; fi ;;
  esac
  kib=$(df -Pk "$dir" 2>/dev/null | awk 'NR == 2 {print $4}')
  case "$size:$kib" in
  *[!0-9:]* | :* | *:) ;;
  *)
    extra=$size
    if [ -n "$wal" ] && [ "$extra" -gt $((2 * wal)) ]; then extra=$((2 * wal)); fi
    need=$((size + extra + 67108864)); free=$((kib * 1024))
    if [ "$need" -gt "$free" ]; then
      if pg_restore --list "$D/$file" >/dev/null 2>"$W/err"; then
        printf '%s\t%s\t%s %s\n' ` + dumpCheckSkipped + ` "$db" "$need" "$free"
      else
        printf '%s\t%s\t%s\n' ` + dumpCheckBad + ` "$db" "pg_restore cannot read the dump: $(tail -n 1 "$W/err")"
      fi
      continue
    fi ;;
  esac
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

// dumpCheck is one database's result from drillDumpScript. A skipped restore
// has OK set to true. Need and Free are the estimated required and available
// space in bytes for a skipped restore.
type dumpCheck struct {
	Database   string
	OK         bool
	Detail     string
	Skipped    bool
	Need, Free int64
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
			if len(fields) == 3 && fields[0] == dumpCheckSkipped {
				need, free, ok := parseSkippedRestore(fields[2])
				if !ok || fields[1] == "" {
					return nil, true, fmt.Errorf("unexpected dump check output: %q", line)
				}
				results = append(results, dumpCheck{Database: fields[1], OK: true, Skipped: true, Need: need, Free: free})
				continue
			}
			if len(fields) != 3 || (fields[0] != dumpCheckOK && fields[0] != dumpCheckBad) {
				return nil, true, fmt.Errorf("unexpected dump check output: %q", line)
			}
			results = append(results, dumpCheck{Database: fields[1], OK: fields[0] == dumpCheckOK, Detail: fields[2]})
		}
	}
	return results, true, nil
}

// parseSkippedRestore reads a skipped restore's "need free" byte counts. It
// accepts two nonnegative int64 decimal values separated by a single space.
// need must exceed free, matching drillDumpScript's insufficient-space check.
func parseSkippedRestore(detail string) (need, free int64, ok bool) {
	needText, freeText, found := strings.Cut(detail, " ")
	if !found || !isDecimal(needText) || !isDecimal(freeText) {
		return 0, 0, false
	}
	need, errNeed := strconv.ParseInt(needText, 10, 64)
	free, errFree := strconv.ParseInt(freeText, 10, 64)
	if errNeed != nil || errFree != nil || need <= free {
		return 0, 0, false
	}
	return need, free, true
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// checkConsistentDump restores and verifies the service's consistent dump,
// prints one line per database, and returns an error if execution, output parsing
// or any database check fails. If the manifest is missing, it skips dump
// verification and reports that only the volume copy was checked. Restores
// skipped for insufficient space are reported without failing the check.
func checkConsistentDump(ctx context.Context, exec podExecFunc, podName string) error {
	var stdout, stderr bytes.Buffer
	if err := exec(ctx, []string{"sh", "-c", drillDumpScript}, nil, &stdout, &stderr); err != nil {
		return fmt.Errorf("checking the consistent dump: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	results, present, err := parseDumpCheck(stdout.String())
	if err != nil {
		return err
	}
	if !present {
		fmt.Printf("  ·  %s: the backup contains no consistent dump, so the drill checked only the volume copy\n", podName)
		return nil
	}
	failed := false
	for _, r := range results {
		if r.Skipped {
			fmt.Printf("  ⚠  %s: dump of %s: checksum matches and its table of contents is readable. The drill skipped the test restore because the restored volume has %s free and the restore needs about %s. Use a larger volume for the service to include this check.\n",
				podName, r.Database, humanBytes(r.Free), humanBytes(r.Need))
			continue
		}
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

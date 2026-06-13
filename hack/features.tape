# pgbranch v0.3.0 feature tour. Records the CLI against a running branchd.
# Endpoints/secrets come from the recorder's environment (not this file):
#   pgb  → PGBRANCH_SERVER, PGBRANCH_TOKEN
#   psql → PGHOST, PGPORT, PGUSER, PGPASSWORD, PGDATABASE=postgres@demo
# Record with:
#   PGBRANCH_SERVER=… PGBRANCH_TOKEN=… PGHOST=… PGPORT=6432 PGUSER=postgres \
#   PGPASSWORD=… PGDATABASE=postgres@demo PATH=/opt/homebrew/opt/libpq/bin:$PATH \
#   vhs hack/features.tape
Output docs/features.gif
Set Shell bash
Set FontSize 24
Set Width 1600
Set Height 860
Set Padding 24
Set Framerate 24
Set TypingSpeed 24ms
Set Theme "Catppuccin Mocha"
Set WindowBar Colorful
Set BorderRadius 8
Set WaitTimeout 90s

Hide
Type "alias pgb='/Users/basit/projects/pgbranch/bin/pgb'; export PS1='$ '"
Enter
Type "pgb branch destroy exp >/dev/null 2>&1; pgb branch destroy demo >/dev/null 2>&1; clear"
Enter
Show

Type "# pgbranch — git branch for Postgres. One seeded source:"
Enter
Sleep 1500ms
Type "pgb source ls"
Enter
Sleep 2500ms

Type "# Branch it: a full, writable, PII-masked copy of prod —"
Enter
Sleep 800ms
Type "pgb branch create demo --from prod"
Enter
Sleep 9s

Type "# Apply this PR's migration to the branch:"
Enter
Sleep 800ms
Type "psql -q -f /tmp/demo-mig.sql"
Enter
Sleep 3s

Type "# See exactly what changed vs prod — schema + row deltas:"
Enter
Sleep 800ms
Type "pgb diff demo"
Enter
Sleep 13s

Type "# Branch off a branch, instantly:"
Enter
Sleep 600ms
Type "pgb branch create exp --from-branch demo"
Enter
Sleep 18s

Type "pgb branch ls"
Enter
Sleep 4s
Sleep 1500ms

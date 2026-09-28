# pgoverlay feature tour, recorded with vhs (https://github.com/charmbracelet/vhs)
# against a running branchd (server mode, docker runtime); timings are not staged.
# Fixtures live in hack/features/: prod.sql (a synthetic prod with fake PII),
# mask.sql (the masking script) and 0003_add_shipments.sql (the PR's migration).
#
# Prep (outside the recording), on a Linux host with a local Docker engine:
#   make build
#   docker run -d --name pgoverlay-tour-src -e POSTGRES_PASSWORD=secret postgres:17 \
#     -c wal_level=replica -c max_wal_senders=4
#   docker exec pgoverlay-tour-src sh -c \
#     'echo "host replication all all scram-sha-256" >> "$PGDATA/pg_hba.conf"'
#   docker exec pgoverlay-tour-src psql -U postgres -c "SELECT pg_reload_conf();"
#   docker exec -i pgoverlay-tour-src psql -U postgres -v ON_ERROR_STOP=1 < hack/features/prod.sql
#   export PGOVERLAY_TOKEN=$(openssl rand -hex 24) PGOVERLAY_SERVER=http://127.0.0.1:7070
#   PGOVERLAY_HOME=/tmp/pgoverlay-tour-home ./bin/branchd &
#   SRC_IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' \
#     pgoverlay-tour-src)
#   PGPASSWORD=secret ./bin/pgb source add prod --host "$SRC_IP" --user postgres
#   ./bin/pgb source set-mask prod hack/features/mask.sql
# Record, from the repo root (psql connects through branchd's router, so the
# database is "postgres@demo"; any psql on PATH works):
#   PGHOST=127.0.0.1 PGPORT=6432 PGUSER=postgres PGPASSWORD=secret \
#   PGDATABASE=postgres@demo vhs hack/features.tape
# (Ubuntu 24.04+ blocks Chromium's sandbox: run VHS_NO_SANDBOX=true vhs ...)
#
# Clean up afterwards: pgb branch destroy exp demo; pgb source rm prod; stop
# branchd; docker rm -f -v pgoverlay-tour-src; rm -rf /tmp/pgoverlay-tour-home

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
Type `export PATH="$PWD/bin:$PATH" && cd hack/features`
Enter
Wait
Type "pgb branch destroy exp >/dev/null 2>&1; pgb branch destroy demo >/dev/null 2>&1; clear"
Enter
Wait
Show

Type "# pgoverlay: git branch for Postgres. One source, seeded from prod:"
Enter
Sleep 800ms
Type "pgb source ls"
Enter
Wait
Sleep 2s

Type "# Branch it: a full, writable copy of prod, PII masked on the way:"
Enter
Sleep 800ms
Type "pgb branch create demo --from prod"
Enter
Wait
Sleep 2s

Type "# Connect through the router (database postgres@demo):"
Enter
Sleep 600ms
Type `psql -c "SELECT id, name, email FROM customers ORDER BY id LIMIT 3"`
Enter
Wait
Sleep 3s

Type "# Apply this PR's migration to the branch:"
Enter
Sleep 600ms
Type "psql -q -f 0003_add_shipments.sql"
Enter
Wait
Sleep 1500ms

Type "# See what changed vs prod: schema diff + row deltas:"
Enter
Sleep 600ms
Type "pgb diff demo"
Enter
Wait
Sleep 5s

# On the overlay backend branching a branch freezes the parent: demo is
# checkpointed and restarted on a new port, so this takes longer than a
# create from the source. Keep the caption honest about that.
Type "# Branch off a branch (demo is frozen and restarted, on a new port):"
Enter
Sleep 600ms
Type "pgb branch create exp --from-branch demo"
Enter
Wait
Sleep 2s

Type "pgb branch ls"
Enter
Wait
Sleep 4s

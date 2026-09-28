# pgoverlay README demo, recorded with vhs (https://github.com/charmbracelet/vhs)
# against a REAL 1 GiB pgbench database (pgbench -i -s 68); timings are not staged.
# Record on a Linux host with a local Docker engine (local-mode pgb runs the
# branch containers there).
#
# Prep (outside the recording):
#   docker run -d --name pgoverlay-demo-src -e POSTGRES_PASSWORD=secret postgres:17 \
#     -c wal_level=replica -c max_wal_senders=4
#   docker exec pgoverlay-demo-src sh -c \
#     'echo "host replication all all scram-sha-256" >> "$PGDATA/pg_hba.conf"'
#   docker exec pgoverlay-demo-src psql -U postgres -c "SELECT pg_reload_conf();"
#   docker exec pgoverlay-demo-src pgbench -i -s 68 -U postgres postgres
#   SRC_IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' \
#     pgoverlay-demo-src)
#   mkdir -p /tmp/pgoverlay-demo-home
#   PGOVERLAY_HOME=/tmp/pgoverlay-demo-home PGPASSWORD=secret \
#     ./bin/pgb source add main --host "$SRC_IP" --user postgres
# Then, from the repo root:  make build && vhs hack/demo.tape
# (Ubuntu 24.04+ blocks Chromium's sandbox: run VHS_NO_SANDBOX=true vhs ...)
#
# Clean up afterwards:
#   PGOVERLAY_HOME=/tmp/pgoverlay-demo-home ./bin/pgb branch destroy pr-42
#   PGOVERLAY_HOME=/tmp/pgoverlay-demo-home ./bin/pgb source rm main
#   docker rm -f -v pgoverlay-demo-src && rm -rf /tmp/pgoverlay-demo-home

Output docs/demo.gif

Set Shell bash
Set FontSize 26
Set Width 1600
Set Height 820
Set Padding 24
Set Framerate 24
Set TypingSpeed 28ms
Set Theme "Catppuccin Mocha"
Set WindowBar Colorful
Set BorderRadius 8
Set WaitTimeout 60s

Env PGOVERLAY_HOME "/tmp/pgoverlay-demo-home"
Env PGPASSWORD "secret"

Hide
Type `export PATH="$PWD/bin:$PATH" && clear`
Enter
Wait
Show

# The source was seeded once from a running Postgres (see README quickstart).
Type "pgb source ls"
Enter
Wait
Sleep 1s

Type `docker exec pgoverlay-demo-src psql -U postgres -c "SELECT count(*) FROM pgbench_accounts"`
Enter
Wait
Sleep 1s

# Hero moment: branch the 1 GiB database.
Type "pgb branch create pr-42 --from main"
Enter
Wait
Sleep 2.5s

# The whole dataset is there.
Type `docker exec pgoverlay-br-pr-42 psql -U postgres -tc "\dt+"`
Enter
Wait
Sleep 2.2s

# Branches are writable.
Type `docker exec pgoverlay-br-pr-42 psql -U postgres -c "DELETE FROM pgbench_tellers"`
Enter
Wait
Sleep 1s

Type "# source is untouched:"
Enter
Type `docker exec pgoverlay-demo-src psql -U postgres -c "SELECT count(*) FROM pgbench_tellers"`
Enter
Wait
Sleep 2s

# Branch a running branch. On the overlay backend this freezes the parent:
# pr-42 is checkpointed and restarted (on a new port), so it takes longer
# than a create from the source.
Type "# branch a branch (the parent pr-42 is frozen and restarted):"
Enter
Type "pgb branch create pr-42-fix --from-branch pr-42"
Enter
Wait
Sleep 1.5s

Type "pgb branch ls"
Enter
Wait
Sleep 2.2s

Type "pgb branch destroy pr-42-fix"
Enter
Wait
Sleep 2s

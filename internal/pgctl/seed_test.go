package pgctl

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

// StartSourcePG starts a "production" postgres on a dedicated docker network
// and returns its in-network host, port, network name, and a host connection string.
func StartSourcePG(t *testing.T, ctx context.Context) (host string, port int, networkName string, hostConn string) {
	t.Helper()
	// testcontainers-go resolves the docker host from DOCKER_HOST or
	// /var/run/docker.sock but not from docker CLI contexts (Colima).
	if os.Getenv("DOCKER_HOST") == "" {
		if h := runtime.DockerHostFromCLIContext(); h != "" {
			os.Setenv("DOCKER_HOST", h)
		}
	}
	net, err := network.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { net.Remove(context.Background()) })
	req := tc.ContainerRequest{
		Image:          "postgres:17",
		Env:            map[string]string{"POSTGRES_PASSWORD": "secret"},
		Networks:       []string{net.Name},
		NetworkAliases: map[string][]string{net.Name: {"sourcedb"}},
		ExposedPorts:   []string{"5432/tcp"},
		Cmd:            []string{"-c", "wal_level=replica", "-c", "max_wal_senders=4"},
		// The stock image's pg_hba.conf has no remote "replication" entry
		// (its catch-all "host all all all" doesn't match replication
		// connections), so pg_basebackup from a sibling container is
		// rejected. Append one via an initdb script.
		Files: []tc.ContainerFile{{
			Reader:            strings.NewReader("#!/bin/sh\necho 'host replication all all scram-sha-256' >> \"$PGDATA/pg_hba.conf\"\n"),
			ContainerFilePath: "/docker-entrypoint-initdb.d/zz-replication.sh",
			FileMode:          0o755,
		}},
		WaitingFor: wait.ForListeningPort("5432/tcp").WithStartupTimeout(60 * time.Second),
	}
	c, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{ContainerRequest: req, Started: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Terminate(context.Background()) })
	mp, err := c.MappedPort(ctx, "5432")
	if err != nil {
		t.Fatal(err)
	}
	return "sourcedb", 5432, net.Name, fmt.Sprintf("postgres://postgres:secret@localhost:%d/postgres", mp.Num())
}

func TestSeedFromRunningPostgres(t *testing.T) {
	if os.Getenv("PGBRANCH_IT") != "1" {
		t.Skip("set PGBRANCH_IT=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	host, port, networkName, hostConn := StartSourcePG(t, ctx)

	conn, err := pgx.Connect(ctx, hostConn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `CREATE TABLE t(i int); INSERT INTO t SELECT generate_series(1,1000)`); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)

	d, err := runtime.NewDockerDriver()
	if err != nil {
		t.Fatal(err)
	}
	vol := "pgbranch-test-seed"
	if err := d.CreateVolume(ctx, vol, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.RemoveVolume(context.Background(), vol) })

	err = Seed(ctx, d, SeedSpec{
		Image: "postgres:17", Volume: vol, Network: networkName,
		Host: host, Port: port, User: "postgres", Password: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	// PG_VERSION present => valid cluster layout
	if err := d.RunHelper(ctx, runtime.HelperSpec{
		Image: "alpine:3.21", Cmd: []string{"test", "-f", "/seed/data/PG_VERSION"},
		Mounts: []runtime.Mount{{Volume: vol, Target: "/seed", ReadOnly: true}},
	}); err != nil {
		t.Fatalf("seed volume missing PG_VERSION: %v", err)
	}
}

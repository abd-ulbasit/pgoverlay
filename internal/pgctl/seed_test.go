package pgctl

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

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
	if _, err := d.RunHelper(ctx, runtime.HelperSpec{
		Image: "alpine:3.21", Cmd: []string{"test", "-f", "/seed/data/PG_VERSION"},
		Mounts: []runtime.Mount{{Volume: vol, Target: "/seed", ReadOnly: true}},
	}); err != nil {
		t.Fatalf("seed volume missing PG_VERSION: %v", err)
	}
}

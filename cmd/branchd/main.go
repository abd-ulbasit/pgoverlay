// branchd is pgbranch's daemon: REST control plane (--api-addr) and Postgres
// wire-protocol router (--pg-addr) over one shared engine/registry, plus a
// TTL reaper. Auth is a single bearer token from PGBRANCH_TOKEN (required).
//
// Shutdown (SIGINT/SIGTERM) is graceful: listeners close, in-flight requests
// finish, and branch containers keep running — they are durable state.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/abd-ulbasit/pgbranch/internal/api"
	"github.com/abd-ulbasit/pgbranch/internal/config"
	"github.com/abd-ulbasit/pgbranch/internal/engine"
	"github.com/abd-ulbasit/pgbranch/internal/pgproxy"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
	"github.com/abd-ulbasit/pgbranch/internal/runtime"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	apiAddr := flag.String("api-addr", ":7070", "REST API listen address")
	pgAddr := flag.String("pg-addr", ":6432", "Postgres router listen address")
	reapInterval := flag.Duration("reap-interval", 30*time.Second, "TTL reaper tick interval")
	flag.Parse()

	token := os.Getenv("PGBRANCH_TOKEN")
	if token == "" {
		return errors.New("PGBRANCH_TOKEN must be set (bearer token for the REST API)")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.EnsureHome(); err != nil {
		return err
	}
	reg, err := registry.Open(cfg.RegistryPath)
	if err != nil {
		return err
	}
	defer reg.Close()
	drv, err := runtime.NewDockerDriver()
	if err != nil {
		return err
	}
	eng := engine.New(reg, drv, cfg.PostgresImage)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := eng.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)

	// REST API
	srv := &http.Server{Addr: *apiAddr, Handler: api.New(eng, reg, token).Handler()}
	g.Go(func() error {
		log.Printf("REST API listening on %s", *apiAddr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shCtx)
	})

	// Postgres wire-protocol router (Serve closes the listener on ctx done)
	lis, err := net.Listen("tcp", *pgAddr)
	if err != nil {
		return fmt.Errorf("pg router listen: %w", err)
	}
	g.Go(func() error {
		log.Printf("pg router listening on %s (connect with dbname@branch)", *pgAddr)
		return pgproxy.New(&pgproxy.RegistryResolver{Reg: reg}).Serve(ctx, lis)
	})

	// TTL reaper
	g.Go(func() error {
		eng.RunReaper(ctx, *reapInterval, log.Printf)
		return nil
	})

	err = g.Wait()
	log.Println("branchd stopped (branch containers keep running)")
	return err
}

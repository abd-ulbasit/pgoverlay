// branchd is pgbranch's daemon: REST control plane (--api-addr) and Postgres
// wire-protocol router (--pg-addr) over one shared engine/registry, plus a
// TTL reaper. Auth is a single bearer token from PGBRANCH_TOKEN (required).
//
// Shutdown (SIGINT/SIGTERM) is graceful: listeners close, in-flight requests
// finish, and branch containers keep running — they are durable state.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/abd-ulbasit/pgbranch/internal/api"
	"github.com/abd-ulbasit/pgbranch/internal/config"
	"github.com/abd-ulbasit/pgbranch/internal/cow"
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

// uiURL renders a clickable address for the embedded web UI from the API
// listen address (":7070" -> "http://localhost:7070/ui/").
func uiURL(apiAddr string, tlsEnabled bool) string {
	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(apiAddr)
	if err != nil {
		return scheme + "://" + apiAddr + "/ui/"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return fmt.Sprintf("%s://%s/ui/", scheme, net.JoinHostPort(host, port))
}

// tlsConfigFromFlags loads an optional PEM cert/key flag pair (--<name>-tls-cert
// / --<name>-tls-key). Both empty = TLS off (nil config); one without the
// other is a startup error.
func tlsConfigFromFlags(certFile, keyFile, name string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("--%s-tls-cert and --%s-tls-key must be set together", name, name)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load --%s-tls-cert/--%s-tls-key: %w", name, name, err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

func run() error {
	apiAddr := flag.String("api-addr", ":7070", "REST API listen address")
	pgAddr := flag.String("pg-addr", ":6432", "Postgres router listen address")
	reapInterval := flag.Duration("reap-interval", 30*time.Second, "TTL reaper tick interval")
	runtimeName := flag.String("runtime", "docker", "container runtime: docker or kube")
	kubeNamespace := flag.String("kube-namespace", "", `namespace for branch/helper pods (default: POD_NAMESPACE when in-cluster, else "pgbranch")`)
	kubeNode := flag.String("kube-node", "", "storage node name (required with --runtime kube; all CoW data lives on this node)")
	kubeDataRoot := flag.String("kube-data-root", "/var/lib/pgbranch", "CoW data root on the storage node")
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig path (default: in-cluster config, then KUBECONFIG / ~/.kube/config)")
	cowBackend := flag.String("cow", string(cow.BackendOverlay), "copy-on-write backend: overlay (default) or zfs (experimental, see docs/zfs.md)")
	zfsDataset := flag.String("zfs-dataset", "", "dataset prefix holding all pgbranch datasets, e.g. tank/pgbranch (required with --cow zfs)")
	apiTLSCert := flag.String("api-tls-cert", "", "PEM certificate for the REST API (TLS off when unset; requires --api-tls-key)")
	apiTLSKey := flag.String("api-tls-key", "", "PEM private key for the REST API (requires --api-tls-cert)")
	pgTLSCert := flag.String("pg-tls-cert", "", "PEM certificate for the Postgres router (SSLRequest answered 'N' when unset; requires --pg-tls-key)")
	pgTLSKey := flag.String("pg-tls-key", "", "PEM private key for the Postgres router (requires --pg-tls-cert)")
	flag.Parse()

	apiTLS, err := tlsConfigFromFlags(*apiTLSCert, *apiTLSKey, "api")
	if err != nil {
		return err
	}
	pgTLS, err := tlsConfigFromFlags(*pgTLSCert, *pgTLSKey, "pg")
	if err != nil {
		return err
	}

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
	var drv runtime.Driver
	switch *runtimeName {
	case "docker":
		drv, err = runtime.NewDockerDriver()
	case "kube":
		if *kubeNode == "" {
			return errors.New("--kube-node is required with --runtime kube (the node that stores all branch data)")
		}
		ns := *kubeNamespace
		if ns == "" {
			ns = os.Getenv("POD_NAMESPACE")
		}
		if ns == "" {
			ns = "pgbranch"
		}
		drv, err = runtime.NewKubeDriver(*kubeconfig, ns, *kubeNode, *kubeDataRoot)
	default:
		return fmt.Errorf("unknown --runtime %q (want docker or kube)", *runtimeName)
	}
	if err != nil {
		return fmt.Errorf("init %s runtime: %w", *runtimeName, err)
	}
	backend, err := cow.ParseBackend(*cowBackend)
	if err != nil {
		return err
	}
	if backend == cow.BackendZFS && *zfsDataset == "" {
		return errors.New("--zfs-dataset is required with --cow zfs (the dataset prefix pgbranch owns, e.g. tank/pgbranch)")
	}
	eng := engine.NewWithPlanner(reg, drv, cfg.PostgresImage,
		cow.Planner{Backend: backend, Dataset: strings.Trim(*zfsDataset, "/")})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := eng.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}

	g, ctx := errgroup.WithContext(ctx)

	// REST API (plain listener, wrapped with TLS when --api-tls-* is set)
	apiLis, err := net.Listen("tcp", *apiAddr)
	if err != nil {
		return fmt.Errorf("api listen: %w", err)
	}
	if apiTLS != nil {
		apiLis = tls.NewListener(apiLis, apiTLS)
	}
	srv := &http.Server{Addr: *apiAddr, Handler: api.New(eng, reg, token).Handler()}
	g.Go(func() error {
		log.Printf("REST API listening on %s (TLS %v)", *apiAddr, apiTLS != nil)
		log.Printf("web UI at %s", uiURL(*apiAddr, apiTLS != nil))
		if err := srv.Serve(apiLis); !errors.Is(err, http.ErrServerClosed) {
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
		log.Printf("pg router listening on %s (connect with dbname@branch; TLS %v)", *pgAddr, pgTLS != nil)
		px := pgproxy.New(&pgproxy.RegistryResolver{Reg: reg})
		px.TLSConfig = pgTLS
		return px.Serve(ctx, lis)
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

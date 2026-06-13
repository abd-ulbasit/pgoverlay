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
	"github.com/abd-ulbasit/pgbranch/internal/metrics"
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

// storageOptions captures the --runtime/--kube-storage/--cow flag triangle
// plus the csi-only flags.
type storageOptions struct {
	runtime       string // docker | kube
	kubeStorage   string // hostpath | csi
	cowFlag       string // --cow value
	cowSet        bool   // --cow explicitly passed
	storageClass  string // --csi-storage-class
	snapshotClass string // --csi-snapshot-class
	volumeSize    string // --csi-volume-size
	kubeNode      string // --kube-node
}

// resolveStorage validates the storage flag combination and returns the
// effective cow backend. --kube-storage csi FORCES the csi backend (no
// separate --cow needed); every invalid combination is a startup error.
func resolveStorage(o storageOptions) (cow.Backend, error) {
	if o.runtime != "docker" && o.runtime != "kube" {
		return "", fmt.Errorf("unknown --runtime %q (want docker or kube)", o.runtime)
	}
	backend, err := cow.ParseBackend(o.cowFlag)
	if err != nil {
		return "", err
	}
	switch o.kubeStorage {
	case "csi":
		if o.runtime != "kube" {
			return "", errors.New("--kube-storage csi requires --runtime kube")
		}
		if o.storageClass == "" {
			return "", errors.New("--csi-storage-class is required with --kube-storage csi (a StorageClass whose CSI driver supports PVC cloning, or snapshots with --csi-snapshot-class)")
		}
		if o.cowSet && backend != cow.BackendCSI {
			return "", fmt.Errorf("--kube-storage csi forces --cow csi (got --cow %s)", backend)
		}
		return cow.BackendCSI, nil
	case "hostpath":
		if backend == cow.BackendCSI {
			return "", errors.New("--cow csi requires --runtime kube --kube-storage csi")
		}
		if o.storageClass != "" || o.snapshotClass != "" || o.volumeSize != "" {
			return "", errors.New("--csi-storage-class, --csi-snapshot-class and --csi-volume-size require --kube-storage csi")
		}
		if o.runtime == "kube" && o.kubeNode == "" {
			return "", errors.New("--kube-node is required with --runtime kube --kube-storage hostpath (the node that stores all branch data)")
		}
		return backend, nil
	default:
		return "", fmt.Errorf("unknown --kube-storage %q (want hostpath or csi)", o.kubeStorage)
	}
}

func run() error {
	apiAddr := flag.String("api-addr", ":7070", "REST API listen address")
	pgAddr := flag.String("pg-addr", ":6432", "Postgres router listen address")
	reconcileInterval := flag.Duration("reconcile-interval", 60*time.Second, "reconcile loop tick interval (TTL reap + leak GC + drift convergence)")
	reapInterval := flag.Duration("reap-interval", 0, "DEPRECATED alias for --reconcile-interval (folded into the unified reconcile loop)")
	stuckTimeout := flag.Duration("stuck-timeout", 10*time.Minute, "age past which a creating/resetting branch row is considered stuck and failed by reconcile")
	runtimeName := flag.String("runtime", "docker", "container runtime: docker or kube")
	kubeNamespace := flag.String("kube-namespace", "", `namespace for branch/helper pods (default: POD_NAMESPACE when in-cluster, else "pgbranch")`)
	kubeNode := flag.String("kube-node", "", "storage node name (required with --runtime kube --kube-storage hostpath; all CoW data lives on this node)")
	kubeDataRoot := flag.String("kube-data-root", "/var/lib/pgbranch", "CoW data root on the storage node (hostpath storage only)")
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig path (default: in-cluster config, then KUBECONFIG / ~/.kube/config)")
	kubeStorage := flag.String("kube-storage", "hostpath", "kube storage mode: hostpath (single node, data under --kube-data-root) or csi (multi-node, PVC clones; see docs/kubernetes.md)")
	csiStorageClass := flag.String("csi-storage-class", "", "StorageClass for pgbranch PVCs (required with --kube-storage csi; its CSI driver must support PVC cloning, or snapshots with --csi-snapshot-class)")
	csiSnapshotClass := flag.String("csi-snapshot-class", "", "VolumeSnapshotClass: clone branches via VolumeSnapshot+restore instead of direct PVC clones (--kube-storage csi only)")
	csiVolumeSize := flag.String("csi-volume-size", "", "size of every pgbranch PVC, e.g. 50Gi (default 10Gi; --kube-storage csi only)")
	cowBackend := flag.String("cow", string(cow.BackendOverlay), "copy-on-write backend: overlay (default), zfs (experimental, see docs/zfs.md) or csi (forced by --kube-storage csi)")
	zfsDataset := flag.String("zfs-dataset", "", "dataset prefix holding all pgbranch datasets, e.g. tank/pgbranch (required with --cow zfs)")
	rotateCreds := flag.Bool("rotate-branch-credentials", false, "give every branch its own generated password instead of inheriting the source's (returned as `password` in branch API responses; see docs/architecture.md)")
	apiTLSCert := flag.String("api-tls-cert", "", "PEM certificate for the REST API (TLS off when unset; requires --api-tls-key)")
	apiTLSKey := flag.String("api-tls-key", "", "PEM private key for the REST API (requires --api-tls-cert)")
	pgTLSCert := flag.String("pg-tls-cert", "", "PEM certificate for the Postgres router (SSLRequest answered 'N' when unset; requires --pg-tls-key)")
	pgTLSKey := flag.String("pg-tls-key", "", "PEM private key for the Postgres router (requires --pg-tls-cert)")
	flag.Parse()

	// --reap-interval is a deprecated alias: when set (non-zero) it folds into
	// the single reconcile loop's interval. We never run two loops.
	if *reapInterval > 0 {
		log.Printf("warning: --reap-interval is deprecated; using its value (%s) as --reconcile-interval", *reapInterval)
		*reconcileInterval = *reapInterval
	}

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
	cowSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "cow" {
			cowSet = true
		}
	})
	backend, err := resolveStorage(storageOptions{
		runtime: *runtimeName, kubeStorage: *kubeStorage, cowFlag: *cowBackend, cowSet: cowSet,
		storageClass: *csiStorageClass, snapshotClass: *csiSnapshotClass, volumeSize: *csiVolumeSize,
		kubeNode: *kubeNode,
	})
	if err != nil {
		return err
	}
	if backend == cow.BackendZFS && *zfsDataset == "" {
		return errors.New("--zfs-dataset is required with --cow zfs (the dataset prefix pgbranch owns, e.g. tank/pgbranch)")
	}
	var drv runtime.Driver
	switch *runtimeName {
	case "docker":
		drv, err = runtime.NewDockerDriver()
	case "kube":
		ns := *kubeNamespace
		if ns == "" {
			ns = os.Getenv("POD_NAMESPACE")
		}
		if ns == "" {
			ns = "pgbranch"
		}
		if backend == cow.BackendCSI {
			drv, err = runtime.NewKubeDriverCSI(*kubeconfig, ns, runtime.CSIConfig{
				StorageClass: *csiStorageClass, SnapshotClass: *csiSnapshotClass, VolumeSize: *csiVolumeSize,
			})
		} else {
			drv, err = runtime.NewKubeDriver(*kubeconfig, ns, *kubeNode, *kubeDataRoot)
		}
	}
	if err != nil {
		return fmt.Errorf("init %s runtime: %w", *runtimeName, err)
	}
	m := metrics.New()
	m.SetStateCounter(reg)
	engOpts := []engine.Option{engine.WithMetrics(m)}
	if *rotateCreds {
		engOpts = append(engOpts, engine.WithCredentialRotation())
	}
	eng := engine.NewWithPlanner(reg, drv, cfg.PostgresImage,
		cow.Planner{Backend: backend, Dataset: strings.Trim(*zfsDataset, "/")}, engOpts...)

	// readiness: the registry is reachable (trivial query) and the driver
	// responds (cheap ListManaged). branchd's liveness stays /healthz.
	ready := func(ctx context.Context) error {
		if err := reg.Ping(ctx); err != nil {
			return fmt.Errorf("registry unreachable: %w", err)
		}
		if _, err := drv.ListManaged(ctx); err != nil {
			return fmt.Errorf("driver unresponsive: %w", err)
		}
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	g, ctx := errgroup.WithContext(ctx)

	// REST API (plain listener, wrapped with TLS when --api-tls-* is set)
	apiLis, err := net.Listen("tcp", *apiAddr)
	if err != nil {
		return fmt.Errorf("api listen: %w", err)
	}
	if apiTLS != nil {
		apiLis = tls.NewListener(apiLis, apiTLS)
	}
	srv := &http.Server{Addr: *apiAddr, Handler: api.New(eng, reg, token, m.Handler(), ready, *stuckTimeout).Handler()}
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

	// unified reconcile loop: TTL reap + stuck-row failure + orphan-container
	// removal + dangling layer/volume GC, on one ticker (runs once immediately
	// so startup drift converges without waiting a full interval).
	g.Go(func() error {
		eng.RunReconcile(ctx, *reconcileInterval, *stuckTimeout, log.Printf)
		return nil
	})

	err = g.Wait()
	log.Println("branchd stopped (branch containers keep running)")
	return err
}

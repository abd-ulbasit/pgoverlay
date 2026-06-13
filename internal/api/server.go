// Package api is branchd's REST control plane: a thin JSON layer over the
// engine. stdlib net/http only; routes use Go 1.22 method patterns. All /v1
// routes require the bearer token; /healthz does not.
package api

import (
	"context"
	"net/http"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/engine"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

// Wire types. The password in CreateSourceRequest/RefreshSourceRequest is
// used for the seed connection only — never stored, never returned.

type Source struct {
	Name      string `json:"name"`
	PGVersion string `json:"pg_version"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	User      string `json:"user"`
	Database  string `json:"database"`
	Network   string `json:"network,omitempty"`
	// Via is the seeding method: "basebackup" (pg_basebackup) or "dump"
	// (pg_dump — managed Postgres without REPLICATION privilege).
	Via         string   `json:"via"`
	DumpSchemas []string `json:"dump_schemas,omitempty"`
	State       string   `json:"state"`
	Generation  int      `json:"generation"`
	CreatedAt   string   `json:"created_at"`
}

type Branch struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	// Parent is the branch this one was created from (branch-from-branch);
	// "" when created directly from the source.
	Parent string `json:"parent,omitempty"`
	State  string `json:"state"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	User   string `json:"user"`
	// Password is the branch's own rotated password — present only when
	// branchd runs with --rotate-branch-credentials; otherwise the branch
	// inherits the source's credentials and the field is omitted.
	Password string `json:"password,omitempty"`
	Database string `json:"database"`
	// ProxyDatabase is the database param to use when connecting through the
	// wire-protocol router: dbname@branch.
	ProxyDatabase string `json:"proxy_database"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	CreatedAt     string `json:"created_at"`
}

type CreateSourceRequest struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	User      string `json:"user"`
	Database  string `json:"database"`
	Network   string `json:"network"`
	PGVersion string `json:"pg_version"`
	Password  string `json:"password"`
	// Via selects the seeding method: "basebackup" (default) or "dump".
	Via string `json:"via,omitempty"`
	// DumpSchemas scopes a via=dump seed to the given schemas (empty = the
	// whole database). Only valid with via=dump.
	DumpSchemas []string `json:"dump_schemas,omitempty"`
}

// CreateBranchRequest creates a branch off a source (Source) or off another
// branch (Parent) — exactly one of the two must be set.
type CreateBranchRequest struct {
	Name       string `json:"name"`
	Source     string `json:"source,omitempty"`
	Parent     string `json:"parent,omitempty"`
	TTLSeconds int    `json:"ttl_seconds"`
}

type RefreshSourceRequest struct {
	Password string `json:"password"`
}

// MaskScript is one per-source masking statement, applied in order inside
// every new/reset branch before it is marked ready.
type MaskScript struct {
	Name string `json:"name"`
	SQL  string `json:"sql"`
}

// Ready reports whether branchd can serve traffic: the registry is reachable
// and the container driver responds. Returns nil when ready, an error
// otherwise. branchd supplies a closure; tests inject a fake.
type Ready func(ctx context.Context) error

type Server struct {
	eng          *engine.Engine
	reg          *registry.Registry
	token        string
	metrics      http.Handler
	ready        Ready
	stuckTimeout time.Duration
}

// New builds the API server. metricsHandler serves /metrics (promhttp over the
// metrics registry) and ready backs /readyz; both may be nil (then /metrics
// 404s and /readyz reports ready iff the handler is wired). branchd always
// passes both. stuckTimeout is the reconcile cutoff for stuck creating/
// resetting rows (0 → DefaultStuckTimeout).
func New(eng *engine.Engine, reg *registry.Registry, token string, metricsHandler http.Handler, ready Ready, stuckTimeout time.Duration) *Server {
	if stuckTimeout <= 0 {
		stuckTimeout = DefaultStuckTimeout
	}
	return &Server{eng: eng, reg: reg, token: token, metrics: metricsHandler, ready: ready, stuckTimeout: stuckTimeout}
}

// DefaultStuckTimeout is the fallback cutoff for reconcile's stuck-row pass.
const DefaultStuckTimeout = 10 * time.Minute

func (s *Server) Handler() http.Handler {
	v1 := http.NewServeMux()
	v1.HandleFunc("POST /v1/sources", s.createSource)
	v1.HandleFunc("GET /v1/sources", s.listSources)
	v1.HandleFunc("DELETE /v1/sources/{name}", s.removeSource)
	v1.HandleFunc("POST /v1/sources/{name}/refresh", s.refreshSource)
	v1.HandleFunc("PUT /v1/sources/{name}/mask", s.setMaskScripts)
	v1.HandleFunc("GET /v1/sources/{name}/mask", s.getMaskScripts)
	v1.HandleFunc("POST /v1/branches", s.createBranch)
	v1.HandleFunc("GET /v1/branches", s.listBranches)
	v1.HandleFunc("GET /v1/branches/{name}", s.getBranch)
	v1.HandleFunc("GET /v1/branches/{name}/usage", s.branchUsage)
	v1.HandleFunc("GET /v1/branches/{name}/diff", s.branchDiff)
	v1.HandleFunc("DELETE /v1/branches/{name}", s.destroyBranch)
	v1.HandleFunc("POST /v1/branches/{name}/reset", s.resetBranch)
	v1.HandleFunc("GET /v1/reconcile/plan", s.reconcilePlan)
	v1.HandleFunc("POST /v1/reconcile", s.reconcileApply)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// /readyz and /metrics sit outside the bearer auth: Prometheus scrapers
	// and kubelet readiness probes do not present a token, and neither leaks
	// secrets (metrics are aggregate; readiness is a boolean).
	mux.HandleFunc("GET /readyz", s.readyz)
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics)
	}
	// static UI assets carry no secrets and are served without auth; every
	// API call the page makes goes through /v1 and needs the bearer token.
	mux.Handle("GET /ui/", uiHandler())
	mux.Handle("/v1/", s.auth(v1))
	return mux
}

// readyz reports readiness via the injected checker: 200 when the registry is
// reachable and the driver responds, 503 otherwise. With no checker wired it
// reports ready (process is up).
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.ready != nil {
		if err := s.ready(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

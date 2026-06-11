// Package api is branchd's REST control plane: a thin JSON layer over the
// engine. stdlib net/http only; routes use Go 1.22 method patterns. All /v1
// routes require the bearer token; /healthz does not.
package api

import (
	"net/http"

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
	Parent   string `json:"parent,omitempty"`
	State    string `json:"state"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	User     string `json:"user"`
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

type Server struct {
	eng   *engine.Engine
	reg   *registry.Registry
	token string
}

func New(eng *engine.Engine, reg *registry.Registry, token string) *Server {
	return &Server{eng: eng, reg: reg, token: token}
}

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
	v1.HandleFunc("DELETE /v1/branches/{name}", s.destroyBranch)
	v1.HandleFunc("POST /v1/branches/{name}/reset", s.resetBranch)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// static UI assets carry no secrets and are served without auth; every
	// API call the page makes goes through /v1 and needs the bearer token.
	mux.Handle("GET /ui/", uiHandler())
	mux.Handle("/v1/", s.auth(v1))
	return mux
}

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
	Name       string `json:"name"`
	PGVersion  string `json:"pg_version"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	Database   string `json:"database"`
	Network    string `json:"network,omitempty"`
	State      string `json:"state"`
	Generation int    `json:"generation"`
	CreatedAt  string `json:"created_at"`
}

type Branch struct {
	Name     string `json:"name"`
	Source   string `json:"source"`
	State    string `json:"state"`
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
}

type CreateBranchRequest struct {
	Name       string `json:"name"`
	Source     string `json:"source"`
	TTLSeconds int    `json:"ttl_seconds"`
}

type RefreshSourceRequest struct {
	Password string `json:"password"`
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
	v1.HandleFunc("POST /v1/branches", s.createBranch)
	v1.HandleFunc("GET /v1/branches", s.listBranches)
	v1.HandleFunc("GET /v1/branches/{name}", s.getBranch)
	v1.HandleFunc("DELETE /v1/branches/{name}", s.destroyBranch)
	v1.HandleFunc("POST /v1/branches/{name}/reset", s.resetBranch)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.Handle("/v1/", s.auth(v1))
	return mux
}

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/engine"
	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeEngineError maps engine/registry failures to HTTP statuses: invalid
// input -> 400, missing rows -> 404, name/lifecycle conflicts -> 409,
// everything else -> 500.
func writeEngineError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	msg := err.Error()
	switch {
	case errors.Is(err, engine.ErrInvalidName),
		errors.Is(err, registry.ErrUnsupportedPGVersion):
		code = http.StatusBadRequest
	case errors.Is(err, registry.ErrNotFound):
		code = http.StatusNotFound
	case strings.Contains(msg, "UNIQUE constraint"),
		strings.Contains(msg, "live branch"),
		strings.Contains(msg, "child branch"),
		strings.Contains(msg, "illegal branch transition"),
		strings.Contains(msg, "not ready"):
		code = http.StatusConflict
	}
	writeError(w, code, msg)
}

func decode[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return v, false
	}
	return v, true
}

func sourceJSON(s *registry.Source) Source {
	return Source{
		Name: s.Name, PGVersion: s.PGVersion, Host: s.ConnHost, Port: s.ConnPort,
		User: s.ConnUser, Database: s.ConnDB, Network: s.Network,
		State: string(s.State), Generation: s.Generation, CreatedAt: s.CreatedAt,
	}
}

// branchJSON renders a branch with its source's connection identity and the
// router hint (dbname@branch).
func (s *Server) branchJSON(b *registry.Branch) Branch {
	user, db := "postgres", "postgres"
	srcName := ""
	if src, err := s.reg.GetSourceByID(b.SourceID); err == nil {
		srcName = src.Name
		if src.ConnUser != "" {
			user = src.ConnUser
		}
		if src.ConnDB != "" {
			db = src.ConnDB
		}
	}
	return Branch{
		Name: b.Name, Source: srcName, Parent: b.ParentBranchName, State: string(b.State), Host: b.Host, Port: b.Port,
		User: user, Database: db, ProxyDatabase: db + "@" + b.Name,
		ExpiresAt: b.ExpiresAt, CreatedAt: b.CreatedAt,
	}
}

func (s *Server) createSource(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[CreateSourceRequest](w, r)
	if !ok {
		return
	}
	if req.Name == "" || req.Host == "" {
		writeError(w, http.StatusBadRequest, "name and host are required")
		return
	}
	if req.Port == 0 {
		req.Port = 5432
	}
	if req.User == "" {
		req.User = "postgres"
	}
	if req.Database == "" {
		req.Database = "postgres"
	}
	src := &registry.Source{
		Name: req.Name, PGVersion: req.PGVersion, ConnHost: req.Host,
		ConnPort: req.Port, ConnUser: req.User, ConnDB: req.Database, Network: req.Network,
	}
	if err := s.eng.AddSource(r.Context(), src, req.Password); err != nil {
		writeEngineError(w, err)
		return
	}
	fresh, err := s.reg.GetSourceByName(req.Name)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sourceJSON(fresh))
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	sources, err := s.reg.ListSources()
	if err != nil {
		writeEngineError(w, err)
		return
	}
	out := make([]Source, 0, len(sources))
	for _, src := range sources {
		out = append(out, sourceJSON(src))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) removeSource(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.RemoveSource(r.Context(), r.PathValue("name")); err != nil {
		writeEngineError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) refreshSource(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[RefreshSourceRequest](w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if err := s.eng.RefreshSource(r.Context(), name, req.Password); err != nil {
		writeEngineError(w, err)
		return
	}
	fresh, err := s.reg.GetSourceByName(name)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sourceJSON(fresh))
}

// setMaskScripts replaces a source's masking scripts with the request body
// (a JSON array of {name, sql}; empty array clears them).
func (s *Server) setMaskScripts(w http.ResponseWriter, r *http.Request) {
	src, err := s.reg.GetSourceByName(r.PathValue("name"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	scripts, ok := decode[[]MaskScript](w, r)
	if !ok {
		return
	}
	for _, sc := range scripts {
		if sc.SQL == "" {
			writeError(w, http.StatusBadRequest, "mask script sql must not be empty")
			return
		}
	}
	rs := make([]registry.MaskScript, len(scripts))
	for i, sc := range scripts {
		rs[i] = registry.MaskScript{Name: sc.Name, SQL: sc.SQL}
	}
	if err := s.reg.SetMaskScripts(src.ID, rs); err != nil {
		writeEngineError(w, err)
		return
	}
	if scripts == nil {
		scripts = []MaskScript{}
	}
	writeJSON(w, http.StatusOK, scripts)
}

func (s *Server) getMaskScripts(w http.ResponseWriter, r *http.Request) {
	src, err := s.reg.GetSourceByName(r.PathValue("name"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	rs, err := s.reg.GetMaskScripts(src.ID)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	out := make([]MaskScript, 0, len(rs))
	for _, sc := range rs {
		out = append(out, MaskScript{Name: sc.Name, SQL: sc.SQL})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createBranch(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[CreateBranchRequest](w, r)
	if !ok {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if (req.Source == "") == (req.Parent == "") {
		writeError(w, http.StatusBadRequest, "exactly one of source or parent is required")
		return
	}
	if req.TTLSeconds < 0 {
		writeError(w, http.StatusBadRequest, "ttl_seconds must be >= 0")
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	var (
		b   *registry.Branch
		err error
	)
	if req.Parent != "" {
		b, err = s.eng.CreateBranchFrom(r.Context(), req.Name, req.Parent, ttl)
	} else {
		b, err = s.eng.CreateBranch(r.Context(), req.Name, req.Source, ttl)
	}
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.branchJSON(b))
}

func (s *Server) listBranches(w http.ResponseWriter, r *http.Request) {
	branches, err := s.reg.ListLiveBranches()
	if err != nil {
		writeEngineError(w, err)
		return
	}
	out := make([]Branch, 0, len(branches))
	for _, b := range branches {
		out = append(out, s.branchJSON(b))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getBranch(w http.ResponseWriter, r *http.Request) {
	b, err := s.reg.GetBranchByName(r.PathValue("name"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.branchJSON(b))
}

// branchUsage measures the branch's rw-layer disk usage via a one-shot
// helper container — a runtime roundtrip, so callers should treat it as an
// on-demand probe, not a free field.
func (s *Server) branchUsage(w http.ResponseWriter, r *http.Request) {
	n, err := s.eng.BranchUsage(r.Context(), r.PathValue("name"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"bytes": n})
}

func (s *Server) destroyBranch(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.DestroyBranch(r.Context(), r.PathValue("name")); err != nil {
		writeEngineError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resetBranch(w http.ResponseWriter, r *http.Request) {
	b, err := s.eng.ResetBranch(r.Context(), r.PathValue("name"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.branchJSON(b))
}

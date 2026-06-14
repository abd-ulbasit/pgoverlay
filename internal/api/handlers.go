package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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
// input -> 400, missing rows -> 404, quota exceeded -> 403, name/lifecycle
// conflicts -> 409, everything else -> 500.
//
// The mapped 4xx cases return their (intentional, already-clean) messages.
// The default 500 case does NOT: err.Error() can carry SQLite/driver/volume/
// path internals, so the real error is logged server-side and the client sees
// a generic body. r may be nil (logs without method/path then).
func writeEngineError(w http.ResponseWriter, r *http.Request, err error) {
	msg := err.Error()
	switch {
	case errors.Is(err, engine.ErrInvalidName),
		errors.Is(err, registry.ErrUnsupportedPGVersion):
		writeError(w, http.StatusBadRequest, msg)
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, msg)
	case errors.Is(err, engine.ErrQuotaExceeded):
		writeError(w, http.StatusForbidden, msg)
	case strings.Contains(msg, "UNIQUE constraint"),
		strings.Contains(msg, "live branch"),
		strings.Contains(msg, "child branch"),
		strings.Contains(msg, "illegal branch transition"),
		strings.Contains(msg, "not ready"):
		writeError(w, http.StatusConflict, msg)
	default:
		// Unmapped: treat as internal. Log the full detail; tell the client nothing.
		attrs := []any{"error", err}
		if r != nil {
			attrs = append(attrs, "method", r.Method, "path", r.URL.Path)
		}
		slog.Error("api: internal server error", attrs...)
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
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
		Via: s.SeedVia, DumpSchemas: s.DumpSchemas,
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
		User: user, Password: b.Password, Database: db, ProxyDatabase: db + "@" + b.Name,
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
	if req.Via == "" {
		req.Via = registry.SeedViaBasebackup
	}
	if req.Via != registry.SeedViaBasebackup && req.Via != registry.SeedViaDump {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid via %q: want %q or %q",
			req.Via, registry.SeedViaBasebackup, registry.SeedViaDump))
		return
	}
	if len(req.DumpSchemas) > 0 && req.Via != registry.SeedViaDump {
		writeError(w, http.StatusBadRequest, "dump_schemas is only valid with via=dump")
		return
	}
	src := &registry.Source{
		Name: req.Name, PGVersion: req.PGVersion, ConnHost: req.Host,
		ConnPort: req.Port, ConnUser: req.User, ConnDB: req.Database, Network: req.Network,
		SeedVia: req.Via, DumpSchemas: req.DumpSchemas,
	}
	if err := s.eng.AddSource(r.Context(), src, req.Password); err != nil {
		writeEngineError(w, r, err)
		return
	}
	fresh, err := s.reg.GetSourceByName(req.Name)
	if err != nil {
		writeEngineError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sourceJSON(fresh))
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	sources, err := s.reg.ListSources()
	if err != nil {
		writeEngineError(w, r, err)
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
		writeEngineError(w, r, err)
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
		writeEngineError(w, r, err)
		return
	}
	fresh, err := s.reg.GetSourceByName(name)
	if err != nil {
		writeEngineError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, sourceJSON(fresh))
}

// setMaskScripts replaces a source's masking scripts with the request body
// (a JSON array of {name, sql}; empty array clears them).
func (s *Server) setMaskScripts(w http.ResponseWriter, r *http.Request) {
	src, err := s.reg.GetSourceByName(r.PathValue("name"))
	if err != nil {
		writeEngineError(w, r, err)
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
		writeEngineError(w, r, err)
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
		writeEngineError(w, r, err)
		return
	}
	rs, err := s.reg.GetMaskScripts(src.ID)
	if err != nil {
		writeEngineError(w, r, err)
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
		writeEngineError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.branchJSON(b))
}

func (s *Server) listBranches(w http.ResponseWriter, r *http.Request) {
	branches, err := s.reg.ListLiveBranches()
	if err != nil {
		writeEngineError(w, r, err)
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
		writeEngineError(w, r, err)
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
		writeEngineError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"bytes": n})
}

// branchDiff reports what changed in a branch relative to its base: a
// unified schema diff plus per-table row-estimate deltas (engine.DiffResult).
// This is a LONG request — the engine provisions a throwaway clone of the
// branch's base and pg_dumps both instances, so expect ~5-10s of latency;
// clients should use a generous timeout. 404 unknown branch, 409 not ready.
func (s *Server) branchDiff(w http.ResponseWriter, r *http.Request) {
	var opts []engine.DiffOption
	// ?data=N turns on bounded data sampling (up to N branch-only rows per
	// grown table). data=0/absent leaves sampling off.
	if v := r.URL.Query().Get("data"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "data must be a non-negative integer")
			return
		}
		if n > 0 {
			opts = append(opts, engine.WithDataSample(n))
		}
	}
	res, err := s.eng.DiffBranch(r.Context(), r.PathValue("name"), opts...)
	if err != nil {
		writeEngineError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// branchHistory returns a branch's audit trail: every recorded state
// transition with its reason, the actor that caused it, and the timestamp,
// oldest first. Role-gated at viewer. 404 if the name was never used.
func (s *Server) branchHistory(w http.ResponseWriter, r *http.Request) {
	rows, err := s.reg.BranchHistory(r.PathValue("name"))
	if err != nil {
		writeEngineError(w, r, err)
		return
	}
	out := make([]Transition, 0, len(rows))
	for _, t := range rows {
		out = append(out, Transition{
			FromState: t.FromState, ToState: t.ToState,
			Reason: t.Reason, Actor: t.Actor, At: t.At,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) destroyBranch(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.DestroyBranch(r.Context(), r.PathValue("name")); err != nil {
		writeEngineError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resetBranch(w http.ResponseWriter, r *http.Request) {
	b, err := s.eng.ResetBranch(r.Context(), r.PathValue("name"))
	if err != nil {
		writeEngineError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.branchJSON(b))
}

// reconcilePlan computes the read-only convergence plan (drift report) and
// returns it as JSON. Backs `pgb doctor`. Mutates nothing.
func (s *Server) reconcilePlan(w http.ResponseWriter, r *http.Request) {
	plan, err := s.eng.PlanReconcile(r.Context(), time.Now(), s.stuckTimeout)
	if err != nil {
		writeEngineError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// reconcileApply runs a reconcile pass and returns the actions actually taken.
// Backs `pgb gc`. Best-effort: a partial failure still returns the actions that
// succeeded with a 200 (the error surfaces in branchd logs/metrics).
func (s *Server) reconcileApply(w http.ResponseWriter, r *http.Request) {
	taken, err := s.eng.ApplyReconcile(r.Context(), time.Now(), s.stuckTimeout)
	if err != nil {
		writeEngineError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, taken)
}

// createToken mints an API token (admin-only) and returns the plaintext once.
func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var req CreateTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if !registry.ValidRole(req.Role) {
		writeError(w, http.StatusBadRequest, "invalid role: want admin, operator or viewer")
		return
	}
	plaintext, err := s.reg.CreateAPIToken(req.Name, req.Role)
	if err != nil {
		writeEngineError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, CreateTokenResponse{Token: plaintext})
}

// listTokens returns token metadata (admin-only) — never the plaintext or hash.
func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	tokens, err := s.reg.ListAPITokens()
	if err != nil {
		writeEngineError(w, r, err)
		return
	}
	out := make([]Token, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, Token{Name: t.Name, Role: t.Role, CreatedAt: t.CreatedAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// revokeToken deletes a token by name (admin-only).
func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	if err := s.reg.RevokeAPIToken(r.PathValue("name")); err != nil {
		writeEngineError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

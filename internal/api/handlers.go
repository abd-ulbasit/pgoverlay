package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/engine"
	"github.com/abd-ulbasit/pgoverlay/internal/pgctl"
	"github.com/abd-ulbasit/pgoverlay/internal/registry"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, ErrorResponse{Error: msg})
}

// writeEngineError maps engine/registry failures to HTTP statuses: invalid
// input -> 400, missing rows -> 404, quota exceeded -> 403, a failed destroy
// teardown -> 409 (a resource still in use), 502 (the runtime unreachable) or
// 500, a mutation interrupted by lost leadership or shutdown -> 503 (stuck
// timeout -> 504), a failing seed or masking script -> 422, name/lifecycle
// conflicts -> 409, everything else -> 500.
//
// The mapped 4xx cases return their (intentional, already-clean) messages;
// seed/masking failures return the tool's output (never a password: it only
// travels in the helper's environment), clipped. A failed destroy returns the
// cause it journaled, the same text `pgb history` shows any viewer. A
// duplicate name returns a plain sentence instead of the raw SQLite
// constraint text. The default 500 case does NOT return the message:
// err.Error() can carry SQLite/driver/volume/path internals, so the real
// error is logged server-side and the client sees a generic body. r may be
// nil (logs without method/path then).
func writeEngineError(w http.ResponseWriter, r *http.Request, err error) {
	msg := err.Error()
	var destroyErr *engine.DestroyError
	switch {
	case errors.Is(err, engine.ErrInvalidName),
		errors.Is(err, registry.ErrUnsupportedPGVersion),
		errors.Is(err, registry.ErrInvalidImage),
		errors.Is(err, registry.ErrInvalidTokenName),
		errors.Is(err, pgctl.ErrInvalidSpec),
		errors.Is(err, pgctl.ErrVersionMismatch):
		writeError(w, http.StatusBadRequest, msg)
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, msg)
	case errors.Is(err, engine.ErrBaseGone):
		writeError(w, http.StatusConflict, msg)
	case errors.Is(err, engine.ErrQuotaExceeded):
		writeError(w, http.StatusForbidden, msg)
	case errors.As(err, &destroyErr):
		// The teardown failed and the branch stays in destroying with the
		// cause journaled. Checked before the interruption case: the teardown
		// runs detached from the request, so its failure is its own even when
		// the request's deadline passed meanwhile.
		code := http.StatusInternalServerError
		switch {
		case destroyErr.InUse:
			code = http.StatusConflict // free the resource, then destroy again
		case destroyErr.RuntimeUnavailable:
			code = http.StatusBadGateway // the runtime behind branchd is down
		}
		slog.Warn("api: destroy failed", "branch", destroyErr.Branch, "error", err)
		writeError(w, code, clipMessage(fmt.Sprintf("destroy of branch %q failed; it stays in destroying, destroy it again to retry: %s",
			destroyErr.Branch, destroyErr.Reason)))
	case r != nil && r.Context().Err() != nil && interruptedStatus(r.Context()) != 0:
		// The mutation's context was ended from outside the saga (leadership
		// lost, shutdown, stuck timeout): the failure is that interruption, and
		// the saga has already compensated. Tell the client it can retry.
		cause := context.Cause(r.Context())
		slog.Warn("api: operation cancelled", "cause", cause, "error", err, "method", r.Method, "path", r.URL.Path)
		writeError(w, interruptedStatus(r.Context()), cause.Error()+"; the operation was cancelled and its partial work rolled back, retry it")
	case errors.Is(err, engine.ErrSeedFailed), errors.Is(err, engine.ErrMaskingFailed):
		// Caused by the source's configuration or its masking SQL; checked
		// before the substring matches below because the message carries
		// arbitrary tool output. The full text stays in the log.
		slog.Warn("api: operation failed on user-supplied configuration", "error", err)
		writeError(w, http.StatusUnprocessableEntity, clipMessage(msg))
	case errors.Is(err, registry.ErrAlreadyExists),
		errors.Is(err, registry.ErrIllegalTransition),
		errors.Is(err, engine.ErrNotRecoverable):
		// lifecycle conflicts; the registry's messages are already clean
		// (a duplicate names the holder and its state)
		writeError(w, http.StatusConflict, msg)
	case strings.Contains(msg, "UNIQUE constraint"):
		writeError(w, http.StatusConflict, duplicateMessage(msg))
	case strings.Contains(msg, "live branch"),
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

// uniqueTableRe pulls the table out of SQLite's "UNIQUE constraint failed:
// <table>.<column>" text.
var uniqueTableRe = regexp.MustCompile(`UNIQUE constraint failed: (\w+)\.`)

// duplicateMessage turns a raw SQLite unique-constraint error into a sentence
// a client can act on, without echoing driver internals.
func duplicateMessage(msg string) string {
	kind := "an object"
	if m := uniqueTableRe.FindStringSubmatch(msg); m != nil {
		switch m[1] {
		case "branches":
			kind = "a live branch"
		case "sources":
			kind = "a source"
		case "api_tokens":
			kind = "a token"
		}
	}
	return kind + " with that name already exists"
}

// maxErrorMessage bounds the tool output returned in a 422 body; the full
// error is still in branchd's log.
const maxErrorMessage = 2048

// clipMessage keeps the head (what failed) and the tail (the tool's final
// error lines) of an over-long message.
func clipMessage(msg string) string {
	if len(msg) <= maxErrorMessage {
		return msg
	}
	const head = 512
	tail := maxErrorMessage - head
	return strings.ToValidUTF8(msg[:head]+" … "+msg[len(msg)-tail:], "")
}

// interruptedStatus maps why a mutation's context ended to a status: 503 when
// leadership moved or branchd is shutting down (retry against the leader),
// 504 when the saga ran past the stuck timeout, 0 for anything else (e.g. a
// read whose client went away).
func interruptedStatus(ctx context.Context) int {
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errLeadershipLost), errors.Is(cause, errShuttingDown):
		return http.StatusServiceUnavailable
	case errors.Is(cause, errMutationTimeout):
		return http.StatusGatewayTimeout
	}
	return 0
}

// maxBodyBytes caps every JSON request body. The largest legitimate body is a
// source's masking scripts; 1 MiB of SQL is far beyond any real set.
const maxBodyBytes = 1 << 20

// decode reads exactly one JSON value of type T from the request body. It is
// strict on purpose: an unknown field (a typo such as "ttl" for
// "ttl_seconds") is a 400 naming the field instead of being silently dropped,
// trailing data after the value is a 400, and a body over maxBodyBytes is a
// 413 before it is buffered.
func decode[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(&v)
	if err == nil {
		var extra json.RawMessage
		if xerr := dec.Decode(&extra); xerr != io.EOF {
			err = xerr
			if err == nil {
				err = errors.New("unexpected data after the JSON value")
			}
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds %d bytes", tooBig.Limit))
			return v, false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+strings.TrimPrefix(err.Error(), "json: "))
		return v, false
	}
	return v, true
}

func sourceJSON(s *registry.Source) Source {
	return Source{
		Name: s.Name, PGVersion: s.PGVersion, Host: s.ConnHost, Port: s.ConnPort,
		User: s.ConnUser, Database: s.ConnDB, Network: s.Network,
		Via: s.SeedVia, DumpSchemas: s.DumpSchemas, Image: s.Image,
		State: string(s.State), Generation: s.Generation, CreatedAt: s.CreatedAt,
	}
}

// branchJSON renders a branch with its source's connection identity and the
// router hint (dbname@branch). A failed source lookup is returned (callers
// answer 500) instead of being papered over with default credentials that
// would send a client to the wrong database; only a source row that is
// genuinely gone falls back to the defaults.
func (s *Server) branchJSON(b *registry.Branch) (Branch, error) {
	src, err := s.reg.GetSourceByID(b.SourceID)
	if err != nil {
		if !errors.Is(err, registry.ErrNotFound) {
			return Branch{}, fmt.Errorf("branch %q: load source: %w", b.Name, err)
		}
		src = nil
	}
	return s.renderBranch(b, src), nil
}

// renderBranch builds the wire Branch from a branch row and its source (nil
// when the source row no longer exists), with the advertised router address.
func (s *Server) renderBranch(b *registry.Branch, src *registry.Source) Branch {
	user, db := "postgres", "postgres"
	srcName := ""
	if src != nil {
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
		User: user, Password: b.Password, PasswordUnavailable: b.PasswordUnavailable,
		Database: db, ProxyDatabase: db + "@" + b.Name,
		ExpiresAt: b.ExpiresAt, CreatedAt: b.CreatedAt,
		ProxyHost: s.proxy.host, ProxyPort: s.proxy.port,
	}
}

func (s *Server) createSource(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[CreateSourceRequest](w, r)
	if !ok {
		return
	}
	if req.Name == "" || strings.TrimSpace(req.Host) == "" {
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
		SeedVia: req.Via, DumpSchemas: req.DumpSchemas, Image: req.Image,
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

// maxTTLSeconds is the largest ttl_seconds that fits a time.Duration (~292
// years).
const maxTTLSeconds = math.MaxInt64 / int64(time.Second)

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
	// Bound the TTL before converting it: time.Duration(n)*time.Second wraps
	// negative for n > maxTTLSeconds, which the engine would read as "no TTL"
	// (never expires) and so slip past --max-ttl.
	if req.TTLSeconds < 0 || int64(req.TTLSeconds) > maxTTLSeconds {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("ttl_seconds must be between 0 and %d", maxTTLSeconds))
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
	s.writeBranch(w, r, http.StatusCreated, b)
}

// writeBranch renders b (with its source's identity) or the lookup error.
func (s *Server) writeBranch(w http.ResponseWriter, r *http.Request, code int, b *registry.Branch) {
	out, err := s.branchJSON(b)
	if err != nil {
		writeEngineError(w, r, err)
		return
	}
	writeJSON(w, code, out)
}

func (s *Server) listBranches(w http.ResponseWriter, r *http.Request) {
	branches, err := s.reg.ListLiveBranches()
	if err != nil {
		writeEngineError(w, r, err)
		return
	}
	// one sources query for the whole list instead of one per branch
	sources, err := s.reg.ListSources()
	if err != nil {
		writeEngineError(w, r, err)
		return
	}
	byID := make(map[string]*registry.Source, len(sources))
	for _, src := range sources {
		byID[src.ID] = src
	}
	out := make([]Branch, 0, len(branches))
	for _, b := range branches {
		out = append(out, s.renderBranch(b, byID[b.SourceID]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getBranch(w http.ResponseWriter, r *http.Request) {
	b, err := s.reg.GetBranchByName(r.PathValue("name"))
	if err != nil {
		writeEngineError(w, r, err)
		return
	}
	s.writeBranch(w, r, http.StatusOK, b)
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
	writeJSON(w, http.StatusOK, Usage{Bytes: n})
}

// branchDiff reports what changed in a branch relative to its base: a
// unified schema diff plus per-table row-estimate deltas (engine.DiffResult).
// This is a LONG request — the engine provisions a throwaway clone of the
// branch's base and pg_dumps both instances, so expect ~5-10s of latency;
// clients should use a generous timeout. Because it writes a registry row and
// provisions an instance it is routed like a mutation: operator role, leader
// only. 404 unknown branch, 409 not ready.
func (s *Server) branchDiff(w http.ResponseWriter, r *http.Request) {
	var opts []engine.DiffOption
	// ?data=N turns on bounded data sampling (up to N branch-only rows per
	// grown table, N <= engine.MaxSampleRows). data=0/absent leaves sampling
	// off.
	if v := r.URL.Query().Get("data"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > engine.MaxSampleRows {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("data must be an integer between 0 and %d", engine.MaxSampleRows))
			return
		}
		if n > 0 {
			opts = append(opts, engine.WithDataSample(n))
		}
	}
	// On the csi backend, diffing a branch created from another branch stops
	// that parent briefly around the base clone (engine.WithParentQuiesce):
	// the same disruption as a reset, so it needs the role a reset needs. For
	// a lower role the engine refuses before touching anything (403).
	role, _ := r.Context().Value(roleKey).(string)
	if roleRank[role] >= roleRank[registry.RoleOperator] {
		opts = append(opts, engine.WithParentQuiesce())
	}
	res, err := s.eng.DiffBranch(r.Context(), r.PathValue("name"), opts...)
	if errors.Is(err, engine.ErrParentQuiesce) {
		writeError(w, http.StatusForbidden, "role "+role+" lacks the required "+registry.RoleOperator+" privilege: "+err.Error())
		return
	}
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
	s.writeBranch(w, r, http.StatusOK, b)
}

// recoverBranch restarts a failed branch on its existing data (no re-clone):
// the way back for a branch failed by crash recovery with its data intact.
// 409 when the branch is not failed or its volumes are gone.
func (s *Server) recoverBranch(w http.ResponseWriter, r *http.Request) {
	b, err := s.eng.RecoverBranch(r.Context(), r.PathValue("name"))
	if err != nil {
		writeEngineError(w, r, err)
		return
	}
	s.writeBranch(w, r, http.StatusOK, b)
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
	req, ok := decode[CreateTokenRequest](w, r)
	if !ok {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	// The registry's rule, checked here too so a bad name is refused before
	// the role: names are rendered into every audit entry as "name (role)",
	// and "root" is the built-in PGOVERLAY_TOKEN's audit name.
	if err := registry.ValidateTokenName(req.Name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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

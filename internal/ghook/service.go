// Package ghook is a small GitHub webhook receiver that maps pull-request
// lifecycle events to pgbranch branches (branch-per-PR): opened/reopened →
// ensure branch pr-<number> exists, synchronize → ensure (and optionally
// reset), closed → destroy. It talks to branchd through internal/apiclient
// and, when GitHub credentials are configured (App or PAT), reports back to
// the PR: a pgbranch/branch commit status around every branch operation and
// a live connect-info comment kept current in place.
package ghook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/abd-ulbasit/pgbranch/internal/api"
	"github.com/abd-ulbasit/pgbranch/internal/apiclient"
)

// Config is the static service configuration (see cmd/pgbranch-github for
// the GHOOK_* environment mapping).
type Config struct {
	WebhookSecret string   // HMAC key for X-Hub-Signature-256 (required)
	Source        string   // pgbranch source to branch from (required)
	TTLSeconds    int      // branch TTL passed on create (0 = no TTL)
	ResetOnPush   bool     // synchronize resets the branch when true
	Repos         []string // "owner/name" allow-list; empty allows all
	ProxyHost     string   // host[:port] of the pgbranch proxy, for comments
	// BranchNaming picks the pgbranch branch name for a pull request:
	//   "pr-number" (default): pr-<number>
	//   "git-branch": the PR's head ref, sanitized (e.g. feat/login -> feat-login).
	// git-branch lets preview platforms derive the name from the git ref
	// they already know (Vercel's VERCEL_GIT_COMMIT_REF is present from the
	// very first build, before the PR association exists).
	BranchNaming string
}

type Service struct {
	cfg Config
	pg  *apiclient.Client
	gh  *GitHub // nil when commenting is disabled
	log *slog.Logger
	wg  sync.WaitGroup // in-flight detached branch operations
}

// Wait blocks until all detached branch operations have finished. Call after
// the HTTP server has shut down so in-flight work completes before exit.
func (s *Service) Wait() { s.wg.Wait() }

func New(cfg Config, pg *apiclient.Client, gh *GitHub, log *slog.Logger) *Service {
	return &Service{cfg: cfg, pg: pg, gh: gh, log: log}
}

// Handler returns the HTTP surface: POST /webhook and GET /healthz.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("POST /webhook", s.handleWebhook)
	return mux
}

// payload holds the only fields of a pull_request event we use.
type payload struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Head struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	// Installation is set when the webhook is delivered through a GitHub
	// App; its id keys the installation-token mint in App auth mode.
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

func (s *Service) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !verifySignature(s.cfg.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		s.log.Warn("webhook signature verification failed", "remote", r.RemoteAddr)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	if event := r.Header.Get("X-GitHub-Event"); event != "pull_request" {
		s.log.Debug("ignoring event", "event", event)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "invalid JSON payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !s.repoAllowed(p.Repository.FullName) {
		s.log.Info("ignoring repo not on allow-list", "repo", p.Repository.FullName)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.dispatch(w, r, &p)
}

// verifySignature checks the GitHub HMAC-SHA256 signature header
// ("sha256=<hex>") over the raw request body in constant time.
func verifySignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func (s *Service) repoAllowed(fullName string) bool {
	if len(s.cfg.Repos) == 0 {
		return true
	}
	for _, r := range s.cfg.Repos {
		if strings.EqualFold(r, fullName) {
			return true
		}
	}
	return false
}

func (s *Service) dispatch(w http.ResponseWriter, r *http.Request, p *payload) {
	log := s.log.With("action", p.Action, "repo", p.Repository.FullName,
		"pr", p.Number, "head_sha", p.PullRequest.Head.SHA)
	branch := s.branchName(p)

	switch p.Action {
	case "opened", "reopened", "synchronize", "closed":
	default:
		log.Debug("ignoring pull_request action")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// GitHub abandons webhook deliveries after ~10s, and an abandoned
	// request's canceled context would abort branchd's saga mid-flight —
	// branch creation/reset at pod speed routinely exceeds that deadline.
	// Ack the delivery now and run the operation detached.
	payload := *p
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		switch payload.Action {
		case "opened", "reopened", "synchronize":
			s.handleEnsure(ctx, log, &payload, branch)
		case "closed":
			s.handleClosed(ctx, log, &payload, branch)
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"branch": branch, "status": "accepted"})
}

// branchName derives the pgbranch branch name for a pull request according
// to Config.BranchNaming. git-branch mode falls back to pr-<number> when the
// sanitized ref comes up empty.
func (s *Service) branchName(p *payload) string {
	if s.cfg.BranchNaming == "git-branch" {
		if n := sanitizeBranchName(p.PullRequest.Head.Ref); n != "" {
			return n
		}
	}
	return fmt.Sprintf("pr-%d", p.Number)
}

// sanitizeBranchName maps a git ref to a valid pgbranch branch name
// (^[a-z0-9][a-z0-9-]{0,40}$): lowercase, runs of other characters collapse
// to single dashes, edges trimmed, truncated to 41 chars.
func sanitizeBranchName(ref string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(ref) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
		if b.Len() >= 41 {
			break
		}
	}
	return strings.TrimRight(b.String()[:min(b.Len(), 41)], "-")
}

// handleEnsure brackets the branch operation with commit statuses on the PR
// head SHA — pending before, success/failure after — and keeps the live
// comment current (creating/resetting → ready / reset @ sha). GitHub-side
// failures are logged, never fatal: the branch operation is the point of
// this service.
func (s *Service) handleEnsure(ctx context.Context, log *slog.Logger, p *payload, branch string) {
	verb := "creating"
	if p.Action == "synchronize" && s.cfg.ResetOnPush {
		verb = "resetting"
	}
	s.setStatus(ctx, log, p, "pending", fmt.Sprintf("%s branch %s", verb, branch))
	s.upsertComment(ctx, log, p, commentBody(s.cfg.ProxyHost, branch, verb, nil))

	b, didReset, err := s.ensureBranch(ctx, log, p, branch)
	if err != nil {
		log.Error("handling event failed", "err", err)
		s.setStatus(ctx, log, p, "failure", err.Error())
		return
	}
	state := "ready"
	if didReset {
		state = "reset @ " + shortSHA(p.PullRequest.Head.SHA)
	}
	desc := fmt.Sprintf("branch %s ready", branch)
	if s.cfg.ProxyHost != "" {
		desc += " — connect via " + s.cfg.ProxyHost
	}
	s.setStatus(ctx, log, p, "success", desc)
	s.upsertComment(ctx, log, p, commentBody(s.cfg.ProxyHost, branch, state, b))
}

// handleClosed destroys the branch and rewrites the live comment to record
// that (without a connect string — there is nothing left to connect to). No
// status: statuses on a closed PR don't matter.
func (s *Service) handleClosed(ctx context.Context, log *slog.Logger, p *payload, branch string) {
	if err := s.destroyBranch(ctx, log, branch); err != nil {
		log.Error("handling event failed", "err", err)
		return
	}
	gh := s.github(p)
	if gh == nil {
		return
	}
	body := commentBody(s.cfg.ProxyHost, branch, "destroyed", nil)
	if err := gh.UpdateComment(ctx, p.Repository.FullName, p.Number, body); err != nil {
		log.Warn("updating PR comment failed", "err", err)
	}
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// ensureBranch makes branch exist (creating it from the configured source if
// missing). On synchronize with ResetOnPush, a pre-existing branch is reset
// to the source snapshot (didReset reports that); a freshly created one is
// already pristine.
func (s *Service) ensureBranch(ctx context.Context, log *slog.Logger, p *payload, branch string) (b *api.Branch, didReset bool, err error) {
	b, err = s.pg.GetBranch(ctx, branch)
	switch {
	case apiclient.IsNotFound(err):
		b, err = s.pg.CreateBranch(ctx, api.CreateBranchRequest{
			Name: branch, Source: s.cfg.Source, TTLSeconds: s.cfg.TTLSeconds,
		})
		if err != nil {
			return nil, false, fmt.Errorf("create branch %s: %w", branch, err)
		}
		log.Info("branch created", "branch", branch, "source", s.cfg.Source)
	case err != nil:
		return nil, false, fmt.Errorf("get branch %s: %w", branch, err)
	case p.Action == "synchronize" && s.cfg.ResetOnPush:
		if b, err = s.pg.ResetBranch(ctx, branch); err != nil {
			return nil, false, fmt.Errorf("reset branch %s: %w", branch, err)
		}
		log.Info("branch reset on push", "branch", branch)
		didReset = true
	default:
		log.Debug("branch already exists", "branch", branch)
	}
	return b, didReset, nil
}

// setStatus posts a pgbranch/branch commit status on the PR head SHA when a
// GitHub client is configured. Failures are logged, never fatal (same policy
// as comments). Closed events never reach here: statuses on a closed PR
// don't matter.
func (s *Service) setStatus(ctx context.Context, log *slog.Logger, p *payload, state, desc string) {
	gh := s.github(p)
	if gh == nil || p.PullRequest.Head.SHA == "" {
		return
	}
	if err := gh.SetStatus(ctx, p.Repository.FullName, p.PullRequest.Head.SHA, state, desc); err != nil {
		log.Warn("setting commit status failed", "state", state, "err", err)
	}
}

// github returns the GitHub client bound to the delivery's installation id
// (App auth mints per-installation tokens), or nil when GitHub credentials
// aren't configured.
func (s *Service) github(p *payload) *GitHub {
	if s.gh == nil {
		return nil
	}
	return s.gh.ForInstallation(p.Installation.ID)
}

func (s *Service) destroyBranch(ctx context.Context, log *slog.Logger, branch string) error {
	err := s.pg.DestroyBranch(ctx, branch)
	switch {
	case apiclient.IsNotFound(err):
		log.Debug("branch already gone", "branch", branch)
	case err != nil:
		return fmt.Errorf("destroy branch %s: %w", branch, err)
	default:
		log.Info("branch destroyed", "branch", branch)
	}
	return nil
}

// upsertComment writes body to the PR's live marker comment when a GitHub
// client is configured. Failures are logged, never fatal: the branch
// operation is the point of this service, the comment is a convenience.
func (s *Service) upsertComment(ctx context.Context, log *slog.Logger, p *payload, body string) {
	gh := s.github(p)
	if gh == nil {
		return
	}
	if err := gh.UpsertComment(ctx, p.Repository.FullName, p.Number, body); err != nil {
		log.Warn("posting PR comment failed", "err", err)
	}
}

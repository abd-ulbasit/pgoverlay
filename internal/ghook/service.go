// Package ghook is a small GitHub webhook receiver that maps pull-request
// lifecycle events to pgbranch branches (branch-per-PR): opened/reopened →
// ensure branch pr-<number> exists, synchronize → ensure (and optionally
// reset), closed → destroy. It talks to branchd through internal/apiclient
// and, when a GitHub token is configured, posts a one-time connect-info
// comment on the PR.
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
}

type Service struct {
	cfg Config
	pg  *apiclient.Client
	gh  *GitHub // nil when commenting is disabled
	log *slog.Logger
}

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
		} `json:"head"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
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
	ctx := r.Context()
	branch := fmt.Sprintf("pr-%d", p.Number)

	var err error
	switch p.Action {
	case "opened", "reopened", "synchronize":
		err = s.ensureBranch(ctx, log, p, branch)
	case "closed":
		err = s.destroyBranch(ctx, log, branch)
	default:
		log.Debug("ignoring pull_request action")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		log.Error("handling event failed", "err", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"branch": branch})
}

// ensureBranch makes branch exist (creating it from the configured source if
// missing). On synchronize with ResetOnPush, a pre-existing branch is reset
// to the source snapshot; a freshly created one is already pristine.
func (s *Service) ensureBranch(ctx context.Context, log *slog.Logger, p *payload, branch string) error {
	b, err := s.pg.GetBranch(ctx, branch)
	switch {
	case apiclient.IsNotFound(err):
		b, err = s.pg.CreateBranch(ctx, api.CreateBranchRequest{
			Name: branch, Source: s.cfg.Source, TTLSeconds: s.cfg.TTLSeconds,
		})
		if err != nil {
			return fmt.Errorf("create branch %s: %w", branch, err)
		}
		log.Info("branch created", "branch", branch, "source", s.cfg.Source)
	case err != nil:
		return fmt.Errorf("get branch %s: %w", branch, err)
	case p.Action == "synchronize" && s.cfg.ResetOnPush:
		if b, err = s.pg.ResetBranch(ctx, branch); err != nil {
			return fmt.Errorf("reset branch %s: %w", branch, err)
		}
		log.Info("branch reset on push", "branch", branch)
	default:
		log.Debug("branch already exists", "branch", branch)
	}
	s.maybeComment(ctx, log, p, b)
	return nil
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

// maybeComment posts the connect-info comment when a GitHub client is
// configured. Failures are logged, never fatal: the branch operation is the
// point of this service, the comment is a convenience.
func (s *Service) maybeComment(ctx context.Context, log *slog.Logger, p *payload, b *api.Branch) {
	if s.gh == nil {
		return
	}
	if err := s.gh.EnsureComment(ctx, p.Repository.FullName, p.Number, commentBody(s.cfg.ProxyHost, b)); err != nil {
		log.Warn("posting PR comment failed", "err", err)
	}
}

// Package ghook is a small GitHub webhook receiver that maps pull-request
// lifecycle events to pgoverlay branches (branch-per-PR): opened/reopened →
// ensure branch gh-<repo-key>-pr-<number> exists, synchronize → ensure (and
// optionally reset), closed → destroy. It talks to branchd through
// internal/apiclient and, when GitHub credentials are configured (App or PAT),
// reports back to the PR: a pgoverlay/branch commit status around every
// branch operation and a live connect-info comment kept current in place.
//
// Branch names come from pgoverlayconnect (PRBranchName, RefBranchName), the
// same functions the connect helpers use, so an app derives the name this
// service creates without asking it.
package ghook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/api"
	"github.com/abd-ulbasit/pgoverlay/internal/apiclient"
	"github.com/abd-ulbasit/pgoverlay/pgoverlayconnect"
)

// Config is the static service configuration (see cmd/pgoverlay-github for
// the GHOOK_* environment mapping).
type Config struct {
	WebhookSecret string   // HMAC key for X-Hub-Signature-256 (required)
	Source        string   // pgoverlay source to branch from (required)
	TTLSeconds    int      // branch TTL passed on create (0 = no TTL)
	ResetOnPush   bool     // synchronize resets the branch when true
	Repos         []string // "owner/name" allow-list; empty allows all
	ProxyHost     string   // host[:port] of the pgoverlay proxy, for comments
	// DiffOnPush, when true, posts a schema/data diff comment on opened/
	// synchronize after the branch is ready (opt-in, GHOOK_DIFF_ON_PUSH).
	// Only takes effect when a GitHub client is configured.
	DiffOnPush bool
	// BranchNaming picks the pgoverlay branch name for a pull request. Both
	// modes carry the repository key (pgoverlayconnect.RepoKey), so pull
	// requests of different repositories never share a branch:
	//   "pr-number" (default): gh-<key>-pr-<number>
	//   "git-branch": gh-<key>-<sanitized head ref> (feat/login -> gh-<key>-feat-login),
	//     falling back to the pr-number name for pull requests from forks and
	//     for refs with no letters or digits.
	// git-branch lets preview platforms derive the name from the git ref
	// they already know (Vercel's VERCEL_GIT_COMMIT_REF is present from the
	// very first build, before the PR association exists).
	BranchNaming string
}

// Service handles pull_request deliveries. Branch operations run detached
// from the delivery, one at a time per branch, in arrival order.
type Service struct {
	cfg Config
	pg  *apiclient.Client
	gh  *GitHub // nil when commenting is disabled
	log *slog.Logger
	wg  sync.WaitGroup // queued and in-flight detached branch operations

	// pollEvery is how often a branch that another operation is still
	// creating, resetting or destroying is re-read.
	pollEvery time.Duration

	mu     sync.Mutex
	queues map[string][]func() // per-branch pending work; a key exists while its worker runs
	seen   *deliveryCache
}

// opTimeout bounds one branch operation, including waiting for a branch
// another operation holds.
const opTimeout = 5 * time.Minute

// deliveryCacheSize is how many recent X-GitHub-Delivery ids are remembered
// for de-duplication.
const deliveryCacheSize = 4096

// Wait blocks until all detached branch operations have finished. Call after
// the HTTP server has shut down so in-flight work completes before exit.
func (s *Service) Wait() { s.wg.Wait() }

func New(cfg Config, pg *apiclient.Client, gh *GitHub, log *slog.Logger) *Service {
	return &Service{
		cfg: cfg, pg: pg, gh: gh, log: log,
		pollEvery: 2 * time.Second,
		queues:    map[string][]func(){},
		seen:      newDeliveryCache(deliveryCacheSize),
	}
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
			// Repo is the repository the head branch lives in: this
			// repository, or a fork. GitHub sends null once a fork is
			// deleted.
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
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

	// Delivery is the X-GitHub-Delivery id, for logs and failure statuses.
	Delivery string `json:"-"`
}

// fromFork reports whether the pull request's head branch lives in another
// repository. A fork's branch name is chosen by whoever owns the fork, so in
// git-branch mode it could be picked to equal a branch of this repository
// and take over that pull request's database; fork pull requests are named
// by number instead. A head repository GitHub no longer reports (a deleted
// fork) counts as a fork.
func (p *payload) fromFork() bool {
	h := p.PullRequest.Head.Repo
	return h == nil || !strings.EqualFold(h.FullName, p.Repository.FullName)
}

// maxWebhookBody caps the request body read BEFORE signature verification so
// an unauthenticated multi-GB POST can't exhaust memory. GitHub payloads are
// well under 1 MiB.
const maxWebhookBody = 1 << 20

func (s *Service) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// Limit before reading and before HMAC verification: the read itself is
	// the attack surface. ReadAll on an over-limit body returns an error (and
	// MaxBytesReader writes a 413 to w), so we reject without buffering it all.
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusRequestEntityTooLarge)
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
	p.Delivery = r.Header.Get("X-GitHub-Delivery")
	if !s.repoAllowed(p.Repository.FullName) {
		s.log.Info("ignoring repo not on allow-list", "repo", p.Repository.FullName, "delivery", p.Delivery)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.dispatch(w, &p)
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

func (s *Service) dispatch(w http.ResponseWriter, p *payload) {
	switch p.Action {
	case "opened", "reopened", "synchronize", "closed":
	default:
		s.log.Debug("ignoring pull_request action", "action", p.Action, "delivery", p.Delivery)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	branch := s.branchName(p)
	log := s.log.With("delivery", p.Delivery, "action", p.Action, "repo", p.Repository.FullName,
		"pr", p.Number, "head_sha", p.PullRequest.Head.SHA, "branch", branch)

	w.Header().Set("Content-Type", "application/json")
	// A delivery can arrive twice: GitHub and proxies retry, and the
	// Redeliver button reuses the id. Run each one once.
	if p.Delivery != "" && !s.seen.add(p.Delivery) {
		log.Info("ignoring duplicate delivery")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"branch": branch, "status": "duplicate"})
		return
	}

	// GitHub abandons webhook deliveries after ~10s, and an abandoned
	// request's canceled context would abort branchd's saga mid-flight —
	// branch creation/reset at pod speed routinely exceeds that deadline.
	// Ack the delivery now and run the operation detached, queued behind
	// earlier work on the same branch.
	payload := *p
	s.enqueue(branch, func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		switch payload.Action {
		case "opened", "reopened", "synchronize":
			s.handleEnsure(ctx, log, &payload, branch)
		case "closed":
			s.handleClosed(ctx, log, &payload, branch)
		}
	})

	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"branch": branch, "status": "accepted"})
}

// enqueue runs job after every job queued earlier for the same branch, so
// the deliveries for one pull request (opened, a burst of pushes, closed)
// apply in the order they arrived instead of racing: a reset issued while
// the create still runs would fail, and a status posted before the branch is
// ready would send CI to a database that is not there yet. Different
// branches run concurrently.
func (s *Service) enqueue(branch string, job func()) {
	s.wg.Add(1)
	s.mu.Lock()
	q, running := s.queues[branch]
	s.queues[branch] = append(q, job)
	s.mu.Unlock()
	if !running {
		go s.drain(branch)
	}
}

// drain runs the branch's queued jobs one by one and retires the queue once
// it is empty.
func (s *Service) drain(branch string) {
	for {
		s.mu.Lock()
		q := s.queues[branch]
		if len(q) == 0 {
			delete(s.queues, branch)
			s.mu.Unlock()
			return
		}
		job := q[0]
		q[0] = nil
		s.queues[branch] = q[1:]
		s.mu.Unlock()

		job()
		s.wg.Done()
	}
}

// deliveryCache remembers the most recent X-GitHub-Delivery ids. It is
// bounded (the oldest id is forgotten first) and in memory, so it stops
// retries and redeliveries, not a replay days later; handleClosed also asks
// GitHub whether the pull request is still closed.
type deliveryCache struct {
	mu   sync.Mutex
	ids  map[string]struct{}
	ring []string
	next int
}

func newDeliveryCache(n int) *deliveryCache {
	return &deliveryCache{ids: make(map[string]struct{}, n), ring: make([]string, n)}
}

// add records id and reports whether it was new.
func (c *deliveryCache) add(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.ids[id]; ok {
		return false
	}
	if old := c.ring[c.next]; old != "" {
		delete(c.ids, old)
	}
	c.ring[c.next] = id
	c.ids[id] = struct{}{}
	c.next = (c.next + 1) % len(c.ring)
	return true
}

// branchName derives the pgoverlay branch name for a pull request according
// to Config.BranchNaming, with the functions the connect helpers use:
// pr-number -> "gh-<key>-pr-<n>", git-branch -> "gh-<key>-<ref>". The key is
// derived from the repository, so pull request #7 of two allow-listed
// repositories gets two branches, and a close in one can never destroy the
// other's. git-branch mode falls back to the pr-number name for fork pull
// requests and when the sanitized ref comes up empty.
func (s *Service) branchName(p *payload) string {
	repo := p.Repository.FullName
	if s.cfg.BranchNaming == "git-branch" && !p.fromFork() {
		if n := pgoverlayconnect.RefBranchName(repo, p.PullRequest.Head.Ref); n != "" {
			return n
		}
	}
	return pgoverlayconnect.PRBranchName(repo, p.Number)
}

// handleEnsure brackets the branch operation with commit statuses on the PR
// head SHA — pending before, success/failure after — and keeps the live
// comment current (creating/resetting → ready / reset @ sha, or failed).
// GitHub-side failures are logged, never fatal: the branch operation is the
// point of this service.
func (s *Service) handleEnsure(ctx context.Context, log *slog.Logger, p *payload, branch string) {
	verb := "creating"
	if p.Action == "synchronize" && s.cfg.ResetOnPush {
		verb = "resetting"
	}
	s.setStatus(ctx, log, p, "pending", fmt.Sprintf("%s branch %s", verb, branch))
	s.upsertComment(ctx, log, p, commentMarker, commentBody(s.cfg.ProxyHost, branch, verb, nil))

	b, didReset, err := s.ensureBranch(ctx, log, p, branch)
	if err != nil {
		log.Error("handling event failed", "err", err)
		s.setStatus(ctx, log, p, "failure", publicReason(err, p.Delivery))
		s.upsertComment(ctx, log, p, commentMarker, commentBody(s.cfg.ProxyHost, branch, "failed (see the pgoverlay/branch status)", nil))
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
	s.upsertComment(ctx, log, p, commentMarker, commentBody(s.cfg.ProxyHost, branch, state, b))

	s.diffComment(ctx, log, p, branch)
}

// diffComment posts the schema/data diff comment when DiffOnPush is enabled
// and a GitHub client is configured. It runs after the branch is ready, asks
// branchd for the diff (no data sampling — the comment stays schema + delta
// table) and upserts it under the diff marker. Non-fatal on every error: the
// diff comment is a convenience, the branch operation is the point.
func (s *Service) diffComment(ctx context.Context, log *slog.Logger, p *payload, branch string) {
	if !s.cfg.DiffOnPush {
		return
	}
	gh := s.github(p)
	if gh == nil {
		return
	}
	res, err := s.pg.DiffBranch(ctx, branch, 0)
	if err != nil {
		log.Warn("diff for PR comment failed", "err", err)
		return
	}
	if err := gh.UpsertComment(ctx, p.Repository.FullName, p.Number, diffMarker, diffCommentBody(branch, res)); err != nil {
		log.Warn("posting diff comment failed", "err", err)
	}
}

// handleClosed destroys the branch and rewrites the live comment to record
// that (without a connect string — there is nothing left to connect to). No
// status: statuses on a closed PR don't matter.
func (s *Service) handleClosed(ctx context.Context, log *slog.Logger, p *payload, branch string) {
	if s.reopened(ctx, log, p) {
		return
	}
	if err := s.destroyBranch(ctx, log, branch); err != nil {
		log.Error("handling event failed", "err", err)
		return
	}
	gh := s.github(p)
	if gh == nil {
		return
	}
	body := commentBody(s.cfg.ProxyHost, branch, "destroyed", nil)
	if err := gh.UpdateComment(ctx, p.Repository.FullName, p.Number, commentMarker, body); err != nil {
		log.Warn("updating PR comment failed", "err", err)
	}
}

// reopened reports whether GitHub says the pull request is open again, which
// makes this closed delivery stale (a redelivery, a replayed request, or one
// that was queued behind the reopen): destroying the branch would pull it
// from under the open pull request. Without GitHub credentials, or when the
// lookup fails, the close goes ahead.
func (s *Service) reopened(ctx context.Context, log *slog.Logger, p *payload) bool {
	gh := s.github(p)
	if gh == nil {
		return false
	}
	state, err := gh.PullRequestState(ctx, p.Repository.FullName, p.Number)
	if err != nil {
		log.Warn("reading the pull request state failed; destroying the branch anyway", "err", err)
		return false
	}
	if state == "open" {
		log.Warn("pull request is open again; ignoring the stale closed delivery")
		return true
	}
	return false
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Branch states as branchd reports them (registry.BranchState).
const (
	stateCreating   = "creating"
	stateReady      = "ready"
	stateFailed     = "failed"
	stateResetting  = "resetting"
	stateDestroying = "destroying"
)

// ensureBranch makes branch exist and be ready, creating it from the
// configured source when missing. On synchronize with ResetOnPush a ready
// branch is reset (didReset reports that); a freshly created one is already
// pristine. A branch another operation is still creating, resetting or
// destroying is waited for first. A failed branch (its create or reset did
// not finish) is destroyed and created again: it cannot be reset and the
// proxy does not route to it. The returned branch is always ready.
func (s *Service) ensureBranch(ctx context.Context, log *slog.Logger, p *payload, branch string) (b *api.Branch, didReset bool, err error) {
	b, err = s.settledBranch(ctx, branch)
	switch {
	case apiclient.IsNotFound(err):
		b, err = s.createBranch(ctx, log, branch)
		return b, false, err
	case err != nil:
		return nil, false, err
	}
	switch b.State {
	case stateReady:
		if p.Action != "synchronize" || !s.cfg.ResetOnPush {
			log.Debug("branch already exists")
			return b, false, nil
		}
		if b, err = s.pg.ResetBranch(ctx, branch); err != nil {
			return nil, false, &opError{op: "reset branch " + branch, err: err}
		}
		log.Info("branch reset on push")
		b, err = s.requireReady(ctx, branch, b)
		return b, err == nil, err
	case stateFailed:
		log.Warn("branch is failed; destroying it and creating it again")
		if err := s.pg.DestroyBranch(ctx, branch); err != nil && !apiclient.IsNotFound(err) {
			return nil, false, &opError{op: "destroy failed branch " + branch, err: err}
		}
		b, err = s.createBranch(ctx, log, branch)
		return b, false, err
	default:
		return nil, false, &stateError{branch: branch, state: b.State}
	}
}

func (s *Service) createBranch(ctx context.Context, log *slog.Logger, branch string) (*api.Branch, error) {
	b, err := s.pg.CreateBranch(ctx, api.CreateBranchRequest{
		Name: branch, Source: s.cfg.Source, TTLSeconds: s.cfg.TTLSeconds,
	})
	if err != nil {
		return nil, &opError{op: "create branch " + branch, err: err}
	}
	log.Info("branch created", "source", s.cfg.Source)
	return s.requireReady(ctx, branch, b)
}

// requireReady returns b when it is ready; otherwise it waits for the
// operation still running on the branch and fails unless that leaves it
// ready.
func (s *Service) requireReady(ctx context.Context, branch string, b *api.Branch) (*api.Branch, error) {
	if b.State == stateReady {
		return b, nil
	}
	b, err := s.settledBranch(ctx, branch)
	if err != nil {
		return nil, err
	}
	if b.State != stateReady {
		return nil, &stateError{branch: branch, state: b.State}
	}
	return b, nil
}

// settledBranch reads branch, re-reading while another operation still runs
// on it (creating, resetting, destroying) until it settles or ctx ends. A
// missing branch returns an error apiclient.IsNotFound recognizes.
func (s *Service) settledBranch(ctx context.Context, branch string) (*api.Branch, error) {
	waiting := "" // the busy state seen on the previous read
	for {
		b, err := s.pg.GetBranch(ctx, branch)
		if err != nil {
			if waiting != "" && ctx.Err() != nil { // the deadline hit mid-read
				return nil, &stateError{branch: branch, state: waiting, timedOut: true}
			}
			return nil, &opError{op: "get branch " + branch, err: err}
		}
		switch b.State {
		case stateCreating, stateResetting, stateDestroying:
			waiting = b.State
		default:
			return b, nil
		}
		t := time.NewTimer(s.pollEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, &stateError{branch: branch, state: b.State, timedOut: true}
		case <-t.C:
		}
	}
}

// opError is a failed branchd call: op says which step ("create branch
// gh-…"), err is the cause.
type opError struct {
	op  string
	err error
}

func (e *opError) Error() string { return e.op + ": " + e.err.Error() }
func (e *opError) Unwrap() error { return e.err }

// stateError is a branch left in a state the operation cannot use, or one
// that did not settle in time. Its wording is ghook's own.
type stateError struct {
	branch, state string
	timedOut      bool
}

func (e *stateError) Error() string {
	if e.timedOut {
		return fmt.Sprintf("timed out waiting for branch %s (still %s)", e.branch, e.state)
	}
	return fmt.Sprintf("branch %s is %s, not ready", e.branch, e.state)
}

// publicReason is the commit-status description for a failed operation.
// Commit statuses are public on public repositories, so only messages whose
// wording is known are passed through: ghook's own state errors and
// branchd's deliberate 4xx answers (an invalid name, a quota, a conflict).
// Anything else, such as a transport error that names branchd's in-cluster
// address, stays in the log; the status points there by delivery id.
func publicReason(err error, delivery string) string {
	var (
		ste *stateError
		se  *apiclient.StatusError
		oe  *opError
	)
	if errors.As(err, &ste) || (errors.As(err, &se) && se.StatusCode >= 400 && se.StatusCode < 500) {
		return err.Error()
	}
	op := "branch operation"
	if errors.As(err, &oe) {
		op = oe.op
	}
	msg := op + " failed; see the pgoverlay-github logs"
	if delivery != "" {
		msg += " (delivery " + delivery + ")"
	}
	return msg
}

// setStatus posts a pgoverlay/branch commit status on the PR head SHA when a
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

// destroyBranch destroys branch once no other operation runs on it. A
// branch that is already gone is not an error.
func (s *Service) destroyBranch(ctx context.Context, log *slog.Logger, branch string) error {
	_, err := s.settledBranch(ctx, branch)
	if err == nil {
		err = s.pg.DestroyBranch(ctx, branch)
		if err != nil {
			err = &opError{op: "destroy branch " + branch, err: err}
		}
	}
	switch {
	case apiclient.IsNotFound(err):
		log.Debug("branch already gone")
	case err != nil:
		return err
	default:
		log.Info("branch destroyed")
	}
	return nil
}

// upsertComment writes body to the PR comment carrying marker when a GitHub
// client is configured. Failures are logged, never fatal: the branch
// operation is the point of this service, the comment is a convenience.
func (s *Service) upsertComment(ctx context.Context, log *slog.Logger, p *payload, marker, body string) {
	gh := s.github(p)
	if gh == nil {
		return
	}
	if err := gh.UpsertComment(ctx, p.Repository.FullName, p.Number, marker, body); err != nil {
		log.Warn("posting PR comment failed", "err", err)
	}
}

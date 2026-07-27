// pgoverlay-github is the branch-per-PR webhook service: it receives GitHub
// pull_request webhooks and drives branchd through its REST API — opened or
// reopened PRs get a branch pr-<number>, pushes optionally reset it, closing
// the PR destroys it. With GitHub credentials (App or PAT) it also sets the
// pgoverlay/branch commit status and keeps a live connect-info comment.
//
// Configuration is environment-only (GHOOK_*); see docs/github-app.md.
// Shutdown (SIGINT/SIGTERM) is graceful: in-flight deliveries finish.
package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/apiclient"
	"github.com/abd-ulbasit/pgoverlay/internal/ghook"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := ghook.LoadEnv(os.Getenv)
	if err != nil {
		return err
	}
	if len(cfg.Repos) == 0 {
		logger.Warn("GHOOK_REPOS is empty: webhooks from ANY repository sharing the secret are accepted")
	}

	var gh *ghook.GitHub
	switch {
	case cfg.AppID != "":
		key, err := ghook.ParseAppPrivateKey([]byte(cfg.AppPrivateKey))
		if err != nil {
			return err
		}
		app := ghook.NewAppAuth(cfg.AppID, key, cfg.GitHubAPI, nil)
		gh = &ghook.GitHub{BaseURL: cfg.GitHubAPI, Token: app.Token}
		logger.Info("GitHub App auth: minting installation tokens", "app_id", cfg.AppID)
	case cfg.GitHubToken != "":
		gh = &ghook.GitHub{BaseURL: cfg.GitHubAPI, Token: ghook.StaticToken(cfg.GitHubToken)}
	default:
		logger.Info("no GitHub credentials (GHOOK_APP_ID or GHOOK_GITHUB_TOKEN): PR comments and commit statuses disabled")
	}

	svc := ghook.New(cfg.Config, apiclient.New(cfg.PGOverlayServer, cfg.PGOverlayToken), gh, logger)
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           svc.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		logger.Info("pgoverlay-github listening", "addr", cfg.Listen,
			"pgoverlay", cfg.PGOverlayServer, "source", cfg.Source, "repos", cfg.Repos)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	svc.Wait()
	logger.Info("pgoverlay-github stopped")
	return nil
}

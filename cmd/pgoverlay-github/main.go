// pgoverlay-github is the branch-per-PR webhook service: it receives GitHub
// pull_request webhooks and drives branchd through its REST API — opened or
// reopened PRs get a branch gh-<repo-key>-pr-<number>, pushes optionally
// reset it, closing the PR destroys it. With GitHub credentials (App or PAT)
// it also sets the pgoverlay/branch commit status and keeps a live
// connect-info comment.
//
// Configuration is environment-only (GHOOK_*); see docs/github-app.md.
// Shutdown (SIGINT/SIGTERM) is graceful: in-flight deliveries finish.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/abd-ulbasit/pgoverlay/internal/apiclient"
	"github.com/abd-ulbasit/pgoverlay/internal/ghook"
	"github.com/abd-ulbasit/pgoverlay/internal/version"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

// newServer returns the webhook HTTP server. /webhook faces the internet and
// its body has to be read before the signature can be checked, so every
// phase of a request is time-bounded, not only the headers: a client that
// sends headers and then trickles the body (up to the 1 MiB cap) is cut off
// by ReadTimeout instead of holding a connection and a goroutine forever.
// The handler acknowledges within milliseconds (branch work runs detached),
// so the write and idle limits are generous. A GitHub delivery is well under
// 1 MiB and arrives in far less than 30 seconds.
func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}
}

func run() error {
	// Configuration is environment-only; the one flag prints the version.
	showVersion := flag.Bool("version", false, "print the pgoverlay-github version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("pgoverlay-github " + version.String())
		return nil
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	logger.Info("pgoverlay-github starting", "version", version.String())
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
	srv := newServer(cfg.Listen, svc.Handler())

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

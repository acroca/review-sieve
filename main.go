package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/acroca/review-sieve/internal/config"
	"github.com/acroca/review-sieve/internal/github"
	"github.com/acroca/review-sieve/internal/sieve"
)

const apiBaseURL = "https://api.github.com"

func redact(value string, cfg config.Config) string {
	tokens := []string{cfg.Token}
	for _, token := range cfg.OwnerTokens {
		tokens = append(tokens, token)
	}
	sort.Slice(tokens, func(i, j int) bool { return len(tokens[i]) > len(tokens[j]) })
	for _, token := range tokens {
		if token != "" {
			value = strings.ReplaceAll(value, token, "[REDACTED]")
		}
	}
	return value
}

func run(ctx context.Context, environ []string, apiBase string, stdout, stderr io.Writer) int {
	cfg, err := config.Parse(environ)
	if err != nil {
		fmt.Fprintf(stderr, "review-sieve: %v\n", err)
		return 1
	}

	client, err := github.New(apiBase, cfg.Token)
	if err != nil {
		fmt.Fprintf(stderr, "review-sieve: cannot authenticate with GITHUB_TOKEN: %v\n", err)
		return 1
	}
	login, err := client.User(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "review-sieve: cannot authenticate with GITHUB_TOKEN: %v\n", err)
		return 1
	}
	logger := slog.New(slog.NewTextHandler(stdout, nil))
	logger.Info("review-sieve started", "login", redact(login, cfg), "mode", redact(cfg.Mode, cfg),
		"poll_interval", redact(cfg.PollInterval.String(), cfg), "owner_tokens", redact(cfg.OwnerNames(), cfg))
	sieve.New(client, cfg, login, logger).Run(ctx)
	return 0
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Environ(), apiBaseURL, os.Stdout, os.Stderr))
}

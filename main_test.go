package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunInvalidConfigurationDoesNotRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	var out, errOut bytes.Buffer
	if code := run(context.Background(), nil, server.URL, &out, &errOut); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if got := errOut.String(); got != "review-sieve: GITHUB_TOKEN is required\n" {
		t.Fatalf("stderr = %q", got)
	}
	if requests.Load() != 0 {
		t.Fatal("invalid configuration made a network request")
	}
}

func TestStartupLogListsModeRules(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"login":"alice"}`)
		case "/notifications":
			fmt.Fprint(w, `[]`)
			cancel()
		}
	}))
	defer server.Close()
	var out, errOut bytes.Buffer
	env := []string{"GITHUB_TOKEN=test-token", "MODE_DONE=Other/Repo,Acme-Corp", "MODE_LOG=acme-corp/critical"}
	if code := run(ctx, env, server.URL, &out, &errOut); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, errOut.String())
	}
	for _, want := range []string{"mode=log ", "mode_done=acme-corp,other/repo ", `mode_read="" `, "mode_log=acme-corp/critical "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in startup log: %s", want, out.String())
		}
	}
}

func TestRunAuthenticationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rejected", http.StatusUnauthorized)
	}))
	defer server.Close()
	const token = "test-token-for-authentication"
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"GITHUB_TOKEN=" + token}, server.URL, &out, &errOut); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "cannot authenticate with GITHUB_TOKEN:") {
		t.Fatalf("missing authentication error: %s", errOut.String())
	}
	if strings.Contains(out.String()+errOut.String(), token) {
		t.Fatal("token appeared in output")
	}
}

func TestStartupLogRedactsTokenInLogin(t *testing.T) {
	const token = "test-token-in-login"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			fmt.Fprintf(w, `{"login":%q}`, token)
		case "/notifications":
			fmt.Fprint(w, `[]`)
			cancel()
		}
	}))
	defer server.Close()
	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"GITHUB_TOKEN=" + token}, server.URL, &out, &errOut); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, errOut.String())
	}
	if strings.Contains(out.String()+errOut.String(), token) {
		t.Fatal("token appeared in startup output")
	}
}

func TestRunProcessesAndCancels(t *testing.T) {
	const token = "test-token-for-notifications"
	marked := make(chan struct{}, 1)
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/user":
			fmt.Fprint(w, `{"login":"alice"}`)
		case r.URL.Path == "/notifications":
			fmt.Fprintf(w, `[{"id":"123","reason":"review_requested","updated_at":"2026-01-01T00:00:00Z","subject":{"type":"PullRequest","url":%q,"title":"Fix"},"repository":{"full_name":"acme/repo","owner":{"login":"acme"}}}]`, base+"/repos/acme/repo/pulls/42")
		case r.URL.Path == "/repos/acme/repo/pulls/42":
			fmt.Fprint(w, `{"number":42,"title":"Fix","user":{"login":"bob"},"assignees":[],"requested_reviewers":[]}`)
		case r.URL.Path == "/repos/acme/repo/pulls/42/reviews":
			fmt.Fprint(w, `[]`)
		case r.URL.Path == "/notifications/threads/123" && r.Method == http.MethodGet:
			fmt.Fprintf(w, `{"id":"123","reason":"review_requested","updated_at":"2026-01-01T00:00:00Z","subject":{"type":"PullRequest","url":%q,"title":"Fix"},"repository":{"full_name":"acme/repo","owner":{"login":"acme"}}}`, base+"/repos/acme/repo/pulls/42")
		case r.URL.Path == "/notifications/threads/123" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
			marked <- struct{}{}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base = server.URL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"GITHUB_TOKEN=" + token, "MODE=done", "POLL_INTERVAL=1h"}, base, &out, &errOut)
	}()
	select {
	case <-marked:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("thread was not marked done")
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code = %d, stderr = %s", code, errOut.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop after cancellation")
	}
	if !strings.Contains(out.String(), "review-sieve started") {
		t.Fatalf("missing startup log: %s", out.String())
	}
	if strings.Contains(out.String()+errOut.String(), token) {
		t.Fatal("token appeared in output")
	}
}

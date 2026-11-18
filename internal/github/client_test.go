package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(server.URL, "main-secret")
	if err != nil {
		t.Fatal(err)
	}
	return client, server
}

func TestHeadersPaginationAndConditional(t *testing.T) {
	var calls []string
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.RequestURI())
		token := "main-secret"
		if strings.Contains(r.URL.Path, "/pulls/") {
			token = "owner-secret"
		}
		for header, want := range map[string]string{
			"Authorization":        "Bearer " + token,
			"Accept":               "application/vnd.github+json",
			"X-GitHub-Api-Version": "2022-11-28",
			"User-Agent":           "review-sieve",
		} {
			if got := r.Header.Get(header); got != want {
				t.Errorf("%s = %q, want %q", header, got, want)
			}
		}
		switch r.URL.Path {
		case "/user":
			fmt.Fprint(w, `{"login":"alice"}`)
		case "/notifications":
			if r.Header.Get("If-Modified-Since") == "previous" {
				w.Header().Set("X-Poll-Interval", "120")
				w.WriteHeader(http.StatusNotModified)
				return
			}
			if r.URL.Query().Get("page") == "" {
				w.Header().Set("Last-Modified", "modified")
				w.Header().Set("X-Poll-Interval", "75")
				w.Header().Add("Link", `</notifications?page=2>; rel="last"`)
				w.Header().Add("Link", `</notifications?page=2>; rel="next"`)
				fmt.Fprint(w, `[{"id":"1","reason":"review_requested"}]`)
			} else {
				if r.Header.Get("If-Modified-Since") != "" {
					t.Error("pagination request must not be conditional")
				}
				fmt.Fprint(w, `[{"id":"2"}]`)
			}
		case "/repos/acme/app/pulls/42":
			fmt.Fprint(w, `{"number":42,"title":"Fix","user":{"login":"bob"},"assignees":[],"requested_reviewers":[]}`)
		case "/repos/acme/app/pulls/42/reviews":
			if r.URL.Query().Get("page") == "" {
				w.Header().Set("Link", `</repos/acme/app/pulls/42/reviews?page=2>; rel="next"`)
				fmt.Fprint(w, `[{"user":{"login":"eve"}}]`)
			} else {
				fmt.Fprint(w, `[{"user":{"login":"alice"}}]`)
			}
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx := context.Background()
	if user, err := client.User(ctx); err != nil || user != "alice" {
		t.Fatalf("User = %q, %v", user, err)
	}
	threads, modified, interval, notModified, err := client.Notifications(ctx, "")
	if err != nil || notModified || modified != "modified" || interval != 75*time.Second || len(threads) != 2 || threads[1].ID != "2" {
		t.Fatalf("Notifications = %#v %q %v %v %v", threads, modified, interval, notModified, err)
	}
	_, _, interval, notModified, err = client.Notifications(ctx, "previous")
	if err != nil || !notModified || interval != 120*time.Second {
		t.Fatalf("conditional Notifications = %v %v %v", interval, notModified, err)
	}
	subject := server.URL + "/repos/acme/app/pulls/42"
	pr, err := client.PullRequest(ctx, subject, "owner-secret")
	if err != nil || pr.Number != 42 || pr.User.Login != "bob" {
		t.Fatalf("PullRequest = %#v, %v", pr, err)
	}
	reviews, err := client.Reviews(ctx, subject, "owner-secret")
	if err != nil || len(reviews) != 2 || reviews[1].User.Login != "alice" {
		t.Fatalf("Reviews = %#v, %v", reviews, err)
	}
	if len(calls) != 7 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestAccessDeniedAndSafeErrors(t *testing.T) {
	for _, tc := range []struct {
		name, remaining, retry, body string
		status                       int
		denied                       bool
	}{
		{"404", "", "", `{"message":"main-secret"}`, 404, true},
		{"403", "", "", "", 403, true},
		{"403 exhausted", "0", "", "", 403, false},
		{"403 retry", "", "60", "", 403, false},
		{"403 body", "", "", `{"message":"secondary rate limit"}`, 403, true},
		{"404 exhausted", "0", "60", "", 404, true},
		{"429", "", "", "", 429, false},
		{"500", "", "", "", 500, false},
		{"401", "", "", "", 401, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.remaining != "" {
					w.Header().Set("X-RateLimit-Remaining", tc.remaining)
				}
				if tc.retry != "" {
					w.Header().Set("Retry-After", tc.retry)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			_, err := client.PullRequest(context.Background(), server.URL+"/pull?secret=owner-secret", "owner-secret")
			if err == nil || IsAccessDenied(err) != tc.denied || strings.Contains(err.Error(), "secret") {
				t.Fatalf("error = %v, denied = %v", err, IsAccessDenied(err))
			}
			if IsAccessDenied(fmt.Errorf("wrapped: %w", err)) != tc.denied {
				t.Fatal("wrapped error classification differs")
			}
		})
	}
}

func TestRejectForeignURLsAndRedirects(t *testing.T) {
	var foreignRequests atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
	}))
	defer foreign.Close()
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/notifications" {
			w.Header().Set("Link", "<"+foreign.URL+"/next>; rel=\"next\"")
			fmt.Fprint(w, `[]`)
			return
		}
		http.Redirect(w, r, foreign.URL+"/owner-secret", http.StatusFound)
	})
	for _, raw := range []string{foreign.URL + "/pull", "file:///pull", server.URL + "@evil.example/pull"} {
		if _, err := client.PullRequest(context.Background(), raw, "owner-secret"); err == nil || strings.Contains(err.Error(), "owner-secret") {
			t.Fatalf("PullRequest(%q) error = %v", raw, err)
		}
		if _, err := client.Reviews(context.Background(), raw, "owner-secret"); err == nil {
			t.Fatalf("Reviews(%q) succeeded", raw)
		}
	}
	if _, err := client.PullRequest(context.Background(), server.URL+"/redirect?token=owner-secret", "owner-secret"); err == nil || strings.Contains(err.Error(), "owner-secret") {
		t.Fatalf("redirect error = %v", err)
	}
	if _, _, _, _, err := client.Notifications(context.Background(), ""); err == nil {
		t.Fatal("foreign pagination succeeded")
	}
	if foreignRequests.Load() != 0 {
		t.Fatalf("foreign server received %d requests", foreignRequests.Load())
	}
}

func TestMarkAndIDValidation(t *testing.T) {
	var calls []string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+"main-secret" {
			t.Error("mark did not use main token")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	for _, mode := range []string{"read", "done"} {
		if err := client.Mark(context.Background(), "123", mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"", "../123", "123/other", "123%2Fother", "a", "1?x=2"} {
		if err := client.Mark(context.Background(), id, "read"); err == nil {
			t.Fatalf("accepted invalid id %q", id)
		}
	}
	for _, mode := range []string{"log", "unsubscribe", "unknown"} {
		if err := client.Mark(context.Background(), "123", mode); err == nil {
			t.Fatalf("accepted mode %q", mode)
		}
	}
	if got := strings.Join(calls, ","); got != "PATCH /notifications/threads/123,DELETE /notifications/threads/123" {
		t.Fatal(got)
	}
}

func TestMarkNeverFollowsRedirects(t *testing.T) {
	var calls atomic.Int32
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/other", http.StatusTemporaryRedirect)
	})
	for _, mode := range []string{"read", "done"} {
		if err := client.Mark(context.Background(), "123", mode); err == nil {
			t.Fatalf("%s redirect was accepted", mode)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("unexpected redirected requests: %d", calls.Load())
	}
}

func TestMalformedResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		reviews    bool
	}{
		{"missing assignees", `{"number":1,"user":{"login":"a"},"requested_reviewers":[]}`, false},
		{"missing reviewers", `{"number":1,"user":{"login":"a"},"assignees":[]}`, false},
		{"null reviewers", `{"number":1,"user":{"login":"a"},"assignees":[],"requested_reviewers":null}`, false},
		{"bad user", `{"number":1,"user":{"login":"a"},"assignees":[{}],"requested_reviewers":[]}`, false},
		{"null reviews", `null`, true},
		{"truncated reviews", `[`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) })
			var err error
			if tc.reviews {
				_, err = client.Reviews(context.Background(), server.URL+"/pull", "")
			} else {
				_, err = client.PullRequest(context.Background(), server.URL+"/pull", "")
			}
			if err == nil {
				t.Fatal("malformed response accepted")
			}
		})
	}
}

// GitHub returns a null user for content left behind by a deleted account.
func TestDeletedAccountsAreAccepted(t *testing.T) {
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/reviews") {
			fmt.Fprint(w, `[{"user":null},{"user":{"login":"eve"}}]`)
			return
		}
		fmt.Fprint(w, `{"number":1,"title":"Fix","user":null,"assignees":[],"requested_reviewers":[]}`)
	})
	pr, err := client.PullRequest(context.Background(), server.URL+"/pull", "")
	if err != nil || pr.Number != 1 || pr.User.Login != "" {
		t.Fatalf("pull request by deleted account: %+v, %v", pr, err)
	}
	reviews, err := client.Reviews(context.Background(), server.URL+"/pull", "")
	if err != nil || len(reviews) != 2 || reviews[0].User.Login != "" || reviews[1].User.Login != "eve" {
		t.Fatalf("review by deleted account: %+v, %v", reviews, err)
	}
}

func TestPaginationCycleAndContext(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `</notifications>; rel="next"`)
		fmt.Fprint(w, `[]`)
	})
	if _, _, _, _, err := client.Notifications(context.Background(), ""); err == nil {
		t.Fatal("pagination cycle accepted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.User(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestTransportErrorsAndInvalidBaseAreSafe(t *testing.T) {
	for _, raw := range []string{"file:///secret", "http://" + "secret" + "@example.com", "http://example.com/?token=secret", "://secret"} {
		if _, err := New(raw, "secret"); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid base error = %v", err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()
	client, err := New(server.URL, "main-secret")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.PullRequest(context.Background(), server.URL+"/pull?token=owner-secret", "owner-secret")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe transport error = %v", err)
	}
}

func TestRequestTimeoutAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		bodyStarted bool
		cancel      bool
	}{
		{name: "timeout waiting for headers"},
		{name: "timeout reading body", bodyStarted: true},
		{name: "context deadline reading body", bodyStarted: true, cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.bodyStarted {
					fmt.Fprint(w, `{"login":`)
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			})
			defer close(release)
			if client.http.Timeout != 30*time.Second {
				t.Fatalf("unexpected default timeout: %v", client.http.Timeout)
			}
			client.http.Timeout = 80 * time.Millisecond
			ctx := context.Background()
			if tc.cancel {
				client.http.Timeout = 2 * time.Second
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 80*time.Millisecond)
				defer cancel()
			}
			start := time.Now()
			_, err := client.User(ctx)
			if err == nil || strings.Contains(err.Error(), "main-secret") {
				t.Fatalf("invalid timeout error: %v", err)
			}
			if tc.cancel && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected context deadline, got %v", err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("request took too long: %v", elapsed)
			}
		})
	}
}

func TestGetThread(t *testing.T) {
	var calls int
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/notifications/threads/123" ||
			r.Header.Get("Authorization") != "Bearer "+"main-secret" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"id":"123","updated_at":"2026-01-01T00:00:00Z","reason":"review_requested","subject":{"type":"PullRequest","url":"https://api.github.com/repos/a/b/pulls/1"}}`)
	})
	thread, err := client.GetThread(context.Background(), "123")
	if err != nil || thread.ID != "123" || thread.Reason != "review_requested" || thread.UpdatedAt == "" {
		t.Fatalf("GetThread = %#v, %v", thread, err)
	}
	for _, id := range []string{"", "../123", "123/other", "123%2Fother", "a", "1?x=2"} {
		if _, err := client.GetThread(context.Background(), id); err == nil {
			t.Fatalf("accepted invalid id %q", id)
		}
	}
	if calls != 1 {
		t.Fatalf("got %d requests, want 1", calls)
	}
}

func TestGetThreadRejectsMalformedResponses(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`null`, http.StatusOK},
		{`[]`, http.StatusOK},
		{`{}`, http.StatusOK},
		{`{"id":"123"}`, http.StatusOK},
		{`{"id":"124","updated_at":"2026-01-01T00:00:00Z"}`, http.StatusOK},
		{`{"id":"123","updated_at":"2026-01-01T00:00:00Z"}`, http.StatusNoContent},
		{`{"message":"main-secret"}`, http.StatusNotFound},
	} {
		t.Run(fmt.Sprintf("%d_%s", tc.status, tc.body), func(t *testing.T) {
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			_, err := client.GetThread(context.Background(), "123")
			if err == nil || strings.Contains(err.Error(), "main-secret") {
				t.Fatalf("invalid response error = %v", err)
			}
			if tc.status == http.StatusNotFound && !IsAccessDenied(err) {
				t.Fatalf("404 not recognized as access denied: %v", err)
			}
		})
	}
}

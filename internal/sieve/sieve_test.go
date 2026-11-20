package sieve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acroca/review-sieve/internal/config"
	"github.com/acroca/review-sieve/internal/github"
)

func TestCandidate(t *testing.T) {
	tests := []struct {
		name, reason, typ, url string
		want                   bool
	}{
		{"review", "review_requested", "PullRequest", "/pulls/1", true},
		{"mention", "mention", "PullRequest", "/pulls/1", false},
		{"issue", "review_requested", "Issue", "/issues/1", false},
		{"no URL", "review_requested", "PullRequest", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Candidate(github.Thread{Reason: tt.reason, Subject: github.Subject{Type: tt.typ, URL: tt.url}})
			if got != tt.want {
				t.Fatalf("Candidate = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTeamOnly(t *testing.T) {
	base := github.PullRequest{Number: 1, Title: "title", User: github.User{Login: "author"}, Assignees: []github.User{}, RequestedReviewers: []github.User{}}
	tests := []struct {
		name   string
		change func(*github.PullRequest) []github.Review
		login  string
		want   bool
	}{
		{"no stake", func(*github.PullRequest) []github.Review { return []github.Review{} }, "me", true},
		{"reviewer", func(p *github.PullRequest) []github.Review {
			p.RequestedReviewers = []github.User{{Login: "ME"}}
			return []github.Review{}
		}, "me", false},
		{"author", func(p *github.PullRequest) []github.Review { p.User.Login = "ME"; return []github.Review{} }, "me", false},
		{"assignee", func(p *github.PullRequest) []github.Review {
			p.Assignees = []github.User{{Login: "ME"}}
			return []github.Review{}
		}, "me", false},
		{"reviewed", func(*github.PullRequest) []github.Review { return []github.Review{{User: github.User{Login: "ME"}}} }, "me", false},
		{"deleted author", func(p *github.PullRequest) []github.Review { p.User.Login = ""; return []github.Review{} }, "me", true},
		{"review by deleted account", func(*github.PullRequest) []github.Review { return []github.Review{{}} }, "me", true},
		{"underscore logins", func(p *github.PullRequest) []github.Review {
			p.User.Login = "ann_corp"
			return []github.Review{{User: github.User{Login: "bob_corp"}}}
		}, "me_corp", true},
		{"underscore login requested", func(p *github.PullRequest) []github.Review {
			p.RequestedReviewers = []github.User{{Login: "ME_corp"}}
			return []github.Review{}
		}, "me_corp", false},
		{"bot author", func(p *github.PullRequest) []github.Review {
			p.User.Login = "dependabot[bot]"
			return []github.Review{}
		}, "me", true},
		{"missing reviewers", func(p *github.PullRequest) []github.Review { p.RequestedReviewers = nil; return []github.Review{} }, "me", false},
		{"missing assignees", func(p *github.PullRequest) []github.Review { p.Assignees = nil; return []github.Review{} }, "me", false},
		{"missing reviews", func(*github.PullRequest) []github.Review { return nil }, "me", false},
		{"missing reviewer login", func(p *github.PullRequest) []github.Review {
			p.RequestedReviewers = []github.User{{}}
			return []github.Review{}
		}, "me", false},
		{"missing login", func(*github.PullRequest) []github.Review { return []github.Review{} }, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := base
			reviews := tt.change(&p)
			if got := TeamOnly(p, reviews, tt.login); got != tt.want {
				t.Fatalf("TeamOnly = %v, want %v", got, tt.want)
			}
		})
	}
}

type testServer struct {
	mu       sync.Mutex
	server   *httptest.Server
	threads  []github.Thread
	pr       github.PullRequest
	reviews  []github.Review
	failures map[string][]int
	requests []string
	tokens   []string
	marked   []string
	interval string
	modified string
	current  *github.Thread
	// hook runs for every request before the server state is locked, so it can
	// observe or delay requests that arrive at the same time.
	hook func(*http.Request)
}

// addThreads appends copies of the first thread with IDs 2..n.
func (f *testServer) addThreads(n int) {
	for i := 2; i <= n; i++ {
		thread := f.threads[0]
		thread.ID = fmt.Sprint(i)
		f.threads = append(f.threads, thread)
	}
}

// track returns a hook body that records how many matching requests overlap.
func track(match func(*http.Request) bool, hold time.Duration) (hook func(*http.Request), peak *atomic.Int32) {
	var inFlight atomic.Int32
	peak = new(atomic.Int32)
	return func(r *http.Request) {
		if !match(r) {
			return
		}
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(hold)
	}, peak
}

func isPullRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/1")
}

func isMark(r *http.Request) bool {
	return r.Method != http.MethodGet && strings.HasPrefix(r.URL.Path, "/notifications/threads/")
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	f := &testServer{
		pr:      github.PullRequest{Number: 1, Title: "test PR", User: github.User{Login: "author"}, Assignees: []github.User{}, RequestedReviewers: []github.User{}},
		reviews: []github.Review{}, failures: make(map[string][]int),
		interval: "90", modified: "Mon, 02 Jan 2006 15:04:05 GMT",
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	f.threads = []github.Thread{{ID: "1", Reason: "review_requested", UpdatedAt: "2026-01-01T00:00:00Z", Subject: github.Subject{Type: "PullRequest", URL: f.server.URL + "/repos/acme/repo/pulls/1", Title: "test PR"}, Repository: github.Repository{FullName: "acme/repo", Owner: github.User{Login: "acme"}}}}
	return f
}

func (f *testServer) serve(w http.ResponseWriter, r *http.Request) {
	if f.hook != nil {
		f.hook(r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	f.requests = append(f.requests, r.Method+" "+path+" "+r.Header.Get("If-Modified-Since"))
	f.tokens = append(f.tokens, r.Header.Get("Authorization"))
	failureKey := r.Method + " " + path
	if len(f.failures[failureKey]) == 0 {
		failureKey = path
	}
	if failures := f.failures[failureKey]; len(failures) > 0 {
		f.failures[failureKey] = failures[1:]
		if failures[0] == 429 {
			w.Header().Set("Retry-After", "1")
		}
		w.WriteHeader(failures[0])
		return
	}
	switch {
	case path == "/notifications":
		w.Header().Set("X-Poll-Interval", f.interval)
		w.Header().Set("Last-Modified", f.modified)
		if r.Header.Get("If-Modified-Since") == f.modified {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_ = json.NewEncoder(w).Encode(f.threads)
	case strings.HasSuffix(path, "/reviews"):
		_ = json.NewEncoder(w).Encode(f.reviews)
	case strings.HasPrefix(path, "/repos/"):
		_ = json.NewEncoder(w).Encode(f.pr)
	case strings.HasPrefix(path, "/notifications/threads/"):
		if r.Method == http.MethodGet {
			if f.current != nil {
				_ = json.NewEncoder(w).Encode(f.current)
				return
			}
			listed := f.threads[0]
			for _, thread := range f.threads {
				if thread.ID == strings.TrimPrefix(path, "/notifications/threads/") {
					listed = thread
				}
			}
			_ = json.NewEncoder(w).Encode(listed)
			return
		}
		f.marked = append(f.marked, r.Method+" "+path)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *testServer) newSieve(t *testing.T, mode string) (*Sieve, *bytes.Buffer) {
	t.Helper()
	client, err := github.New(f.server.URL, "main-token")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	cfg := config.Config{Token: "main-token", Mode: mode, PollInterval: time.Minute, OwnerTokens: map[string]string{"ACME": "owner-token"}}
	s := New(client, cfg, "me", slog.New(slog.NewTextHandler(&logs, nil)))
	// Tests run unpaced unless they are about pacing.
	s.lookupPace, s.markPace = 0, 0
	return s, &logs
}

// arrivals returns a hook that records when matching requests arrive, and a
// function reporting the shortest gap between two consecutive arrivals.
func arrivals(match func(*http.Request) bool) (hook func(*http.Request), shortestGap func() (time.Duration, int)) {
	var mu sync.Mutex
	var times []time.Time
	hook = func(r *http.Request) {
		if match(r) {
			mu.Lock()
			times = append(times, time.Now())
			mu.Unlock()
		}
	}
	return hook, func() (time.Duration, int) {
		mu.Lock()
		defer mu.Unlock()
		shortest := time.Duration(1<<63 - 1)
		for i := 1; i < len(times); i++ {
			shortest = min(shortest, times[i].Sub(times[i-1]))
		}
		return shortest, len(times)
	}
}

func TestPollModesAndDeduplication(t *testing.T) {
	for _, mode := range []string{"log", "read", "done"} {
		t.Run(mode, func(t *testing.T) {
			f := newTestServer(t)
			s, logs := f.newSieve(t, mode)
			wait, err := s.Poll(context.Background())
			if err != nil || wait != 90*time.Second {
				t.Fatalf("Poll = %v, %v", wait, err)
			}
			// A later full list must not re-evaluate an unchanged thread.
			f.modified = "Tue, 03 Jan 2006 15:04:05 GMT"
			if _, err := s.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if count := strings.Count(logs.String(), "team-only review request"); count != 1 {
				t.Fatalf("got %d logs: %s", count, logs)
			}
			action := mode
			if mode == "log" {
				action = "logged"
			}
			if !strings.Contains(logs.String(), "action="+action) {
				t.Fatalf("missing action %s: %s", action, logs)
			}
			if mode == "log" && len(f.marked) != 0 {
				t.Fatalf("unexpected marks: %v", f.marked)
			}
			if mode != "log" {
				method := map[string]string{"read": "PATCH", "done": "DELETE"}[mode]
				if len(f.marked) != 1 || f.marked[0] != method+" /notifications/threads/1" {
					t.Fatalf("marks = %v", f.marked)
				}
			}
			if len(f.tokens) < 3 || !strings.Contains(f.tokens[0], "main-token") || !strings.Contains(f.tokens[1], "owner-token") || !strings.Contains(f.tokens[2], "owner-token") {
				t.Fatalf("wrong tokens: %v", f.tokens)
			}
		})
	}
}

func TestPollUsesModeRulePerRepository(t *testing.T) {
	f := newTestServer(t)
	for id, repo := range map[string]string{"1": "Acme/Repo", "2": "acme/critical", "3": "other/repo", "4": "unlisted/repo"} {
		owner, _, _ := strings.Cut(repo, "/")
		thread := f.threads[0]
		thread.ID = id
		thread.Subject.URL = f.server.URL + "/repos/" + repo + "/pulls/1"
		thread.Repository = github.Repository{FullName: repo, Owner: github.User{Login: owner}}
		if id == "1" {
			f.threads[0] = thread
		} else {
			f.threads = append(f.threads, thread)
		}
	}
	s, logs := f.newSieve(t, "log")
	s.cfg.Rules = map[string]string{"acme": "done", "acme/critical": "log", "other/repo": "read"}
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The exception inside a "done" owner and the unlisted repository are
	// only logged; nothing may be marked for them.
	sort.Strings(f.marked)
	if want := []string{"DELETE /notifications/threads/1", "PATCH /notifications/threads/3"}; !reflect.DeepEqual(f.marked, want) {
		t.Fatalf("marks = %v, want %v", f.marked, want)
	}
	for _, want := range []string{
		"repo=Acme/Repo pr=1 title=\"test PR\" action=done",
		"repo=acme/critical pr=1 title=\"test PR\" action=logged",
		"repo=other/repo pr=1 title=\"test PR\" action=read",
		"repo=unlisted/repo pr=1 title=\"test PR\" action=logged",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("missing %q in logs: %s", want, logs)
		}
	}
}

func TestPollRetries(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		status     int
	}{
		{"pull request", "/repos/acme/repo/pulls/1", 500},
		{"reviews", "/repos/acme/repo/pulls/1/reviews", 500},
		{"rate limit", "/repos/acme/repo/pulls/1", 429},
		{"mark", "PATCH /notifications/threads/1", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestServer(t)
			f.failures[tc.path] = []int{tc.status}
			s, logs := f.newSieve(t, "read")
			for i := 0; i < 2; i++ {
				if _, err := s.Poll(context.Background()); err != nil {
					t.Fatal(err)
				}
				if i == 0 && len(f.marked) != 0 {
					t.Fatalf("marked despite injected failure: %v", f.requests)
				}
			}
			if len(f.marked) != 1 {
				t.Fatalf("thread not retried: requests=%v logs=%s", f.requests, logs)
			}
			if !strings.Contains(logs.String(), "could not evaluate notification") || strings.Contains(logs.String(), "level=WARN") {
				t.Fatalf("unexpected logs: %s", logs)
			}
			var lists []string
			for _, r := range f.requests {
				if strings.HasPrefix(r, "GET /notifications ") {
					lists = append(lists, r)
				}
			}
			if len(lists) != 2 || lists[1] != "GET /notifications " {
				t.Fatalf("retry was conditional: %v", lists)
			}
			if tc.name == "mark" {
				writes := 0
				for _, request := range f.requests {
					if request == "PATCH /notifications/threads/1 " {
						writes++
					}
				}
				if writes != 2 {
					t.Fatalf("expected failed PATCH and retry, got %d: %v", writes, f.requests)
				}
			}
		})
	}
}

func TestPermissionWarningOnce(t *testing.T) {
	for _, ownerToken := range []bool{false, true} {
		t.Run(fmt.Sprint(ownerToken), func(t *testing.T) {
			f := newTestServer(t)
			f.failures["/repos/acme/repo/pulls/1"] = []int{404, 403}
			other := f.threads[0]
			other.ID = "2"
			f.threads = append(f.threads, other)
			s, logs := f.newSieve(t, "read")
			if !ownerToken {
				s.cfg.OwnerTokens = nil
			}
			for i := 0; i < 2; i++ {
				if _, err := s.Poll(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			expected := "cannot read acme/repo with GITHUB_TOKEN; set GITHUB_TOKEN_ACME"
			if ownerToken {
				expected = "GITHUB_TOKEN_ACME cannot read acme/repo"
			}
			if strings.Count(logs.String(), expected) != 1 || len(f.marked) != 0 {
				t.Fatalf("unexpected logs/marks: %s %v", logs, f.marked)
			}
		})
	}
}

func TestActivityAndForgetting(t *testing.T) {
	f := newTestServer(t)
	s, logs := f.newSieve(t, "log")
	poll := func() {
		t.Helper()
		if _, err := s.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	poll()
	f.threads[0].UpdatedAt = "2026-01-01T00:01:00Z"
	f.modified = "second"
	poll()
	saved := f.threads
	f.threads = []github.Thread{}
	f.modified = "third"
	poll()
	f.threads = saved
	f.modified = "fourth"
	poll()
	if count := strings.Count(logs.String(), "team-only review request"); count != 3 {
		t.Fatalf("got %d matches: %s", count, logs)
	}
}

func TestNonCandidatesAndUnsafeData(t *testing.T) {
	f := newTestServer(t)
	f.threads[0].Reason = "mention"
	issue := f.threads[0]
	issue.ID, issue.Reason, issue.Subject.Type = "2", "review_requested", "Issue"
	missing := f.threads[0]
	missing.ID, missing.Reason, missing.Repository.Owner.Login = "3", "review_requested", ""
	f.threads = append(f.threads, issue, missing)
	s, _ := f.newSieve(t, "read")
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 1 || len(f.marked) != 0 {
		t.Fatalf("unexpected requests: %v", f.requests)
	}
}

func TestMalformedNotificationAndData(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testServer)
	}{
		{"missing id", func(f *testServer) { f.threads[0].ID = "" }},
		{"invalid id", func(f *testServer) { f.threads[0].ID = "abc" }},
		{"missing timestamp", func(f *testServer) { f.threads[0].UpdatedAt = "" }},
		{"malformed timestamp", func(f *testServer) { f.threads[0].UpdatedAt = "not-a-time" }},
		{"wrong repository", func(f *testServer) { f.threads[0].Repository.FullName = "other/repo" }},
		{"wrong subject repository", func(f *testServer) { f.threads[0].Subject.URL = f.server.URL + "/repos/other/repo/pulls/1" }},
		{"wrong subject number", func(f *testServer) { f.pr.Number = 2 }},
		{"missing title", func(f *testServer) { f.pr.Title = "" }},
		{"duplicate conflicting id", func(f *testServer) {
			other := f.threads[0]
			other.UpdatedAt = "different"
			f.threads = append(f.threads, other)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestServer(t)
			tt.change(f)
			s, _ := f.newSieve(t, "read")
			for i := 0; i < 2; i++ {
				if _, err := s.Poll(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if len(f.marked) != 0 {
				t.Fatalf("unsafe thread marked: %v", f.marked)
			}
			if f.requests[len(f.requests)-1] == "GET /notifications "+f.modified {
				t.Fatalf("unsafe thread was cached: %v", f.requests)
			}
		})
	}
}

func TestLogsRedactTokensInMetadata(t *testing.T) {
	f := newTestServer(t)
	f.pr.Title = "owner-token and main-token"
	s, logs := f.newSieve(t, "log")
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "owner-token") || strings.Contains(logs.String(), "main-token") {
		t.Fatalf("token leaked: %s", logs)
	}
}

func TestPollLeavesInvolvedUsersAlone(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testServer)
	}{
		{"reviewer", func(f *testServer) { f.pr.RequestedReviewers = []github.User{{Login: "ME"}} }},
		{"author", func(f *testServer) { f.pr.User.Login = "ME" }},
		{"assignee", func(f *testServer) { f.pr.Assignees = []github.User{{Login: "ME"}} }},
		{"reviewed", func(f *testServer) { f.reviews = []github.Review{{User: github.User{Login: "ME"}}} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestServer(t)
			tt.change(f)
			s, logs := f.newSieve(t, "read")
			if _, err := s.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.marked) != 0 || strings.Contains(logs.String(), "team-only review request") {
				t.Fatalf("involved user was marked: %v %s", f.marked, logs)
			}
		})
	}
}

func TestPollAcceptsUnusualAccounts(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testServer)
	}{
		{"underscore author", func(f *testServer) { f.pr.User.Login = "ann_corp" }},
		{"deleted author", func(f *testServer) { f.pr.User.Login = "" }},
		{"underscore reviewer", func(f *testServer) { f.reviews = []github.Review{{User: github.User{Login: "bob_corp"}}} }},
		{"review by deleted account", func(f *testServer) { f.reviews = []github.Review{{}} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestServer(t)
			tt.change(f)
			s, logs := f.newSieve(t, "read")
			if _, err := s.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.marked) != 1 || strings.Contains(logs.String(), "level=ERROR") {
				t.Fatalf("team-only thread was not handled: %v %s", f.marked, logs)
			}
		})
	}
}

func TestPollSkipsReviewsWhenAlreadyInvolved(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testServer)
	}{
		{"reviewer", func(f *testServer) { f.pr.RequestedReviewers = []github.User{{Login: "ME"}} }},
		{"author", func(f *testServer) { f.pr.User.Login = "ME" }},
		{"assignee", func(f *testServer) { f.pr.Assignees = []github.User{{Login: "ME"}} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestServer(t)
			tt.change(f)
			s, _ := f.newSieve(t, "read")
			if _, err := s.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, request := range f.requests {
				if strings.Contains(request, "/reviews") {
					t.Fatalf("reviews fetched although the pull request already shows involvement: %v", f.requests)
				}
			}
			if len(f.marked) != 0 {
				t.Fatalf("involved user was marked: %v", f.marked)
			}
		})
	}
}

func TestPollLooksUpThreadsConcurrently(t *testing.T) {
	f := newTestServer(t)
	f.addThreads(12)
	hook, peak := track(isPullRequest, 30*time.Millisecond)
	f.hook = hook
	s, logs := f.newSieve(t, "log")
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got < 2 || got > lookupWorkers {
		t.Fatalf("peak concurrent lookups = %d, want between 2 and %d", got, lookupWorkers)
	}
	if count := strings.Count(logs.String(), "team-only review request"); count != 12 {
		t.Fatalf("got %d matches, want 12: %s", count, logs)
	}
}

func TestPollMarksOneAtATime(t *testing.T) {
	f := newTestServer(t)
	f.addThreads(6)
	hook, peak := track(isMark, 10*time.Millisecond)
	f.hook = hook
	s, logs := f.newSieve(t, "read")
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got != 1 {
		t.Fatalf("peak concurrent marks = %d, want 1", got)
	}
	if len(f.marked) != 6 {
		t.Fatalf("got %d marks, want 6: %v %s", len(f.marked), f.marked, logs)
	}
}

func TestPollPacesLookups(t *testing.T) {
	f := newTestServer(t)
	f.addThreads(4)
	hook, shortestGap := arrivals(isPullRequest)
	f.hook = hook
	s, _ := f.newSieve(t, "log")
	s.lookupPace = 60 * time.Millisecond
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Half the pace allows for jitter between sending and arriving.
	if gap, n := shortestGap(); n != 4 || gap < 30*time.Millisecond {
		t.Fatalf("%d lookups, shortest gap %v, want 4 lookups at least 30ms apart", n, gap)
	}
}

func TestPollPacesMarks(t *testing.T) {
	f := newTestServer(t)
	f.addThreads(4)
	hook, shortestGap := arrivals(isMark)
	f.hook = hook
	s, _ := f.newSieve(t, "done")
	s.markPace = 60 * time.Millisecond
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gap, n := shortestGap(); n != 4 || gap < 30*time.Millisecond {
		t.Fatalf("%d marks, shortest gap %v, want 4 marks at least 30ms apart", n, gap)
	}
}

func TestPollStopsWhenCancelledAndResumes(t *testing.T) {
	f := newTestServer(t)
	f.addThreads(6)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.hook = func(r *http.Request) {
		if isPullRequest(r) {
			cancel()
		}
	}
	s, logs := f.newSieve(t, "log")
	s.lookupPace = time.Hour
	done := make(chan struct{})
	go func() { _, _ = s.Poll(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Poll did not stop after cancellation")
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("cancellation was reported as a failure: %s", logs)
	}

	// Nothing may be lost: the next poll lists in full and handles every thread.
	f.hook = nil
	s.lookupPace = 0
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if last := f.requests[len(f.requests)-1]; strings.HasPrefix(last, "GET /notifications ") {
		t.Fatalf("poll after cancellation did no work: %v", f.requests)
	}
	if count := strings.Count(logs.String(), "team-only review request"); count != 6 {
		t.Fatalf("got %d matches after resuming, want 6: %s", count, logs)
	}
}

func TestPollFallbackTokenAndConditional(t *testing.T) {
	f := newTestServer(t)
	s, logs := f.newSieve(t, "read")
	s.cfg.OwnerTokens = nil
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 6 || f.requests[5] != "GET /notifications "+f.modified || f.requests[3] != "GET /notifications/threads/1 " || f.requests[4] != "PATCH /notifications/threads/1 " {
		t.Fatalf("unexpected requests: %v", f.requests)
	}
	for _, token := range f.tokens {
		if !strings.Contains(token, "main-token") {
			t.Fatalf("unexpected token: %v", f.tokens)
		}
	}
	if strings.Count(logs.String(), "team-only review request") != 1 {
		t.Fatalf("unexpected logs: %s", logs)
	}
}

func TestPollListFailureAndIntervals(t *testing.T) {
	f := newTestServer(t)
	f.failures["/notifications"] = []int{500}
	s, _ := f.newSieve(t, "log")
	if wait, err := s.Poll(context.Background()); err == nil || wait != time.Minute {
		t.Fatalf("failed list: wait=%v err=%v", wait, err)
	}
	if wait, err := s.Poll(context.Background()); err != nil || wait != 90*time.Second {
		t.Fatalf("server interval: wait=%v err=%v", wait, err)
	}
	f.interval = "10"
	if wait, err := s.Poll(context.Background()); err != nil || wait != time.Minute {
		t.Fatalf("configured interval: wait=%v err=%v", wait, err)
	}
}

func TestForeignOriginIsNeverContacted(t *testing.T) {
	requested := make(chan struct{}, 1)
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requested <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer foreign.Close()
	f := newTestServer(t)
	f.threads[0].Subject.URL = foreign.URL + "/repos/acme/repo/pulls/1"
	s, _ := f.newSieve(t, "read")
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requested:
		t.Fatal("sent a request to an untrusted host")
	default:
	}
	if len(f.marked) != 0 {
		t.Fatal("marked a foreign thread")
	}
}

func TestNewActivityFailureDoesNotReuseOldCache(t *testing.T) {
	f := newTestServer(t)
	s, logs := f.newSieve(t, "read")
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.modified = "another version"
	f.threads[0].UpdatedAt = "2026-01-01T00:01:00Z"
	f.failures["/repos/acme/repo/pulls/1"] = []int{500}
	for i := 0; i < 2; i++ {
		if _, err := s.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.marked) != 2 {
		t.Fatalf("new activity not retried: %v %s", f.requests, logs)
	}
}

func TestChangedThreadIsNotMarked(t *testing.T) {
	tests := []struct {
		name   string
		change func(*github.Thread)
	}{
		{"id", func(th *github.Thread) { th.ID = "2" }},
		{"activity", func(th *github.Thread) { th.UpdatedAt = "2026-01-01T00:02:00Z" }},
		{"reason", func(th *github.Thread) { th.Reason = "mention" }},
		{"subject type", func(th *github.Thread) { th.Subject.Type = "Issue" }},
		{"subject url", func(th *github.Thread) { th.Subject.URL += "0" }},
		{"repository", func(th *github.Thread) { th.Repository.FullName = "another/repo" }},
		{"owner", func(th *github.Thread) { th.Repository.Owner.Login = "another" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestServer(t)
			current := f.threads[0]
			tt.change(&current)
			f.current = &current
			s, logs := f.newSieve(t, "read")
			if _, err := s.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.marked) != 0 || !strings.Contains(logs.String(), "could not evaluate notification") {
				t.Fatalf("changed thread marked: %v %s", f.marked, logs)
			}
			f.current = nil
			if _, err := s.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.marked) != 1 {
				t.Fatalf("unchanged thread was not retried: %v %s", f.requests, logs)
			}
		})
	}
}

func TestThreadCheckFailureRetries(t *testing.T) {
	f := newTestServer(t)
	f.failures["/notifications/threads/1"] = []int{500}
	s, logs := f.newSieve(t, "read")
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.marked) != 0 {
		t.Fatal("marked after failed thread check")
	}
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.marked) != 1 || !strings.Contains(logs.String(), "could not evaluate notification") {
		t.Fatalf("failed thread check was not retried: %v %s", f.requests, logs)
	}
}

func TestNewActivityBetweenListAndMarkIsReevaluated(t *testing.T) {
	f := newTestServer(t)
	current := f.threads[0]
	current.UpdatedAt = "2026-01-01T00:02:00Z"
	f.current = &current
	s, _ := f.newSieve(t, "read")
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.marked) != 0 {
		t.Fatal("marked stale notification")
	}
	f.threads[0] = current
	if _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.marked) != 1 {
		t.Fatalf("new activity was not evaluated: %v", f.requests)
	}
	var lists []string
	for _, request := range f.requests {
		if strings.HasPrefix(request, "GET /notifications ") {
			lists = append(lists, request)
		}
	}
	if len(lists) != 2 || lists[1] != "GET /notifications " {
		t.Fatalf("second listing was conditional: %v", lists)
	}
}

func TestRunRetriesListFailureAndCancels(t *testing.T) {
	f := newTestServer(t)
	f.interval = "0"
	f.failures["/notifications"] = []int{500}
	s, logs := f.newSieve(t, "log")
	s.cfg.PollInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.requests)
		f.mu.Unlock()
		if n >= 5 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
	if !strings.Contains(logs.String(), "team-only review request") || !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("run did not recover: %s", logs)
	}
}

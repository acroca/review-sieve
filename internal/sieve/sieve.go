// Package sieve identifies team-only review requests and processes an inbox.
package sieve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acroca/review-sieve/internal/config"
	"github.com/acroca/review-sieve/internal/github"
)

// Candidate reports whether a notification can be a team review request.
func Candidate(thread github.Thread) bool {
	return thread.Reason == "review_requested" && thread.Subject.Type == "PullRequest" && thread.Subject.URL != ""
}

// complete reports whether the data needed to judge involvement is present.
// The author of a pull request or review may have no login: GitHub returns a
// null user for deleted accounts, and a deleted account cannot be login.
func complete(pr github.PullRequest, reviews []github.Review, login string) bool {
	if login == "" || pr.Number <= 0 || strings.TrimSpace(pr.Title) == "" ||
		pr.Assignees == nil || pr.RequestedReviewers == nil || reviews == nil {
		return false
	}
	for _, users := range [][]github.User{pr.Assignees, pr.RequestedReviewers} {
		for _, user := range users {
			if user.Login == "" {
				return false
			}
		}
	}
	return true
}

// TeamOnly reports whether a complete pull request and its reviews show no
// direct involvement by login. Missing information is never considered safe.
func TeamOnly(pr github.PullRequest, reviews []github.Review, login string) bool {
	if !complete(pr, reviews, login) || strings.EqualFold(pr.User.Login, login) {
		return false
	}
	for _, users := range [][]github.User{pr.Assignees, pr.RequestedReviewers} {
		for _, user := range users {
			if strings.EqualFold(user.Login, login) {
				return false
			}
		}
	}
	for _, review := range reviews {
		if strings.EqualFold(review.User.Login, login) {
			return false
		}
	}
	return true
}

// GitHub asks clients to avoid bursts: its documented secondary rate limits
// allow 900 points a minute (a read costs 1 point, a write 5), and its best
// practices ask for at least a second between write requests. The values
// below keep a large backlog inside those limits while still looking up
// several pull requests at once.
const (
	// lookupWorkers bounds how many pull request lookups run at the same time.
	lookupWorkers = 4
	// defaultLookupPace is the minimum gap between starting two lookups. A
	// lookup is normally two requests, so this is about 480 reads a minute.
	defaultLookupPace = 250 * time.Millisecond
	// defaultMarkPace is the minimum gap between two marks.
	defaultMarkPace = time.Second
)

// Sieve holds the in-memory state for a user's notification inbox.
type Sieve struct {
	client     *github.Client
	cfg        config.Config
	login      string
	logger     *slog.Logger
	lookupPace time.Duration
	markPace   time.Duration
	modified   string
	evaluated  map[string]string
	warned     map[string]bool
}

// New constructs a polling sieve.
func New(client *github.Client, cfg config.Config, login string, logger *slog.Logger) *Sieve {
	if logger == nil {
		logger = slog.Default()
	}
	return &Sieve{
		client: client, cfg: cfg, login: login, logger: logger,
		lookupPace: defaultLookupPace, markPace: defaultMarkPace,
		evaluated: make(map[string]string), warned: make(map[string]bool),
	}
}

// redact removes configured token values from data supplied by the API or errors.
func (s *Sieve) redact(value string) string {
	tokens := []string{s.cfg.Token}
	for _, token := range s.cfg.OwnerTokens {
		tokens = append(tokens, token)
	}
	// Removing a value can join text into another token, so continue until no
	// configured value occurs. Removing rather than inserting a marker avoids
	// accidentally including a token in the marker itself.
	for {
		previous := value
		for _, token := range tokens {
			if token != "" {
				value = strings.ReplaceAll(value, token, "")
			}
		}
		if value == previous {
			return value
		}
	}
}

// threadNumber verifies that a notification's subject belongs to its repository.
func threadNumber(thread github.Thread) (int, error) {
	repo := strings.Split(thread.Repository.FullName, "/")
	if len(repo) != 2 || repo[0] == "" || repo[1] == "" ||
		thread.Repository.Owner.Login == "" || !strings.EqualFold(repo[0], thread.Repository.Owner.Login) {
		return 0, errors.New("missing or inconsistent repository")
	}
	u, err := url.Parse(thread.Subject.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return 0, errors.New("invalid pull request URL")
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "repos" ||
		!strings.EqualFold(parts[2], repo[0]) || !strings.EqualFold(parts[3], repo[1]) || parts[4] != "pulls" {
		return 0, errors.New("pull request URL does not match repository")
	}
	number, err := strconv.Atoi(parts[5])
	if err != nil || number <= 0 {
		return 0, errors.New("invalid pull request number")
	}
	return number, nil
}

func (s *Sieve) warnDenied(thread github.Thread, variable string, configured bool) {
	key := strings.ToLower(thread.Repository.FullName)
	if s.warned[key] {
		return
	}
	s.warned[key] = true
	var message string
	if configured {
		message = fmt.Sprintf("%s cannot read %s; check the token's repository selection and its \"Pull requests: read\" permission", variable, thread.Repository.FullName)
	} else {
		message = fmt.Sprintf("cannot read %s with GITHUB_TOKEN; set %s to a token with read access to pull requests in %s", thread.Repository.FullName, variable, thread.Repository.Owner.Login)
	}
	s.logger.Warn(s.redact(message))
}

// result is what a lookup learned about one candidate thread.
type result struct {
	thread     github.Thread
	variable   string // owner token variable name, for permission warnings
	configured bool   // whether that variable is set
	denied     bool   // the lookup was refused for permission reasons
	teamOnly   bool
	pr         github.PullRequest
	err        error
}

// lookup fetches what is needed to classify a thread. It runs on a worker
// goroutine, so it must only read the Sieve: state changes, marks and logging
// happen in apply.
func (s *Sieve) lookup(ctx context.Context, thread github.Thread) result {
	r := result{thread: thread}
	number, err := threadNumber(thread)
	if err != nil {
		r.err = err
		return r
	}
	var token string
	token, r.variable, r.configured = s.cfg.TokenForOwner(thread.Repository.Owner.Login)
	pr, err := s.client.PullRequest(ctx, thread.Subject.URL, token)
	if err != nil {
		r.denied, r.err = github.IsAccessDenied(err), err
		return r
	}
	if pr.Number != number || !complete(pr, []github.Review{}, s.login) {
		r.err = errors.New("incomplete or inconsistent pull request data")
		return r
	}
	r.pr = pr
	if !TeamOnly(pr, []github.Review{}, s.login) {
		// Already involved as author, assignee or requested reviewer; the
		// reviews cannot change that, so they are not fetched.
		return r
	}
	reviews, err := s.client.Reviews(ctx, thread.Subject.URL, token)
	if err != nil {
		r.denied, r.err = github.IsAccessDenied(err), err
		return r
	}
	r.teamOnly = TeamOnly(pr, reviews, s.login)
	return r
}

// dismiss acts on a team-only thread according to the mode and returns the
// action taken. Marks are spaced at least markPace apart.
func (s *Sieve) dismiss(ctx context.Context, thread github.Thread, lastMark *time.Time) (string, error) {
	switch s.cfg.Mode {
	case "log":
		return "logged", nil
	case "read", "done":
	default:
		return "", errors.New("invalid mode")
	}
	if !lastMark.IsZero() {
		if err := sleep(ctx, s.markPace-time.Since(*lastMark)); err != nil {
			return "", err
		}
	}
	current, err := s.client.GetThread(ctx, thread.ID)
	if err != nil {
		return "", err
	}
	if current.ID != thread.ID || current.UpdatedAt != thread.UpdatedAt ||
		current.Reason != thread.Reason ||
		current.Subject.Type != thread.Subject.Type || current.Subject.URL != thread.Subject.URL ||
		current.Repository.FullName != thread.Repository.FullName ||
		current.Repository.Owner.Login != thread.Repository.Owner.Login {
		return "", errors.New("notification changed since listing")
	}
	*lastMark = time.Now()
	if err := s.client.Mark(ctx, thread.ID, s.cfg.Mode); err != nil {
		return "", err
	}
	return s.cfg.Mode, nil
}

// apply records the outcome of a lookup. It runs on Poll's goroutine, one
// result at a time, and is the only place that changes state, marks or logs.
func (s *Sieve) apply(ctx context.Context, r result, lastMark *time.Time) {
	thread := r.thread
	switch {
	case r.denied:
		s.warnDenied(thread, r.variable, r.configured)
	case r.err != nil:
		s.fail(ctx, thread, r.err)
		return
	case r.teamOnly:
		action, err := s.dismiss(ctx, thread, lastMark)
		if err != nil {
			s.fail(ctx, thread, err)
			return
		}
		var loggedNumber any = r.pr.Number
		numberText := strconv.Itoa(r.pr.Number)
		if redacted := s.redact(numberText); redacted != numberText {
			loggedNumber = redacted
		}
		s.logger.Info(s.redact("team-only review request"), "repo", s.redact(thread.Repository.FullName), "pr", loggedNumber, "title", s.redact(r.pr.Title), "action", s.redact(action))
	}
	s.evaluated[thread.ID] = thread.UpdatedAt
}

// fail leaves a thread unevaluated and forces the next poll to list in full,
// so the thread is retried even if nothing else changes in the inbox.
func (s *Sieve) fail(ctx context.Context, thread github.Thread, err error) {
	s.modified = ""
	if ctx.Err() != nil {
		return // shutting down, not a failure worth reporting
	}
	s.logger.Error(s.redact("could not evaluate notification"), "repo", s.redact(thread.Repository.FullName), "error", s.redact(err.Error()))
}

// process looks up the pending threads on a few workers and applies each
// result as it arrives.
func (s *Sieve) process(ctx context.Context, pending []github.Thread) {
	if len(pending) == 0 {
		return
	}
	jobs := make(chan github.Thread)
	results := make(chan result, len(pending))
	var workers sync.WaitGroup
	for range min(lookupWorkers, len(pending)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for thread := range jobs {
				results <- s.lookup(ctx, thread)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i, thread := range pending {
			if i > 0 && sleep(ctx, s.lookupPace) != nil {
				return
			}
			select {
			case jobs <- thread:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()
	var lastMark time.Time
	for r := range results {
		s.apply(ctx, r, &lastMark)
	}
	if ctx.Err() != nil {
		s.modified = "" // some threads may not have been looked up
	}
}

// sleep waits for d, or returns early with the context's error.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Poll processes one inbox listing and returns the recommended delay.
func (s *Sieve) Poll(ctx context.Context) (time.Duration, error) {
	threads, modified, interval, notModified, err := s.client.Notifications(ctx, s.modified)
	wait := max(s.cfg.PollInterval, interval)
	if err != nil {
		s.modified = ""
		return wait, errors.New(s.redact(err.Error()))
	}
	if notModified {
		return wait, nil
	}
	s.modified = modified
	seen := make(map[string]github.Thread, len(threads))
	conflicts := make(map[string]bool)
	var order []string // inbox order, each ID once
	for _, thread := range threads {
		if prev, ok := seen[thread.ID]; ok && !reflect.DeepEqual(prev, thread) {
			conflicts[thread.ID] = true
		} else if !ok {
			seen[thread.ID] = thread
			order = append(order, thread.ID)
		}
	}
	for id := range s.evaluated {
		if _, ok := seen[id]; !ok {
			delete(s.evaluated, id)
		}
	}
	var pending []github.Thread
	for _, id := range order {
		thread := seen[id]
		_, timeErr := time.Parse(time.RFC3339Nano, thread.UpdatedAt)
		if conflicts[id] || !validID(id) || timeErr != nil {
			delete(s.evaluated, id)
			s.fail(ctx, thread, errors.New("missing or conflicting notification metadata"))
			continue
		}
		if s.evaluated[id] == thread.UpdatedAt {
			continue
		}
		delete(s.evaluated, id)
		if !Candidate(thread) {
			s.evaluated[id] = thread.UpdatedAt
			continue
		}
		pending = append(pending, thread)
	}
	s.process(ctx, pending)
	return wait, nil
}

func validID(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Run polls until the context is cancelled, retrying failed listings.
func (s *Sieve) Run(ctx context.Context) {
	for ctx.Err() == nil {
		wait, err := s.Poll(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.logger.Error(s.redact("could not list notifications"), "error", s.redact(err.Error()))
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

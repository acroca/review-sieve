// Package github provides the GitHub REST API operations needed by review-sieve.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Thread struct {
	ID         string     `json:"id"`
	Reason     string     `json:"reason"`
	UpdatedAt  string     `json:"updated_at"`
	Subject    Subject    `json:"subject"`
	Repository Repository `json:"repository"`
}

type Subject struct {
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

type Repository struct {
	FullName string `json:"full_name"`
	Owner    User   `json:"owner"`
}

type User struct {
	Login string `json:"login"`
}

type PullRequest struct {
	Number             int    `json:"number"`
	Title              string `json:"title"`
	User               User   `json:"user"`
	Assignees          []User `json:"assignees"`
	RequestedReviewers []User `json:"requested_reviewers"`
}

type Review struct {
	User User `json:"user"`
}

type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// New creates a client whose requests, including redirects and pagination, are
// restricted to the configured scheme and host.
func New(baseURL string, token string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" ||
		(u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid GitHub API base URL")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/"
	u.RawPath = ""
	c := &Client{base: u, token: token}
	c.http = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !c.allowed(req.URL) || len(via) > 0 && via[0].Method != http.MethodGet {
			return errors.New("GitHub API redirect rejected")
		}
		return nil
	}}
	return c, nil
}

func (c *Client) allowed(u *url.URL) bool {
	return u != nil && u.User == nil && u.Opaque == "" && u.Fragment == "" &&
		strings.EqualFold(u.Scheme, c.base.Scheme) && strings.EqualFold(u.Host, c.base.Host)
}

func (c *Client) endpoint(path string) *url.URL {
	u := *c.base
	u.Path = c.base.Path + path
	u.RawPath = ""
	return &u
}

type apiError struct {
	status       int
	accessDenied bool
}

func (e *apiError) Error() string { return fmt.Sprintf("GitHub API returned HTTP %d", e.status) }

// IsAccessDenied reports authorization or resource-access failures, not rate limits.
func IsAccessDenied(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.accessDenied
}

const maxBody = 16 << 20

func (c *Client) request(ctx context.Context, method string, u *url.URL, token, modified string, body []byte) (*http.Response, []byte, error) {
	if !c.allowed(u) {
		return nil, nil, errors.New("GitHub API URL is outside configured origin")
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, errors.New("invalid GitHub API request")
	}
	if token == "" {
		token = c.token
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "review-sieve")
	if modified != "" {
		req.Header.Set("If-Modified-Since", modified)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		// net/http errors can contain URLs, including query parameters.
		return nil, nil, errors.New("GitHub API request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(data) > maxBody {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, errors.New("failed to read GitHub API response")
	}
	if (resp.StatusCode < 200 || resp.StatusCode >= 300) && resp.StatusCode != http.StatusNotModified {
		_, hasRetryAfter := resp.Header["Retry-After"]
		rateLimited := resp.StatusCode == http.StatusForbidden &&
			(resp.Header.Get("X-RateLimit-Remaining") == "0" || hasRetryAfter)
		denied := resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden && !rateLimited
		return nil, nil, &apiError{status: resp.StatusCode, accessDenied: denied}
	}
	return resp, data, nil
}

func decode(data []byte, target any) error {
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) || json.Unmarshal(data, target) != nil {
		return errors.New("invalid GitHub API response")
	}
	return nil
}

func (c *Client) User(ctx context.Context) (string, error) {
	resp, data, err := c.request(ctx, http.MethodGet, c.endpoint("user"), "", "", nil)
	if err != nil {
		return "", err
	}
	var user User
	if resp.StatusCode != http.StatusOK || decode(data, &user) != nil || user.Login == "" {
		return "", errors.New("invalid GitHub API user response")
	}
	return user.Login, nil
}

// nextLink extracts the next page URL from a GitHub Link header.
func nextLink(header string, current *url.URL) (*url.URL, error) {
	for len(header) > 0 {
		header = strings.TrimLeft(header, " \t,")
		if header == "" {
			break
		}
		if header[0] != '<' {
			return nil, errors.New("invalid GitHub pagination header")
		}
		end := strings.IndexByte(header, '>')
		if end < 0 {
			return nil, errors.New("invalid GitHub pagination header")
		}
		raw := header[1:end]
		header = header[end+1:]
		// Link parameter values can be quoted; do not split on commas in quotes.
		inQuote, escape, cut := false, false, len(header)
		for i, r := range header {
			if escape {
				escape = false
				continue
			}
			if r == '\\' && inQuote {
				escape = true
			} else if r == '"' {
				inQuote = !inQuote
			} else if r == ',' && !inQuote {
				cut = i
				break
			}
		}
		if inQuote {
			return nil, errors.New("invalid GitHub pagination header")
		}
		params := header[:cut]
		header = header[cut:]
		for _, param := range strings.Split(params, ";") {
			key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && strings.EqualFold(key, "rel") {
				for _, rel := range strings.Fields(strings.Trim(value, `"`)) {
					if strings.EqualFold(rel, "next") {
						u, err := url.Parse(raw)
						if err != nil {
							return nil, errors.New("invalid GitHub pagination URL")
						}
						return current.ResolveReference(u), nil
					}
				}
			}
		}
	}
	return nil, nil
}

func pollInterval(header string) time.Duration {
	seconds, err := strconv.ParseInt(strings.TrimSpace(header), 10, 64)
	if err != nil || seconds < 0 || seconds > int64((1<<63-1)/int64(time.Second)) {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func (c *Client) Notifications(ctx context.Context, modified string) (threads []Thread, lastModified string, interval time.Duration, notModified bool, err error) {
	u := c.endpoint("notifications")
	seen := map[string]bool{}
	threads = []Thread{}
	first := true
	for u != nil {
		if seen[u.String()] || len(seen) >= 10000 {
			return nil, "", 0, false, errors.New("invalid GitHub pagination cycle")
		}
		seen[u.String()] = true
		conditional := ""
		if first {
			conditional = modified
		}
		resp, data, e := c.request(ctx, http.MethodGet, u, "", conditional, nil)
		if e != nil {
			return nil, "", 0, false, e
		}
		if first {
			lastModified = resp.Header.Get("Last-Modified")
			interval = pollInterval(resp.Header.Get("X-Poll-Interval"))
			if resp.StatusCode == http.StatusNotModified {
				return nil, lastModified, interval, true, nil
			}
		}
		var page []Thread
		if resp.StatusCode != http.StatusOK || decode(data, &page) != nil || page == nil {
			return nil, "", 0, false, errors.New("invalid GitHub notifications response")
		}
		threads = append(threads, page...)
		u, e = nextLink(strings.Join(resp.Header.Values("Link"), ","), u)
		if e != nil {
			return nil, "", 0, false, e
		}
		first = false
	}
	return threads, lastModified, interval, false, nil
}

func (c *Client) subjectURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || !c.allowed(u) {
		return nil, errors.New("GitHub API URL is outside configured origin")
	}
	return u, nil
}

func (c *Client) PullRequest(ctx context.Context, rawURL, token string) (PullRequest, error) {
	var pr PullRequest
	u, err := c.subjectURL(rawURL)
	if err != nil {
		return pr, err
	}
	resp, data, err := c.request(ctx, http.MethodGet, u, token, "", nil)
	if err != nil {
		return pr, err
	}
	// The author is not checked: GitHub returns a null user for deleted accounts.
	if resp.StatusCode != http.StatusOK || decode(data, &pr) != nil || pr.Number <= 0 ||
		pr.Assignees == nil || pr.RequestedReviewers == nil {
		return PullRequest{}, errors.New("invalid GitHub pull request response")
	}
	for _, user := range append(append([]User{}, pr.Assignees...), pr.RequestedReviewers...) {
		if user.Login == "" {
			return PullRequest{}, errors.New("invalid GitHub pull request response")
		}
	}
	return pr, nil
}

func (c *Client) Reviews(ctx context.Context, rawURL, token string) ([]Review, error) {
	u, err := c.subjectURL(rawURL)
	if err != nil {
		return nil, err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/reviews"
	u.RawPath = ""
	seen := map[string]bool{}
	reviews := []Review{}
	for u != nil {
		if seen[u.String()] || len(seen) >= 10000 {
			return nil, errors.New("invalid GitHub pagination cycle")
		}
		seen[u.String()] = true
		resp, data, err := c.request(ctx, http.MethodGet, u, token, "", nil)
		if err != nil {
			return nil, err
		}
		var page []Review
		if resp.StatusCode != http.StatusOK || decode(data, &page) != nil || page == nil {
			return nil, errors.New("invalid GitHub reviews response")
		}
		// Review authors are not checked: deleted accounts have a null user.
		reviews = append(reviews, page...)
		u, err = nextLink(strings.Join(resp.Header.Values("Link"), ","), u)
		if err != nil {
			return nil, err
		}
	}
	return reviews, nil
}

func validThreadID(id string) bool {
	return id != "" && strings.Trim(id, "0123456789") == ""
}

// GetThread retrieves a notification thread using the main token.
func (c *Client) GetThread(ctx context.Context, id string) (Thread, error) {
	if !validThreadID(id) {
		return Thread{}, errors.New("invalid GitHub notification ID")
	}
	resp, data, err := c.request(ctx, http.MethodGet, c.endpoint("notifications/threads/"+id), "", "", nil)
	if err != nil {
		return Thread{}, err
	}
	var thread Thread
	if resp.StatusCode != http.StatusOK || decode(data, &thread) != nil || thread.ID != id || thread.UpdatedAt == "" {
		return Thread{}, errors.New("invalid GitHub notification thread response")
	}
	return thread, nil
}

func (c *Client) Mark(ctx context.Context, id, mode string) error {
	if !validThreadID(id) {
		return errors.New("invalid GitHub notification ID")
	}
	method := ""
	switch mode {
	case "read":
		method = http.MethodPatch
	case "done":
		method = http.MethodDelete
	default:
		return errors.New("invalid GitHub notification mode")
	}
	resp, _, err := c.request(ctx, method, c.endpoint("notifications/threads/"+id), "", "", nil)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotModified {
		return errors.New("invalid GitHub mark response")
	}
	return nil
}

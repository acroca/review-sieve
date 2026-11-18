# Agent guide

`review-sieve` is a background service that polls one user's GitHub
notification inbox and logs, marks read, or marks done the review requests
that were addressed only to a team. See `README.md` for the user-facing
behaviour.

## The rule that outranks everything

Never dismiss a notification the user needed. When anything is uncertain — a
lookup failed, data is missing, a response looks odd — leave the notification
alone. A missed dismissal is a minor annoyance; a wrong one is a silent loss.

## Commands

- `mise install` — installs the pinned Go toolchain from `mise.toml`
- `make test` — `go vet` plus the race-enabled test suite
- `make fmt-check` — fails if any file needs `gofmt`
- `make image` — builds `review-sieve:dev`

Run `make test` and `make fmt-check` before finishing any change.

## Layout

- `main.go` — wiring, signal handling, exit codes
- `internal/config` — environment parsing and the token-per-owner lookup
- `internal/github` — REST client; the only code that talks HTTP
- `internal/sieve` — the classification rule (`Candidate`, `TeamOnly`) and the
  poll cycle (`Poll`, `Run`)

Keep the classification rule a pure function, separate from HTTP and the loop.

## Constraints

- Standard library only. `go.mod` must not gain a `require` block.
- Configuration is environment variables only: `GITHUB_TOKEN`,
  `GITHUB_TOKEN_<OWNER>`, `MODE`, `POLL_INTERVAL`. No config files, no flags.
- State is in memory only.
- Token values must never appear in logs or error messages. Variable names may.
- The only write calls are `PATCH` and `DELETE` on
  `/notifications/threads/{id}`, always with the main token. Never add a call
  that needs repository write access.
- A token is only ever sent to the configured API origin. Keep the
  scheme-and-host check in the client's request path.
- Pull request and review lookups use the owner's token when one is
  configured, and fall back to the main token.

## Behaviour that is easy to break

- **Retry after a transient failure.** If a thread could not be evaluated, the
  stored `Last-Modified` value is cleared so the next poll lists in full.
  Otherwise GitHub answers 304 and the thread is never retried.
- **Rate limits are not permission errors.** A 403 with
  `X-RateLimit-Remaining: 0` or `Retry-After`, and any 429, must be retried and
  must not produce a "set a token" warning.
- **Permission warnings are logged once per repository** per process run.
- **Only lookups run in parallel.** `lookup` runs on worker goroutines and must
  only read the `Sieve`. Everything that changes state, marks a thread or logs
  happens in `apply`, on the goroutine that called `Poll`. Marks are made one
  at a time.
- **Requests are paced on purpose.** `lookupWorkers`, `defaultLookupPace` and
  `defaultMarkPace` keep a large backlog inside GitHub's secondary rate limits
  (900 points a minute; at least a second between writes). Do not raise them
  without checking GitHub's current limits.
- **Reviews are fetched only when needed.** If the pull request already shows
  the user as author, assignee or requested reviewer, the reviews request is
  skipped.
- **Logins are case-insensitive** — compare with `strings.EqualFold`.
- **Accept what GitHub actually returns.** Logins can contain underscores
  (managed enterprise accounts) and a `[bot]` suffix, and a `user` field can be
  null for deleted accounts. Do not add validation that turns valid data into
  a permanent error.

## User-facing strings

The environment variable names, the startup error messages, and the log
messages (`team-only review request`, the two token warnings,
`could not evaluate notification`) are documented in `README.md` and asserted
in tests. If you change one, change all three places together.

## Tests

- Write the test first.
- Never call the real GitHub API. Tests run the real client against
  `net/http/httptest` fakes defined in the test files (`testClient` in
  `internal/github/client_test.go`, `newTestServer` in
  `internal/sieve/sieve_test.go`).
- Any new failure path needs a test showing the notification is left alone.

## Versions

The Go version is pinned in four places that must stay in step: `go.mod`,
`mise.toml`, `Dockerfile` and `.github/workflows/ci.yml`.

## Out of scope

Do not add, unless asked: desktop or other notification delivery,
per-repository allow/deny lists, email handling, thread muting, GitHub
Enterprise Server support, persistent state, or reading tokens from files.

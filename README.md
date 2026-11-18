# review-sieve

## What it does

GitHub sends review-request notifications to every member of a CODEOWNERS
team. review-sieve polls one user's unread notification inbox and clears
requests addressed only to a team, so notification clients see a quieter
inbox.

## What gets dismissed

A notification is eligible only when its reason is `review_requested`, its
subject is a pull request with a URL, and the user is not a requested reviewer,
the author, an assignee, or someone who has submitted a review. Login
comparisons are case-insensitive. Everything else is left alone, including
notifications whose pull request or reviews could not be checked.

Follow-up activity makes a thread unread again and it is evaluated again.
review-sieve does not mute threads or change subscriptions. A team-only request
can still be relevant to you: review the logged matches before enabling a mode
that changes your inbox.

## Modes

Start with `log` and inspect the output before switching to a mode that changes
notifications.

| `MODE` | Effect on an eligible thread |
|---|---|
| `log` (default) | Log what would be dismissed; change nothing |
| `read` | Mark the thread as read |
| `done` | Mark the thread as done |

## Tokens

Create a **classic personal access token** with only the `notifications`
scope for `GITHUB_TOKEN`. Fine-grained tokens do not work with the
notifications API. If an organization enforces SSO, authorize the classic
token for that organization. The main token is also used to look up pull
requests when no owner token is configured, which is sufficient for public
repositories.

For private repositories, configure a fine-grained token for each owner with
access to its relevant repositories and **Pull requests: Read-only** permission.
Some organizations require approval for fine-grained tokens. Name each variable
`GITHUB_TOKEN_<OWNER>`, uppercasing the owner login and replacing hyphens with
underscores; for example, `acme-corp` uses `GITHUB_TOKEN_ACME_CORP`. Empty
owner-token values are ignored.

If a lookup is denied, review-sieve leaves the notification untouched and
warns once per repository. A warning to set `GITHUB_TOKEN_<OWNER>` means the
main token could not read that repository. A warning that the owner token
cannot read it means its repository selection or Pull requests read permission
needs checking. Rate limits are retried instead of producing token warnings.
Keep tokens private; do not commit `.env` or put tokens in issues.

## Running

Copy `.env.example` to `.env`, replace the placeholders, and run:

```sh
docker run --env-file .env ghcr.io/acroca/review-sieve
```

Alternatively, after creating `.env`, start the included Compose service:

```sh
docker compose up -d
docker compose logs -f
```

## Configuration

Configuration is through environment variables only.

| Variable | Default | Meaning |
|---|---|---|
| `GITHUB_TOKEN` | Required | Classic token for authentication, notifications, and fallback pull request lookups |
| `GITHUB_TOKEN_<OWNER>` | None | Optional per-owner token for pull request and review lookups |
| `MODE` | `log` | `log`, `read`, or `done` (case-insensitive) |
| `POLL_INTERVAL` | `60s` | Positive Go duration between polls |

The actual wait is the larger of `POLL_INTERVAL` and GitHub's
`X-Poll-Interval`. Invalid configuration fails at startup.

## Limitations

Noisy notifications can be visible until the next poll, up to one poll
interval under normal operation. Email and mobile push notifications are
unaffected. Only github.com is supported. State is held in memory and is lost
on restart.

To stay within GitHub's rate limits, pull request lookups run at most four at
a time and start at most four per second, and threads are marked one at a
time, at most one per second. A large backlog on the first run is therefore
worked through over a few minutes rather than instantly.

Before marking a thread, review-sieve checks that it has not changed since the
inbox listing. GitHub does not provide an atomic conditional mark for an
individual thread, so activity arriving between that check and the mark can
still be cleared.

## Development

Install [mise](https://mise.jdx.dev) and Docker, then run `mise install` to get
the pinned Go toolchain. Without mise, install the Go version named in
`go.mod`.

- `make test`: run `go vet` and the race-enabled tests.
- `make fmt-check`: fail if Go files need formatting.
- `make image`: build `review-sieve:dev` locally.

## License

[MIT](LICENSE).
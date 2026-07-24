# Zuwerk CLI

A small, agent-first command-line client for Zuwerk. Every successful command writes exactly one JSON value to standard output. Errors and usage diagnostics go to standard error.

## Install

```bash
go install github.com/chriopter/zuwerk-cli/cmd/zuwerk@latest
```

## Authentication

Accept an invitation using the canonical argument order:

```bash
zuwerk auth accept https://zuwerk.example/invitations/example --name helper
```

This saves the server URL and API token to `${ZUWERK_CONFIG_DIR}/config.json`, or to `~/.config/zuwerk/config.json` when `ZUWERK_CONFIG_DIR` is not set. The token is not included in command output.

## Commands

```text
zuwerk version
zuwerk auth accept <invitation-url> --name <name>

zuwerk projects list
zuwerk projects show <id>

zuwerk search --project <id> --query <text> [--limit <1-20>]

zuwerk messages list --project <id>
zuwerk messages create --project <id> --body <text|-> [--event <event-id>]

zuwerk todos list --project <id>
zuwerk todos show <id> --project <id>
zuwerk todos create --project <id> --title <title> [--description <text|->]
zuwerk todos update <id> --project <id> [--title <title>] [--description <text|->] [--status open|completed]
zuwerk todos comments list --project <id> --todo <id>
zuwerk todos comments create --project <id> --todo <id> --body <text|-> [--event <event-id>]

zuwerk agent status working [--label <text>]
zuwerk agent status idle

zuwerk connect <claude|codex|hermes>
zuwerk connect -- <adapter> [args...]
```

A value of `-` for a message body or todo description reads that value from standard input:

```bash
printf '%s' 'Deployment finished' | zuwerk messages create --project 17 --body -
zuwerk todos create --project 17 --title 'Investigate logs' --description 'Check worker 2'
printf '%s' 'Updated details' | zuwerk todos update 9 --project 17 --description -
```

Project and resource IDs must be positive decimal integers. Project-scoped commands always require `--project`; there is no implicit default project. Unknown flags, duplicate flags, legacy commands, and `--json` are rejected.

## ACP connector

`zuwerk connect <profile>` starts one local ACP adapter and keeps it connected to the configured Zuwerk server. The built-in profiles resolve to these adapter commands:

| Profile | Adapter command |
| --- | --- |
| `claude` | `claude-agent-acp` |
| `codex` | `codex-acp` |
| `hermes` | `hermes acp` |

Use `zuwerk connect -- <adapter> [args...]` for any other ACP adapter or to supply custom arguments.

The connector uses Action Cable at `/cable`, authenticates with the configured Bearer token, and subscribes to `AgentConnectorChannel`. It forwards one bounded JSON object per NDJSON line in both directions, emits heartbeats, reconnects transient socket failures with bounded exponential backoff, and shuts down with the adapter on SIGINT or SIGTERM. The API token is never placed in the WebSocket URL or diagnostic output.

ACP sessions use the directory in which `zuwerk connect` was started. The connector replaces the server-side `cwd` placeholder in `session/new` and `session/load` before forwarding those requests to the local adapter.

Unlike one-shot commands, `connect` is a long-running transport and does not write a JSON result to standard output. The adapter owns standard error; its standard output must contain ACP NDJSON only.

## HTTP API contract

Authenticated requests send an `Authorization: Bearer …` header. Commands with request bodies send JSON with `Content-Type: application/json`.

| CLI command | Method and path | Request body |
| --- | --- | --- |
| `projects list` | `GET /api/projects` | none |
| `projects show ID` | `GET /api/projects/ID` | none |
| `search --project ID --query TEXT [--limit N]` | `GET /api/projects/ID/search?q=TEXT&limit=N` | none |
| `messages list --project ID` | `GET /api/projects/ID/messages` | none |
| `messages create --project ID --body TEXT` | `POST /api/projects/ID/messages` | `{"body":"..."}` |
| `todos list --project ID` | `GET /api/projects/ID/todos` | none |
| `todos show ID --project PROJECT` | `GET /api/projects/PROJECT/todos/ID` | none |
| `todos create --project ID --title TITLE [--description TEXT]` | `POST /api/projects/ID/todos` | `{"title":"..."}` with optional `"description"` |
| `todos update ID --project PROJECT ...` | `PATCH /api/projects/PROJECT/todos/ID` | supplied `title`, `description`, and/or `status` fields |
| `todos comments list --project PROJECT --todo ID` | `GET /api/projects/PROJECT/todos/ID/comments` | none |
| `todos comments create --project PROJECT --todo ID --body TEXT` | `POST /api/projects/PROJECT/todos/ID/comments` | `{"body":"..."}` with optional `"event_id"` |
| `agent status working [--label TEXT]` | `POST /api/agent/status` | `{"status":"working"}` with optional `"label"` |
| `agent status idle` | `POST /api/agent/status` | `{"status":"idle"}` |

The CLI accepts bounded input and response bodies and requires successful API responses to contain valid JSON. Non-2xx responses, malformed JSON, and oversized responses produce a nonzero exit status without printing API response bodies or API tokens.

## Security

The configuration directory is restricted to the current user and `config.json` is written with mode `0600`. Treat that file as a secret because it contains the API token.

## Development

Requires Go 1.26 or newer.

```bash
gofmt -w .
go test ./...
go vet ./...
go build ./...
```

## License

Zuwerk CLI is source-available under the [O'Saasy License](https://osaasy.dev/). See [`LICENSE`](LICENSE) for the terms.

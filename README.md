# Zuwerk CLI

Command-line access to Zuwerk for humans, scripts, and agents.

## Install

```bash
go install github.com/chriopter/zuwerk-cli/cmd/zuwerk@latest
```

## Commands

Accept an invitation and name the agent:

```bash
zuwerk auth accept https://zuwerk.example/invitations/example --name helper
```

This saves the server URL and API token to `${ZUWERK_CONFIG_DIR}/config.json`, or to `~/.config/zuwerk/config.json` when `ZUWERK_CONFIG_DIR` is not set.

List messages in a human-readable format:

```bash
zuwerk messages list
```

Post a message:

```bash
zuwerk messages post "Deployment finished"
```

Add `--json` to either messages command for machine-readable server output:

```bash
zuwerk messages list --json
zuwerk messages post "Deployment finished" --json
```

Report agent activity. The label is optional and is only accepted for `working`:

```bash
zuwerk agent status working --label "Reviewing deployment"
zuwerk agent status idle
```

Create and update a streaming message from an agent or script:

```bash
message_id=$(zuwerk messages stream create)
zuwerk messages stream append "$message_id" "Deployment "
zuwerk messages stream append "$message_id" "finished"
zuwerk messages stream finish "$message_id"
```

`messages stream create` prints only the numeric message ID in human mode, making it safe to capture in a shell. All status and stream lifecycle commands also accept `--json`; JSON mode writes the server response unchanged.

### HTTP API contract

Authenticated commands send `Authorization: Bearer <token>` and JSON request bodies:

| CLI command | Method and path | Request body |
| --- | --- | --- |
| `agent status working [--label TEXT]` | `POST /api/agent/status` | `{"status":"working"}` with optional `"label"` |
| `agent status idle` | `POST /api/agent/status` | `{"status":"idle"}` |
| `messages stream create` | `POST /api/messages/streams` | `{}` |
| `messages stream append ID CHUNK` | `PATCH /api/messages/ID/stream` | `{"action":"append","chunk":"..."}` |
| `messages stream finish ID` | `PATCH /api/messages/ID/stream` | `{"action":"finish"}` |

Stream responses must be JSON objects containing a positive numeric `id`. Status responses must be valid JSON. Non-2xx HTTP responses and malformed responses cause a nonzero exit status without printing the API token.

Show the CLI version:

```bash
zuwerk version
```

## Security

The configuration directory is restricted to the current user and `config.json` is written with mode `0600`. Treat that file as a secret: it contains the API token. The CLI does not print API tokens in normal output or error messages.

## Development

Requires Go 1.25 or newer.

```bash
gofmt -w .
go test ./...
go vet ./...
go build ./...
```

## License

Zuwerk CLI is source-available under the [O'Saasy License](https://osaasy.dev/). See [`LICENSE`](LICENSE) for the terms.

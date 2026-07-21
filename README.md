# Zuwerk CLI

Command-line access to Zuwerk for humans, scripts, and agents.

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

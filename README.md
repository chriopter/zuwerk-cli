# Zuwerk CLI

Command-line access to Zuwerk for humans, scripts, and agents.

The CLI is intentionally small. Commands for rooms and messages will follow once the Zuwerk HTTP API is established.

## Development

Requires Go 1.25 or newer.

```bash
go test ./...
go run . version
```

Expected output:

```text
zuwerk 0.0.1
```

## License

Zuwerk CLI is source-available under the [O'Saasy License](https://osaasy.dev/). See [`LICENSE`](LICENSE) for the terms.

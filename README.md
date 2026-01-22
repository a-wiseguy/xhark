# xhark

A tiny OpenAPI client for your terminal: browse endpoints, build requests, run them, and inspect responses in a fast TUI.

- OpenAPI-driven endpoint browser with fuzzy filter
- Request builder (path + query params)
- JSON body editing via your `$XHARK_EDITOR` / `$EDITOR`
- Built-in auth helper: paste Bearer token, or fetch via OAuth2 password flow when declared in the spec

## Quickstart

Run against a local spec:

```bash
go run ./cmd/xhark --spec-file ./openapi.json
```

Run against a URL (base URL inferred from the spec URL):

```bash
go run ./cmd/xhark --spec-url http://localhost:8000/openapi.json
```

If your spec doesn't provide `servers`, pass a base URL:

```bash
go run ./cmd/xhark --spec-file ./openapi.json --base-url http://localhost:8000
```

## Install

```bash
go build -o xhark ./cmd/xhark
./xhark --spec-url http://localhost:8000/openapi.json
```

## Controls

- `type`: filter endpoints
- `Enter`: select / confirm (context dependent)
- `Tab`: switch pane / next field
- `Esc`: back / close modal
- `Ctrl+R`: run request
- `A`: auth modal
- `Ctrl+D`: clear auth for selected scheme (inside auth modal)
- `q`: quit

## Configuration

CLI flags override environment variables.

- `XHARK_SPEC_URL`
- `XHARK_SPEC_FILE`
- `XHARK_BASE_URL`
- `XHARK_DEBUG=1` (writes to `/tmp/xhark.log`)
- `XHARK_EDITOR` (falls back to `EDITOR`, then `vi`)

## Auth Notes

- OAuth2 token fetching works for password flow specs (`oauth2` + `flows.password.tokenUrl`, e.g. FastAPI `OAuth2PasswordBearer`).
- If your spec only declares bearer auth, paste a token in the auth modal and xhark will inject it as `Authorization: Bearer <token>` for secured operations.

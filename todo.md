# TODO

## Critical

- [ ] **Fix update atomicity** — replace delete-then-create with Cloudflare's PUT endpoint to avoid record loss on failed creation
- [ ] **Propagate errors to external-dns** — return non-2xx on `POST /records` when any change fails; currently always returns 204
- [ ] **Fix SRV parsing error propagation** — `parseSRVTarget()` logs and drops errors silently; return the error so the caller can skip or reject the record

## Bugs

- [ ] **Zone filter logic** (`main.go:168`) — third condition `strings.HasSuffix(f, "."+zone.Name)` inverts longest-suffix semantics; audit and fix
- [ ] **No startup validation** — if `CF_DOMAIN_FILTER` is misconfigured, app starts with zero zones and silently fails all operations; log a warning or exit

## Reliability

- [ ] **Add HTTP client timeout** — `http.DefaultClient` has no timeout; set one on the Cloudflare API client to avoid indefinite hangs
- [ ] **Pagination** — only fetches first 100 zones and 100 records per zone; implement cursor-based pagination or at least log a warning when the limit is reached
- [ ] **Graceful shutdown** — handle `SIGTERM` to drain in-flight requests before exiting

## Testing

- [ ] **Unit tests for `parseSRVTarget()`** — cover valid input, missing fields, non-numeric fields, trailing dot handling
- [ ] **Unit tests for zone matching** — cover longest-suffix selection, wildcard (no filter), misconfigured filter
- [ ] **Integration test with mock Cloudflare API** — cover create, update, delete, and partial-failure paths

## Observability

- [ ] **Structured logging** — replace `log.Printf` with `log/slog` (stdlib since Go 1.21); add log levels
- [ ] **Prometheus metrics** — expose request counts, error counts, Cloudflare API latency on `/metrics`
- [ ] **Startup summary log** — log loaded zone count and filter at startup so misconfiguration is immediately visible

## Minor / Code Quality

- [ ] **Configurable port** — hardcoded `8888`; read from env var `WEBHOOK_PORT`
- [ ] **Configurable pagination limit** — read from env var or increase default and add loop
- [ ] **TTL constant** — replace magic value `1` (auto-TTL) with a named constant
- [ ] **Split into packages** — `main.go` is 489 lines; consider `cloudflare/`, `webhook/`, `translation/` sub-packages
- [ ] **CI/CD pipeline** — add GitHub Actions workflow: `go build`, `go test`, `go vet`, `golangci-lint`, Docker build

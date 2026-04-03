# cloudflare-webhook

A minimal external-dns webhook provider that fixes SRV record support for Cloudflare.

## Why this exists

external-dns has a built-in Cloudflare provider, but it does not correctly handle SRV records. When creating an SRV record, the Cloudflare API requires a structured `data` object:

```json
{
  "type": "SRV",
  "name": "_minecraft._tcp.play.example.com",
  "data": {
    "priority": 0,
    "weight": 5,
    "port": 25565,
    "target": "play.example.com."
  }
}
```

The built-in provider sends only a flat string target, leaving `weight`, `port`, and `target` empty — Cloudflare rejects the request with HTTP 400.

This webhook replaces the built-in provider. It passes A, AAAA, CNAME, and TXT records through to Cloudflare unchanged, correctly constructs the `data` object for SRV records, and supports enabling Cloudflare proxy mode on A and CNAME records via an annotation.

## How it works

external-dns supports a [webhook provider protocol](https://github.com/kubernetes-sigs/external-dns/blob/master/docs/tutorials/webhook-provider.md). Instead of talking to a DNS provider directly, external-dns sends all operations to a local HTTP server. This webhook implements that protocol and translates it into Cloudflare API calls.

```
external-dns → POST /records (webhook API)
                     ↓
              cloudflare-webhook
                     ↓
              Cloudflare REST API
```

The webhook runs as a sidecar container in the same pod as external-dns, listening on `localhost:8888`.

### API endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/` | Returns domain filter (used by external-dns on startup to negotiate scope) |
| `GET` | `/healthz` | Health check |
| `GET` | `/records` | Returns all DNS records from Cloudflare as external-dns endpoints |
| `POST` | `/records` | Applies a changeset (create, update, delete) |
| `POST` | `/adjustendpoints` | Passes endpoints through unchanged |

### SRV record translation

external-dns represents SRV targets as a single string: `"priority weight port target"`.

This webhook parses that string and maps it to the Cloudflare `data` struct:

```
"0 5 25565 play.example.com"
        ↓
{ priority: 0, weight: 5, port: 25565, target: "play.example.com." }
```

On reads (`GET /records`), the reverse happens — the Cloudflare `data` object is serialized back into the `"priority weight port target"` string that external-dns expects.

### Cloudflare proxy mode

By default all records are created with `proxied: false`. There are two ways to enable Cloudflare's proxy (orange cloud):

**Global default** — set `CF_PROXIED=true` to proxy all A, AAAA, and CNAME records unless overridden per record.

**Per-record override** — add the following annotation to your Service or Ingress:

```yaml
external-dns.alpha.kubernetes.io/cloudflare-proxied: "true"   # enable
external-dns.alpha.kubernetes.io/cloudflare-proxied: "false"  # disable (overrides CF_PROXIED)
```

The annotation always takes precedence over `CF_PROXIED`. This has no effect on SRV or TXT records — Cloudflare only supports proxying for A, AAAA, and CNAME.

### Zone discovery

On startup the webhook calls `GET /zones` and filters the results to zones matching `CF_DOMAIN_FILTER`. Zone IDs are cached in memory for the lifetime of the process. When looking up which zone a record belongs to, it uses longest-suffix matching so subzones resolve correctly.

## Deployment

The webhook is deployed as a sidecar via the external-dns Helm chart:

```yaml
provider:
  name: webhook
  webhook:
    image:
      repository: ghcr.io/melvspace/cloudflare-webhook
      tag: latest
      pullPolicy: Always
    env:
      - name: CF_API_TOKEN
        value: <your-token>
      - name: CF_DOMAIN_FILTER
        value: example.com
```

The chart automatically configures external-dns with `--provider=webhook --webhook-provider-url=http://localhost:8888`.

## Configuration

| Env var | Required | Description |
|---------|----------|-------------|
| `CF_API_TOKEN` | Yes | Cloudflare API token with DNS edit permissions |
| `CF_DOMAIN_FILTER` | No | Comma-separated list of zones to manage (e.g. `example.com,other.com`). If empty, all zones in the account are managed. |
| `CF_PROXIED` | No | Set to `true` to enable Cloudflare proxy (orange cloud) for all A, AAAA, and CNAME records by default. Can be overridden per record with the `cloudflare-proxied` annotation. Default: `false`. |

## Differences from the built-in Cloudflare provider

| | Built-in provider | This webhook |
|---|---|---|
| SRV records | Broken — sends empty `weight`/`port`/`target` to Cloudflare API | Correctly maps `"priority weight port target"` string to Cloudflare `data` struct |
| Deployment | Compiled into external-dns binary | Runs as a sidecar, deployed separately |
| Dependencies | Part of external-dns release cycle | Independent, uses official [`cloudflare-go`](https://github.com/cloudflare/cloudflare-go) SDK |
| Cloudflare proxy | Configurable per record | Global default via `CF_PROXIED`; per-record override via `cloudflare-proxied` annotation (A, AAAA, and CNAME only) |
| Zone pagination | Handled | Handled — SDK auto-paginates all zone and record list calls |
| Record pagination | Handled | Handled — SDK auto-paginates all zone and record list calls |

## Building

```bash
docker build -t ghcr.io/melvspace/cloudflare-webhook:latest .
docker push ghcr.io/melvspace/cloudflare-webhook:latest
```

Use `--no-cache` if the base image or a dependency changed:

```bash
docker build --no-cache -t ghcr.io/melvspace/cloudflare-webhook:latest .
```

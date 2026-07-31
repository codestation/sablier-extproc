# sablier-extproc

`sablier-extproc` connects [Envoy's External Processing API][envoy-ext-proc]
to [Sablier][sablier]. It lets an Envoy-based gateway start an on-demand
service through Sablier before forwarding traffic to it.

The adapter is a small, stateless Go service. It receives request headers from
Envoy, selects a Sablier group from declarative host and path mappings, and
calls Sablier's dynamic strategy endpoint. It does not access Kubernetes or
scale workloads itself; Sablier remains responsible for sessions, readiness,
and workload lifecycle.

## How it works

For each request, the adapter:

1. Matches the request authority and path to a configured mapping.
2. Bypasses Sablier if the path is excluded or no mapping matches.
3. Calls Sablier once to create or renew the group's session.
4. Continues the request when the group is ready, or responds through Envoy
   while the group starts.

| Sablier result | Request method | Result |
| --- | --- | --- |
| Ready | Any | Continue to the upstream service |
| Not ready | `GET` | Return Sablier's auto-refreshing waiting page |
| Not ready | `HEAD` | Return the waiting-page headers without a body |
| Not ready | Any other method | Return `503 Service Unavailable` with `Retry-After` |
| Error | Any | Continue or return `503`, according to `failurePolicy` |

The adapter never automatically retries a non-idempotent request. It only
processes request headers; request bodies, response data, cookies, and
credentials are not sent to Sablier.

## Requirements

- Sablier 1.15.x
- Envoy with the v3 External Processing API
- Go 1.26 or later when building locally
- Docker when building the image or running the Envoy integration tests

The included Kubernetes resources target Gateway API and kgateway 2.3.x.

## Quick start

Create a `config.yaml`:

```yaml
version: 1

server:
  grpcListenAddress: ":18080"
  adminListenAddress: ":8080"

sablier:
  baseURL: "http://localhost:10000"
  requestTimeout: 3s
  maxResponseBytes: 1MiB
  sessionDuration: 1h
  failurePolicy: closed
  retryAfter: 5s

logging:
  level: info

mappings:
  - host: "app.example.com"
    pathPrefix: "/"
    group: "app"
    exclusions:
      - type: exact
        path: "/healthz"
      - type: pathPrefix
        path: "/metrics"
    waitingPage:
      displayName: "Application"
      theme: "hacker-terminal"
      showDetails: true
      refreshFrequency: 5s
```

Start Sablier, then run the adapter:

```sh
go run ./cmd/sablier-extproc -config config.yaml
```

The service listens for ext_proc gRPC traffic on port `18080` and exposes its
administration endpoints on port `8080`:

```sh
curl http://localhost:8080/livez
curl http://localhost:8080/readyz
curl http://localhost:8080/metrics
```

Envoy must be configured to send request headers to
`envoy.service.ext_proc.v3.ExternalProcessor/Process`. The complete kgateway
example in [`deploy/kubernetes/all.yaml`](deploy/kubernetes/all.yaml) contains
the expected processing mode.

## Kubernetes and kgateway

The example manifest includes:

- a ConfigMap, Deployment, and Service for `sablier-extproc`;
- a demonstration backend and HTTPRoute;
- a `GatewayExtension` configured to send request headers only; and
- a `TrafficPolicy` attaching the extension to the HTTPRoute.

Before applying it, update the Sablier URL, public hostname, Sablier group,
backend, and adapter image:

```sh
kubectl apply -f deploy/kubernetes/all.yaml
```

All example resources share one namespace. If the gateway extension, adapter,
route, or backend use different namespaces, adapt the examples in
[`deploy/kubernetes/cross-namespace-reference-grants.yaml`](deploy/kubernetes/cross-namespace-reference-grants.yaml).

There are two separate failure controls:

- `sablier.failurePolicy` controls what happens when the adapter can respond
  but its call to Sablier fails.
- `GatewayExtension.spec.extProc.failOpen` controls what happens when the
  gateway cannot reach the adapter at all.

## Configuration

Configuration is loaded once at startup. The YAML decoder rejects unknown
fields, invalid values, multiple documents, ambiguous mappings, and files
larger than 4 MiB.

Most global values have defaults:

| Field | Default | Description |
| --- | --- | --- |
| `version` | `1` | Configuration schema version |
| `server.grpcListenAddress` | `:18080` | ext_proc and gRPC health listener |
| `server.adminListenAddress` | `:8080` | Health and Prometheus HTTP listener |
| `sablier.requestTimeout` | `3s` | Timeout for a Sablier request |
| `sablier.maxResponseBytes` | `1MiB` | Maximum waiting-page response size |
| `sablier.sessionDuration` | `1h` | Default Sablier session duration |
| `sablier.failurePolicy` | `closed` | `open` continues; `closed` returns `503` |
| `sablier.retryAfter` | `5s` | Value used to derive the `Retry-After` header |
| `logging.level` | `info` | `debug`, `info`, `warn`, or `error` |

`sablier.baseURL` and at least one mapping are required. The base URL must be
an absolute HTTP(S) origin without credentials, a path, a query, or a fragment.
The maximum accepted `maxResponseBytes` value is 16 MiB.

Each mapping supports:

- an exact hostname or a wildcard such as `*.preview.example.com`;
- a segment-aware `pathPrefix`, defaulting to `/`;
- a required Sablier `group`;
- exact and path-prefix exclusions;
- waiting-page options; and
- optional `sessionDuration` and `failurePolicy` overrides.

Exact hosts take precedence over wildcards. Among wildcard mappings, the most
specific hostname suffix wins, followed by the longest path prefix. Path
prefixes are segment-aware, so `/api` matches `/api/users` but not `/api-v2`.

### Environment overrides

Environment variables take precedence over YAML for global settings. Mappings
and exclusions remain YAML-only.

| Variable | Overrides |
| --- | --- |
| `SABLIER_EXTPROC_CONFIG_FILE` | Path supplied by the `-config` flag |
| `SABLIER_EXTPROC_VERSION` | `version` |
| `SABLIER_EXTPROC_GRPC_LISTEN_ADDRESS` | `server.grpcListenAddress` |
| `SABLIER_EXTPROC_ADMIN_LISTEN_ADDRESS` | `server.adminListenAddress` |
| `SABLIER_EXTPROC_SABLIER_BASE_URL` | `sablier.baseURL` |
| `SABLIER_EXTPROC_SABLIER_REQUEST_TIMEOUT` | `sablier.requestTimeout` |
| `SABLIER_EXTPROC_MAX_RESPONSE_BYTES` | `sablier.maxResponseBytes` |
| `SABLIER_EXTPROC_SESSION_DURATION` | `sablier.sessionDuration` |
| `SABLIER_EXTPROC_FAILURE_POLICY` | `sablier.failurePolicy` |
| `SABLIER_EXTPROC_RETRY_AFTER` | `sablier.retryAfter` |
| `SABLIER_EXTPROC_LOG_LEVEL` | `logging.level` |

Durations use Go duration syntax, such as `500ms`, `5s`, or `1h`. Byte sizes
accept integers or `B`, `KB`, `MB`, `GB`, `KiB`, `MiB`, and `GiB` suffixes.

## Observability

The admin listener provides:

- `GET /livez` — process liveness;
- `GET /readyz` — listener readiness; and
- `GET /metrics` — Prometheus metrics.

The gRPC listener also implements the standard gRPC Health service. Readiness
does not depend on Sablier availability, allowing the configured failure policy
to handle Sablier outages explicitly.

Logs are structured JSON and include bounded request metadata, the selected
group, the decision, latency, and error category. Response bodies, cookies,
and credentials are not logged.

## Build and test

```sh
# Build bin/sablier-extproc
make build

# Run unit tests
make test

# Run formatting, lint, vet, tests, race detection, and vulnerability checks
make verify

# Run the real Envoy integration test (requires Docker)
make integration

# Build a local container image
make docker-build
```

To build a multi-architecture OCI archive:

```sh
make docker-build-multiarch
```

## License

This project is licensed under the [Apache License 2.0](LICENSE).

Sablier is a separate service licensed under AGPL-3.0. This adapter
communicates with it through its HTTP API and does not incorporate Sablier
source code.

## References

- [Envoy External Processing API][envoy-ext-proc]
- [Sablier][sablier]

[envoy-ext-proc]: https://www.envoyproxy.io/docs/envoy/latest/api-v3/service/ext_proc/v3/external_processor.proto
[sablier]: https://sablierapp.dev/

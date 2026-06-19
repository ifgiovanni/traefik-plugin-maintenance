# traefik-plugin-maintenance

Traefik middleware plugin that shows a maintenance page while a backend service is being updated or restarted.

For each request, the plugin queries an external **maintenance-state** HTTP service. If the service is marked as in maintenance, Traefik responds with `503 Service Unavailable` and an HTML page with a spinner. The page polls automatically and reloads once the backend is available again.

## How it works

```
CI / Jenkins  --webhook-->  maintenance-state  <--HTTP--  this plugin  -->  your app
                             (stores state)              (per-service middleware)
```

1. Your deployment pipeline calls `POST /maintenance/start` on **maintenance-state** before stopping the app.
2. This middleware checks `GET /maintenance/status?service=<serviceName>` (with a short in-memory cache).
3. While maintenance is active, users see the modal instead of hitting the backend.
4. When maintenance ends (`POST /maintenance/stop` or TTL expiry), the next poll reloads the real page.

## Yaegi constraint

Traefik plugins run inside the [Yaegi](https://github.com/traefik/yaegi) interpreter and may only use Go's **standard library**. That is why maintenance state lives in a separate HTTP service rather than Redis or another dependency inside the plugin.

## Requirements

- Traefik v2.3+ (plugins enabled)
- A running [maintenance-state](https://github.com/ifgiovanni/traefik-maintenance-mode/tree/main/maintenance-state) service (or any compatible API — see below)

### Expected API

The plugin calls:

```
GET {stateUrl}/maintenance/status?service={serviceName}
```

Expected JSON response:

```json
{ "service": "myapp", "active": true }
```

## Installation

### Option A — Local plugin (development / private deploy)

Mount the plugin source on the Traefik host. The mount path must match the `moduleName` exactly:

```yaml
command:
  - "--experimental.localPlugins.maintenance.moduleName=github.com/ifgiovanni/traefik-plugin-maintenance"
volumes:
  - ./traefik-plugin-maintenance:/plugins-local/src/github.com/ifgiovanni/traefik-plugin-maintenance:ro
```

### Option B — Plugin catalog

Publish this repository and install it from the [Traefik Plugin Catalog](https://plugins.traefik.io/), then reference it in your static configuration:

```yaml
experimental:
  plugins:
    maintenance:
      moduleName: github.com/ifgiovanni/traefik-plugin-maintenance
      version: v1.0.0
```

## Usage

Attach one middleware instance per protected service (opt-in). The `serviceName` value must match the key used when starting/stopping maintenance via webhook.

### Docker labels

```yaml
labels:
  - "traefik.http.routers.myapp.middlewares=maintenance-myapp"
  - "traefik.http.middlewares.maintenance-myapp.plugin.maintenance.serviceName=myapp"
  - "traefik.http.middlewares.maintenance-myapp.plugin.maintenance.stateUrl=http://maintenance-state:8080"
```

### Dynamic configuration (file provider)

```yaml
http:
  middlewares:
    maintenance-myapp:
      plugin:
        maintenance:
          serviceName: myapp
          stateUrl: http://maintenance-state:8080
          pollIntervalSeconds: 5
          cacheSeconds: 3
          failOpen: true
          title: "We are updating this service"
          message: "We'll be back shortly."
```

## Configuration

| Field | Required | Default | Description |
|---|---|---|---|
| `serviceName` | yes | — | Key that matches the maintenance webhook (`POST /maintenance/start {"service":"..."}`) |
| `stateUrl` | yes | — | Base URL of the maintenance-state service |
| `pollIntervalSeconds` | no | `5` | How often the browser re-checks (via `fetch`) whether maintenance has ended |
| `cacheSeconds` | no | `3` | How long the plugin caches the status response before querying again |
| `requestTimeoutMs` | no | `1500` | Timeout for the internal HTTP call to maintenance-state |
| `failOpen` | no | `true` | If maintenance-state is unreachable: `true` = pass traffic through; `false` = show the maintenance page (fail-closed) |
| `title` | no | `"We are updating this service"` | Modal heading |
| `message` | no | `"We'll be back shortly..."` | Modal body text |

## fail-open vs fail-closed

- **`failOpen: true`** (default) — If maintenance-state is down or times out, requests are proxied to the backend. Prioritizes availability.
- **`failOpen: false`** — Shows the maintenance page even when status cannot be fetched. Safer during deploys when you do not want users to hit a restarting backend.

## End-to-end example

See the parent repository's [`docker-compose.example.yml`](https://github.com/ifgiovanni/traefik-maintenance-mode/blob/main/docker-compose.example.yml) for a full stack with Traefik, maintenance-state, and a sample app.

```bash
# Start maintenance for "app"
curl -X POST http://localhost:8081/maintenance/start \
  -H 'Content-Type: application/json' \
  -d '{"service":"app","ttl_seconds":120}'

# Stop maintenance
curl -X POST http://localhost:8081/maintenance/stop \
  -H 'Content-Type: application/json' \
  -d '{"service":"app"}'
```

## License

Same license as the parent [traefik-maintenance-mode](https://github.com/ifgiovanni/traefik-maintenance-mode) project.

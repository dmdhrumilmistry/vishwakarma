# API

Base path `/api/v1`. JSON in and out; errors are `{"error": "message"}`.

**Authentication**: either the session cookie from `POST /login`, the user
header in header mode, or `Authorization: Bearer <apiToken>`.

**CSRF**: requests other than GET that are not bearer-authenticated must send
`X-Vishwakarma-Request: 1`.

The API is a contract: fields and endpoints are added, never renamed or
removed.

## Endpoints

| Method and path | Body | Result |
|---|---|---|
| `POST /login` | `{"password": "..."}` | 200, sets the session cookie (password mode) |
| `POST /logout` | | 204 |
| `GET /info` | | Caller, auth mode, VM availability, templates, policy limits |
| `GET /sandboxes` | | `{"items": [Sandbox]}`: yours, or all for an admin |
| `POST /sandboxes` | `Spec` | 201 `Sandbox` |
| `GET /sandboxes/{name}` | | `Sandbox` |
| `DELETE /sandboxes/{name}` | | 204 |
| `POST /sandboxes/{name}/stop` | | `Sandbox` |
| `POST /sandboxes/{name}/start` | | `Sandbox` |
| `POST /sandboxes/{name}/extend` | `{"ttl": "4h"}` | `Sandbox`, expiry set to now + ttl |
| `GET /sandboxes/{name}/credentials` | | `{"user", "password", "vncPassword"}` (VMs and macOS) |
| `GET /sandboxes/{name}/logs?tail=500` | | text/plain, main container output (containers) |
| `GET /sandboxes/{name}/terminal` | | WebSocket, see below |
| `GET /sandboxes/{name}/vnc` | | WebSocket with raw RFB (VNC) in binary frames, for sandboxes with `"screen": true` |

Health: `GET /healthz` (process up), `GET /readyz` (API server reachable and
the sandbox namespace readable).

Status codes: 400 invalid request or policy violation, 401 not signed in,
403 CSRF header missing or quota reached, 404 not found (also for other
users' sandboxes), 409 name taken or sandbox not running, 429 sign-in locked.

## Spec

```json
{
  "name": "web",                 // required, DNS label, at most 40 characters
  "kind": "container",           // container | vm | macos; defaults to the template's
  "template": "ubuntu-24.04",    // or omit and give image
  "image": "nginx:1.29",         // needs allowCustomImages
  "command": ["nginx", "-g", "daemon off;"],
  "args": [],
  "env": {"KEY": "value"},       // containers
  "ports": [80, 443],            // TCP; VMs default to [22]
  "expose": "cluster",           // cluster | nodeport; macOS always forwards on the Mac
  "cpu": "500m",
  "memory": "512Mi",
  "disk": "10Gi",                // /data volume for containers; disk size for macOS
  "ttl": "4h",                   // default policy defaultTTL, max maxTTL
  "privileged": false,           // containers, needs allowPrivileged
  "sshKey": "ssh-ed25519 AAAA..." // VMs and macOS
}
```

## Sandbox

```json
{
  "name": "web", "kind": "container", "template": "", "image": "nginx:1.29",
  "owner": "admin", "status": "Running", "message": "",
  "createdAt": "2026-10-02T17:44:14Z", "expiresAt": "2026-10-02T21:44:14Z",
  "cpu": "500m", "memory": "256Mi", "disk": "", "privileged": false,
  "user": "", "node": "node-1", "ip": "10.42.0.129", "expose": "nodeport",
  "endpoints": [{"port": 80, "nodePort": 31765, "protocol": "TCP", "address": "192.0.2.10:31765"}]
}
```

macOS sandboxes add `"host"` (the Mac), `"simulated"` (from the agent
simulator), and endpoints with `"name": "ssh"` or `"vnc"` whose `address` is
`<mac>:<hostPort>`. Their credentials include `"vncPassword"`.

`status` is one of `Pending`, `Running`, `Stopped`, `Failed`, `Terminating`;
`message` explains Pending and Failed (for example `ImagePullBackOff: ...`).

## Terminal WebSocket

`GET /api/v1/sandboxes/{name}/terminal` upgrades to a WebSocket. Browsers
must connect from the same origin; other clients send no Origin and a bearer
token.

- **Binary frames** carry terminal bytes both ways.
- **Text frames from the client** are JSON control messages:
  `{"type": "resize", "cols": 120, "rows": 40}` (ignored for VM consoles).
- **Text frames from the server** are status lines, such as
  `session ended`.

Containers get an exec session (`bash`, falling back to `sh`, or the
template `shell`); VMs get the serial console; macOS gets SSH through the
Mac agent, which speaks the same protocol. The server pings every 25
seconds to keep proxies from dropping idle sessions.

Example with [websocat](https://github.com/vi/websocat):

```bash
websocat -b -H "Authorization: Bearer $TOKEN" ws://localhost:8080/api/v1/sandboxes/web/terminal
```

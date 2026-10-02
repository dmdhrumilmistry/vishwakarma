# Development

Requirements: Go 1.26, Docker, Node (only for `node --check`), a Kubernetes
cluster for manual testing (k3s, kind or minikube).

## Layout

| Path | What |
|---|---|
| `cmd/vishwakarma` | `serve`, `agent`, `check-config`, `version` |
| `internal/config` | environment and policy file, defaults, validation |
| `internal/sandbox` | spec validation, object builders, list/status, lifecycle, reaper |
| `internal/kube` | clients, pod exec, KubeVirt serial console, API server discovery |
| `internal/macos` | Mac host agent (Tart and simulator backends, forwarding, SSH) and the server's Pool client |
| `internal/auth` | password sessions, header mode, bearer token, sign-in limiter |
| `internal/api` | REST handlers, error mapping, terminal WebSocket bridge |
| `internal/web/static` | console: vanilla ES modules embedded with `go:embed`, vendored xterm.js |

## Test

```bash
make test lint js
```

Unit tests run the manager against client-go fake clientsets (including a
fake dynamic client for KubeVirt objects) and the API through `httptest`.

## Run locally against a cluster

Create the sandbox namespace, then put throwaway credentials in `dev/env`
(git-ignored):

```bash
kubectl create namespace vishwakarma-sandboxes
mkdir -p dev
cat > dev/env <<EOF
VK_KUBECONFIG=$HOME/.kube/config
VK_ADMIN_PASSWORD=$(openssl rand -hex 12)
VK_SESSION_KEY=$(openssl rand -hex 32)
VK_SECURE_COOKIES=false
VK_CONFIG=dev/config.yaml
EOF
make run
```

The console is at http://localhost:8080. Without `dev/config.yaml` the
built-in policy is used.

To work on macOS without a Mac, run a simulator agent next to it and point
`macos.agents` in `dev/config.yaml` at `http://localhost:8484`, with the same
token in `VK_MACOS_TOKEN` and `VK_AGENT_TOKEN`:

```bash
vishwakarma agent --backend simulator --public-host 127.0.0.1
```

## Deploy a dev build to k3s without a registry

```bash
make image VERSION=dev
docker save ghcr.io/dmdhrumilmistry/vishwakarma:dev | ssh node 'sudo k3s ctr -n k8s.io images import -'
helm upgrade --install vishwakarma ../helm-charts/vishwakarma -n vishwakarma --create-namespace \
  --set image.tag=dev --set image.pullPolicy=Never
```

## Rules

- ASCII only in code, comments, docs and commit messages; no em or en dashes.
- Never commit secrets: no real passwords, tokens, kubeconfigs or `.env` files.
- The API is a contract: add, do not rename or remove. Document changes in
  `docs/api.md`.
- Console: no framework, no CDN, no inline scripts. Build DOM with `h()` and
  put untrusted text in through `textContent` only.

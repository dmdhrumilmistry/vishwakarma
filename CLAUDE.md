# Vishwakarma

Self-hosted throwaway containers, Android (redroid), Linux VMs (KubeVirt) and
macOS VMs (Tart on Mac hosts, through `vishwakarma agent`) for testing apps and
endpoint tools. One Go binary serves the REST API, the terminal
WebSocket and the embedded console; Kubernetes is the only store. Apache 2.0.
The Helm chart lives in github.com/dmdhrumilmistry/helm-charts (`vishwakarma/`).

See docs/development.md for layout, docs/architecture.md for the object model
and security model, docs/api.md for the API.

## Rules

- **ASCII only** in code, comments, docs, UI copy and commit messages. Never
  use em or en dashes; use a hyphen, comma, colon or parentheses.
- **Never commit secrets**: passwords, tokens, kubeconfigs, `.env`. Local
  credentials go in `dev/` (git-ignored).
- **Policy is enforced on the server**, never only in the UI: ownership,
  quotas, limits, custom images, privileged, NodePort. Any change there needs
  a test in `internal/sandbox/manager_test.go` that fails without the rule.
- **The API is a contract**: add fields and endpoints, do not rename or
  remove. Document changes in `docs/api.md`.
- **Sandbox isolation**: pods never get a service account token; every
  sandbox gets a NetworkPolicy when `network.isolate` is on. Keep it that way.
- **Console**: vanilla ES modules, no framework, no bundler, no CDN. Build DOM
  with `h()` from `js/core.js`; untrusted text only via text nodes. Use
  `h()` rather than `Element.append` with possibly-null children.
- **macOS**: the agent API (`internal/macos`) is a contract between server and
  agent versions; add fields, do not rename. Never run macOS guests outside
  Apple hardware; the simulator backend exists for testing on Linux.
- **Images**: `images/android-screen` and the Play Store images
  (`images/android-playstore/build.sh`, rooted and `UNROOTED=1`) are
  published by the release workflow.
- Keep chart RBAC in sync with what the server calls (namespaced Role only).

## Testing

`make test lint js`. For a real cluster: build the image, import it into k3s
(`docker save ... | sudo k3s ctr -n k8s.io images import -`), install the
chart, and exercise create, terminal, stop/start, extend and delete for a
container and a VM.

# Vishwakarma

Self-hosted, throwaway **containers, Linux VMs, Android and macOS** for
testing apps and endpoint tools, run from Kubernetes. Pick a template or
bring your own image, get a browser terminal in seconds, expose ports, and
let it delete itself when its time runs out.

Named after the divine architect and builder of Hindu tradition.

- **Containers** from templates (Ubuntu, Debian, Fedora, Alpine, Kali) or any
  image you want to test, with ports, environment variables and an optional
  persistent `/data` volume.
- **Virtual machines** through [KubeVirt](https://kubevirt.io) when it is
  installed: full guests with their own kernel and systemd, for agents that
  need a real OS. Cloud-init sets up a login user, a generated password and
  your SSH key.
- **Android** in a container ([redroid](https://github.com/remote-android/redroid-doc)):
  Android 12 with its screen in the browser, copy and paste, `adb` access
  and a persistent `/data`, plus an opt-in Google Play variant you build
  yourself.
- **macOS** on Mac hosts through [Tart](https://tart.run): each Mac runs
  `vishwakarma agent`; the server drives it. macOS Sequoia and Tahoe, and an
  Xcode image for the iOS Simulator, with an SSH terminal, VNC screen and
  port forwarding. On Linux nodes with `/dev/kvm`, an opt-in Docker-OSX
  template runs macOS under QEMU (not licensed by Apple on non-Apple
  hardware).
- **Screen in the browser** (noVNC) for Android, VMs and macOS.
- **Browser terminal** (xterm.js): an exec shell for containers, the serial
  console for VMs, SSH for macOS. No kubectl needed.
- **Automatic expiry**: every sandbox has a TTL (default 4h, max 72h) and is
  deleted when it lapses. Extend it from the console or the API.
- **Isolation**: each sandbox gets a NetworkPolicy. It accepts traffic only on
  its exposed ports and cannot reach other sandboxes, cluster workloads or the
  API server; internet egress is allowed (configurable).
- **Guard rails** enforced on the server: per-user quotas, CPU/memory/disk
  limits, custom images and privileged containers each switchable by the
  administrator.
- **No database.** Kubernetes is the store: a sandbox is a Deployment or a
  VirtualMachine plus owned children, found by labels.
- **REST API** with bearer tokens for CI and scripts.

## Install

The Helm chart lives in
[dmdhrumilmistry/helm-charts](https://github.com/dmdhrumilmistry/helm-charts/tree/main/vishwakarma).

```bash
helm repo add dmdhrumilmistry https://dmdhrumilmistry.github.io/helm-charts
helm install vishwakarma dmdhrumilmistry/vishwakarma \
  --namespace vishwakarma --create-namespace
kubectl port-forward -n vishwakarma svc/vishwakarma 8080:8080
```

Sign in at http://localhost:8080 with the generated admin password:

```bash
kubectl get secret -n vishwakarma vishwakarma-secrets \
  -o jsonpath='{.data.adminPassword}' | base64 -d; echo
```

For VMs, install KubeVirt first (see [docs/vms.md](docs/vms.md)). Vishwakarma
detects it automatically.

## Docs

| Doc | What |
|---|---|
| [docs/architecture.md](docs/architecture.md) | How it works, objects created per sandbox, security model |
| [docs/configuration.md](docs/configuration.md) | Policy file, templates, environment variables, auth modes |
| [docs/vms.md](docs/vms.md) | Installing KubeVirt (including on hosts without `/dev/kvm`) |
| [docs/android.md](docs/android.md) | Android sandboxes: binder, adb, scrcpy |
| [docs/macos.md](docs/macos.md) | macOS and the iOS Simulator on Mac hosts, the agent, the simulator |
| [docs/api.md](docs/api.md) | REST API and terminal WebSocket protocol |
| [docs/development.md](docs/development.md) | Building, testing and running locally |

## Quick API tour

```bash
TOKEN=$(kubectl get secret -n vishwakarma vishwakarma-secrets -o jsonpath='{.data.apiToken}' | base64 -d)
curl -H "Authorization: Bearer $TOKEN" -X POST http://localhost:8080/api/v1/sandboxes \
  -d '{"name":"web","image":"nginx:1.29-alpine","ports":[80],"expose":"nodeport","ttl":"2h"}'
curl -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/sandboxes
```

## Security notes

Vishwakarma runs code you do not trust on purpose. Read
[docs/architecture.md](docs/architecture.md#security-model) before exposing it
beyond a lab. In short: keep `allowPrivileged` off unless the cluster is
dedicated to testing, use `header` auth behind an identity proxy for
multi-user setups, and keep the console off the public internet.

## License

Apache-2.0. See [LICENSE](LICENSE). The vendored xterm.js under
`internal/web/static/vendor/xterm` is MIT.

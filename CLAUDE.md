# Vishwakarma

Self-hosted throwaway sandboxes for testing apps and endpoint tools:
containers, Android (redroid), Linux VMs (KubeVirt), macOS on Mac hosts (Tart,
through `vishwakarma agent`) and macOS on Linux nodes (Docker-OSX). One Go
binary serves the REST API, the terminal and VNC WebSockets and the embedded
console; Kubernetes is the only store. Apache 2.0.

- Chart: github.com/dmdhrumilmistry/helm-charts, directory `vishwakarma/`
  (local clone usually at `../helm-charts`).
- Images: Docker Hub only, `docker.io/dmdhrumilmistry/...` (GHCR packages
  were deleted; do not publish there again).
- Docs: docs/development.md (layout), docs/architecture.md (object and
  security model), docs/api.md, docs/configuration.md, docs/vms.md,
  docs/android.md, docs/macos.md.
- Machine-specific details (the test cluster, credentials locations) live in
  `CLAUDE.local.md`, which is git-ignored. Never copy them here.

## Rules

- **ASCII only** in code, comments, docs, UI copy and commit messages. Never
  use em or en dashes; use a hyphen, comma, colon or parentheses.
- **Never commit secrets**: passwords, tokens, kubeconfigs, `.env`, IPs of
  private machines. Local credentials go in `dev/` or `CLAUDE.local.md`
  (both git-ignored). Read tokens on the machine that holds them instead of
  echoing them into commands or chat.
- **Policy is enforced on the server**, never only in the UI: ownership,
  quotas, limits, custom images, privileged, NodePort, macOS on Linux. Any
  change there needs a test that fails without the rule
  (`internal/sandbox/*_test.go`).
- **The API is a contract**: add fields and endpoints, do not rename or
  remove. Document changes in `docs/api.md`. The macOS agent API
  (`internal/macos`) is a contract between server and agent versions too.
- **Sandbox isolation**: pods never get a service account token; every
  sandbox gets a NetworkPolicy when `network.isolate` is on. Screen ports
  admit traffic from the server namespace only.
- **Console**: vanilla ES modules, no framework, no bundler, no CDN. Build DOM
  with `h()` from `js/core.js`; untrusted text only via text nodes. Use `h()`
  rather than `Element.append` with possibly-null children (append prints
  "null"). CSP is `script-src 'self'` (xterm.js needs `style-src
  'unsafe-inline'`); Playwright string predicates trip it, use selectors.
- **Licensing**: Google Play images and macOS disks are not freely
  redistributable. Play Store images are published at the owner's request;
  macOS base images go to the PRIVATE repository
  `docker.io/dmdhrumilmistry/vishwakarma-macos:<flavor>` only. macOS on
  non-Apple hardware stays opt-in (`sandboxes.macosOnLinux`).
- Keep chart RBAC in sync with what the server calls: a namespaced Role in
  the sandbox namespace, `get` on the `kubernetes` EndpointSlice in
  `default`, and a read-only ClusterRole for `nodes` (KVM detection).

## Releasing

1. `make test lint js`, then commit to `main` (ASCII, conventional prose
   message, Co-Authored-By trailer).
2. Tag `vX.Y.Z` and push the tag. `.github/workflows/release.yml` builds and
   pushes to Docker Hub (secrets `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN`):
   the server image (amd64 and arm64), `vishwakarma-android-screen` (amd64
   only, scrcpy has no arm64 static build) and, on tags, both
   `vishwakarma-redroid-playstore` tags. Binaries go to the GitHub release.
3. In the chart repo: bump `version` (and `appVersion` and the
   `artifacthub.io/images` tags when the app changed), add an
   `artifacthub.io/changes` entry, update the root README table, then
   `helm lint vishwakarma && helm package vishwakarma -d vishwakarma && helm
   repo index vishwakarma --url
   https://dmdhrumilmistry.github.io/helm-charts/vishwakarma --merge
   index.yaml && mv vishwakarma/index.yaml index.yaml`, commit and push
   `main` (GitHub Pages serves it).
4. The chart repo checks out with CRLF on Windows; edit tools must keep line
   endings and tolerate gofmt-style column alignment.

## Testing on a real cluster

Build the image locally and import it into k3s instead of pushing:
`docker save <img> | ssh <node> 'sudo k3s ctr -n k8s.io images import -'`
(or scp a tarball, then import). Same tag plus `IfNotPresent` means a
`kubectl rollout restart` picks it up. Exercise create, terminal, screen,
stop/start, extend and delete for each kind you touched. Drive the console
headless with Playwright (`channel="chrome"`) through a port-forward to
localhost; never type real credentials into non-localhost pages.

## Hard-won knowledge

### Kubernetes and networking
- CNIs evaluate egress NetworkPolicy after Service DNAT, so blocking the
  service CIDR does not block `kubernetes.default`; the server adds the API
  server endpoint IPs to `blockCIDRs` at startup (`blockAPIServer`).
- `kubectl port-forward` dies when the target pod restarts; restart it after
  every rollout. Git Bash rewrites absolute paths in arguments
  (`/data/...` becomes `D:/Apps/Git/data/...`): use `MSYS_NO_PATHCONV=1` or
  run the command remotely.
- Memory requests decide scheduling, limits do not: a workload that really
  uses its whole limit (QEMU guests) must request it (`reserveMemory`), or it
  overcommits the node.

### Android (redroid)
- Needs a privileged container and the `binder_linux` module
  (`devices=binder,hwbinder,vndbinder`); persist it via
  `/etc/modules-load.d` and `/etc/modprobe.d`.
- redroid's init exits with SIGHUP (code 129) without a TTY: keep
  `stdin`/`tty` on the container. Android 13 and 14 images fail on newer host
  kernels (`bootstrap-apexd-failed`); 12 works.
- Images have no `/bin/sh`: template shells run `/system/bin/sh` directly.
  `sqlite3` in redroid aborts; copy databases out to read them.
- Screen sidecar: TigerVNC `Xtigervnc` + scrcpy over the pod loopback. scrcpy
  (SDL3) does not read external X clipboard owners in this headless setup,
  so browser paste is typed with `adb shell input text` (ASCII); device copy
  reaches the browser through scrcpy -> X -> Xvnc -> noVNC. Xvnc only keeps
  clipboard ownership while the VNC client stays connected.
- MindTheGapps' SetupWizard crash-loops on redroid (Wi-Fi permission) and
  hides the launcher: the Play Store build removes it. Unrooted variant:
  remove `/system/xbin/su`, set `ro.debuggable=0`, `user`, `release-keys`
  in every `build.prop` (static busybox bind-mounted into a `RUN`, since the
  image has no shell). Play Integrity device checks still fail.

### macOS on Linux (Docker-OSX)
- Only `sickcodes/docker-osx:latest` exists now (`:auto` and other prebuilt
  tags were removed). It downloads the recovery image from Apple on start
  unless the file named by `BASESYSTEM_IMAGE` exists; `NOPICKER=true` drops
  the installer and boots the installed disk; `IMAGE_PATH` moves the disk.
  QEMU runs with `-monitor stdio`, so `kubectl attach -i` can send
  `system_powerdown` for a clean shutdown.
- Without `/dev/kvm` it runs under TCG: offer no AVX/AES-NI
  (`emulatedEnv` CPUID_FLAGS) or corecrypto panics at boot; TCG adds up to
  1 GiB of translation cache, so a 3 GB guest fits a 4.5 GiB limit and a
  4 GB guest gets OOM-killed. Installing Ventura took about 3 hours.
- VMware on a Windows host running Hyper-V (WSL2, Docker Desktop) cannot
  expose VT-x to guests, so such node VMs never get `/dev/kvm`.
- Persisting and reusing an install: `images/macos-base/snapshot.sh`
  (in-cluster `qemu-img convert -c` plus kaniko push, or `NO_PUSH=1`
  tarball); `sandboxes.macosLinuxBaseImages` maps flavors to images.
- Docker Hub personal access tokens with read/write scope can push but
  cannot create repositories through the Hub API; a push to a missing
  repository creates it PUBLIC. Create private repositories in the web UI
  first.

### Console and clipboard
- Browsers expose the Clipboard API only in secure contexts; on plain HTTP
  use the `execCommand('copy')` fallback (`copyText` in core.js). xterm.js
  needs a custom key handler for Ctrl+C-with-selection and lets the
  browser's own paste event through on Ctrl+V.
- noVNC's keyboard handler swallows Ctrl+V before the paste event; take it
  in a capture listener. Legacy cut text is Latin-1 only; the extended
  clipboard is fetched lazily by the server.

# Configuration

Two sources:

- **Environment variables** for process settings and secrets.
- **A policy file** (`/etc/vishwakarma/config.yaml`, rendered by the Helm chart
  from `sandboxes.*` values) for what users may create.

Validate a policy file without a cluster:

```bash
vishwakarma check-config config.yaml
```

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `VK_LISTEN` | `:8080` | Listen address |
| `VK_CONFIG` | `/etc/vishwakarma/config.yaml` | Policy file; built-in defaults when missing |
| `VK_KUBECONFIG` | `$KUBECONFIG` | Kubeconfig for running outside the cluster; in-cluster config when empty |
| `VK_NAMESPACE` | | Overrides the policy namespace |
| `VK_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `VK_AUTH_MODE` | `password` | `password` or `header` |
| `VK_ADMIN_PASSWORD` | | Admin password (password mode), at least 12 characters |
| `VK_API_TOKEN` | | Bearer token for automation, acts as admin; at least 24 characters |
| `VK_SESSION_KEY` | | Signs session cookies, at least 32 characters |
| `VK_SESSION_TTL` | `12h` | Session lifetime |
| `VK_SECURE_COOKIES` | `true` | Secure flag on the cookie; turn off only for plain HTTP on a non-localhost address |
| `VK_AUTH_USER_HEADER` | `X-Forwarded-User` | User header (header mode) |
| `VK_ADMIN_USERS` | | Comma-separated admin users (header mode) |

## Auth modes

**password**: one built-in `admin` user. Good for a personal lab.

**header**: put an identity proxy (oauth2-proxy, Authelia, Pomerium,
Cloudflare Access...) in front of the ingress and let it pass the user name.
Every user then gets their own sandboxes and quota; users in `adminUsers`
see everything. The server trusts the header blindly, so it must not be
reachable except through the proxy (the chart's NetworkPolicy limits callers
to the ingress controller namespace).

The API token works in both modes.

## Policy file

```yaml
namespace: vishwakarma-sandboxes
publicHost: 192.168.1.10        # shown for NodePort endpoints
defaultTTL: 4h
maxTTL: 72h
maxSandboxesPerUser: 5          # admins are exempt; 0 for unlimited
allowCustomImages: true
allowPrivileged: false
allowNodePort: true
storageClass: ""                # for /data volumes; empty = cluster default
defaults: {cpu: "1", memory: 1Gi, disk: 10Gi}
limits:   {cpu: "4", memory: 8Gi, disk: 50Gi}
network:
  isolate: true
  allowEgress: true
  blockCIDRs: [10.42.0.0/16, 10.43.0.0/16]
  blockAPIServer: true
vm:
  enabled: auto                 # auto | true | false
imagePullSecrets: []
nodeSelector: {}
tolerations: []
templates: [...]                # omit for the built-in catalogue
```

Unknown keys are rejected, so typos fail at startup instead of being ignored.

### blockCIDRs per distribution

| Distribution | Pod CIDR | Service CIDR |
|---|---|---|
| k3s / RKE2 | `10.42.0.0/16` | `10.43.0.0/16` |
| kubeadm (Calico default) | `192.168.0.0/16` | `10.96.0.0/12` |
| kind | `10.244.0.0/16` | `10.96.0.0/16` |

Check yours with `kubectl cluster-info dump | grep -m1 -E 'cluster-cidr|service-cluster-ip-range'`.

## Templates

```yaml
templates:
  - name: ubuntu-24.04            # lowercase, digits, '-' and '.'
    displayName: Ubuntu 24.04
    description: Ubuntu userland in a container
    kind: container               # container | vm
    image: ubuntu:24.04
    command: [sleep, infinity]    # keeps base OS images running
    args: []
    shell: /bin/bash              # terminal shell; default tries bash then sh
    ports: [8080]
    env: {MODE: test}
    privileged: false             # needs allowPrivileged
    resources: {cpu: "2", memory: 2Gi, disk: 20Gi}

  - name: ubuntu-24.04-vm
    displayName: Ubuntu 24.04 VM
    kind: vm
    image: quay.io/containerdisks/ubuntu:24.04   # a KubeVirt containerDisk
    user: ubuntu                  # cloud-init login user
    resources: {memory: 2Gi}
    # cloudInit: |                # replaces the generated user data entirely
    #   #cloud-config
    #   ...
```

Users can override CPU, memory, ports, environment and (when
`allowCustomImages` is on) the image and command of any template; the
policy limits still apply.

### Examples for endpoint tools

```yaml
  # An agent that needs host-level access: prefer a VM.
  - name: edr-test-vm
    displayName: EDR test VM (Ubuntu)
    kind: vm
    image: quay.io/containerdisks/ubuntu:24.04
    user: ubuntu
    resources: {cpu: "2", memory: 4Gi}

  # systemd in a privileged container, for agents shipped as services.
  - name: systemd-ubuntu
    displayName: Ubuntu with systemd
    kind: container
    image: jrei/systemd-ubuntu:24.04
    command: [/lib/systemd/systemd]
    privileged: true
```

# Architecture

One Go binary serves the REST API, the terminal WebSocket and the embedded
console (vanilla ES modules, no build step). It talks to the Kubernetes API
with its own service account and keeps no state of its own.

```
browser --HTTPS--> ingress --> vishwakarma --> kube-apiserver
   |                               |                |
   +-- WebSocket /terminal --------+   exec / KubeVirt console subresource
                                                    |
                                     sandbox namespace: Deployments, VMs,
                                     Services, PVCs, Secrets, NetworkPolicies
```

## Objects per sandbox

All objects live in the sandbox namespace (`<release>-sandboxes` by default)
and carry the labels `app.kubernetes.io/managed-by=vishwakarma`,
`vishwakarma.io/sandbox=<name>`, `vishwakarma.io/kind` and
`vishwakarma.io/owner` (a hash of the owner name, which is kept readable in the
`vishwakarma.io/owner` annotation).

| Kind | Primary object | Children (owner references to the primary) |
|---|---|---|
| container | `Deployment <name>` (1 replica, Recreate) | `Service <name>` if ports, `PersistentVolumeClaim <name>-data` if a volume, `NetworkPolicy <name>` |
| vm | `VirtualMachine <name>` (kubevirt.io/v1, containerDisk root) | `Secret <name>` (cloud-init and login), `Service <name>` (port 22 by default), `NetworkPolicy <name>` |

Deleting the primary removes the children through garbage collection. The
expiry is the `vishwakarma.io/expires-at` annotation on the primary; a reaper
loop in the server deletes expired sandboxes every minute.

Stop and start scale the Deployment between 0 and 1, or set the VM
`runStrategy` to `Halted` or `Always`. A container keeps its `/data` volume
across a stop; a VM root disk is a containerDisk and resets.

## Security model

Sandboxes run arbitrary images on purpose, so the design assumes the
workload is hostile.

- **No cluster credentials in sandboxes.** Pods get
  `automountServiceAccountToken: false` and no service links.
- **Network isolation.** Each sandbox NetworkPolicy allows inbound traffic
  only on its exposed ports, and outbound traffic only to DNS in
  `kube-system` and to `0.0.0.0/0` minus `blockCIDRs` (pod and service CIDRs
  by default). At startup the server also adds the real API server endpoint
  IPs, because CNIs evaluate egress after Service DNAT: blocking the service
  CIDR alone would still let a sandbox reach `kubernetes.default`. On
  multi-node clusters add your node CIDR to `blockCIDRs` to keep sandboxes off
  the kubelets. The chart adds a default-deny ingress policy to the namespace.
- **Least privilege for the server.** A namespaced Role in the sandbox
  namespace, plus `get` on the single `kubernetes` EndpointSlice in `default`.
  No ClusterRole.
- **Privileged containers are off by default.** A privileged container can
  take over its node. Turn `allowPrivileged` on only on clusters dedicated to
  testing (endpoint agents such as EDR sensors often need it). VMs are the
  safer choice for kernel-level agents.
- **Ownership is enforced on the server.** A user can only see and act on
  sandboxes whose owner annotation matches them; admins see all. Quotas count
  per owner.
- **Authentication.** Password mode signs an HMAC session cookie
  (HttpOnly, SameSite=Strict) and locks an address out after 10 failed
  sign-ins in 15 minutes. Header mode trusts a header from an identity proxy
  and must never be reachable without it. State-changing requests need the
  `X-Vishwakarma-Request` header (CSRF), and terminal WebSockets check the
  Origin.
- **Console hardening.** A strict CSP (`script-src 'self'`), no inline
  scripts, untrusted text rendered with `textContent` only. Styles allow
  `'unsafe-inline'` because xterm.js injects `<style>` elements.
- **Cloud-init is marshalled, not templated**, so user input such as an SSH
  key cannot inject directives. SSH keys are also validated against the
  OpenSSH public key format.

What it does not do: per-sandbox resource accounting beyond limits and the
optional namespace ResourceQuota, network egress filtering by domain, or
multi-tenant isolation strong enough for mutually hostile tenants on shared
nodes (use VMs, dedicated nodes via `nodeSelector`/`tolerations`, or separate
clusters for that).

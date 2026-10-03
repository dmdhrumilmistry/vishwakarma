# macOS sandboxes (and the iOS Simulator)

macOS guests need Apple's Virtualization.framework, which only exists on
macOS on Apple hardware, and Apple's license only permits macOS VMs there.
So macOS sandboxes do not run as pods. Instead each Mac runs
`vishwakarma agent`, which drives [Tart](https://tart.run), and the
Vishwakarma server (in your cluster) calls it:

```
browser --> vishwakarma (in k3s) --HTTP + token--> vishwakarma agent (on the Mac)
                                                      |-- tart clone / run / stop / delete
                                                      |-- SSH into the guest (terminal, password)
                                                      +-- forwards guest ports to Mac host ports
```

There is no iOS VM. The iOS Simulator runs inside macOS with Xcode, so use
the `macos-xcode` template and drive it with `xcrun simctl` in the terminal
or over VNC.

## Requirements

- An Apple Silicon Mac (Tart does not support Intel Macs) on macOS 13 or
  later, reachable from the cluster.
- [Tart](https://tart.run): `brew install cirruslabs/cli/tart`.
- At most **two macOS VMs run at once per Mac**: Apple's license allows two
  guest instances and Virtualization.framework enforces it. The agent
  defaults `--max-running` to 2 and the server places new VMs on the Mac
  with the most free slots.
- Disk: macOS images are large (the base image is about 25 GB, the Xcode
  image over 60 GB). The first pull takes a while; later clones are fast.

## Set up a Mac

1. Get the shared agent token from the cluster:

   ```bash
   kubectl get secret -n vishwakarma vishwakarma-secrets \
     -o jsonpath='{.data.macosToken}' | base64 -d > vk-agent-token
   ```

   Copy it to the Mac, e.g. `~/.vishwakarma/token`, and `chmod 600` it.

2. Install the agent: download `vishwakarma-darwin-arm64` from the
   [releases](https://github.com/dmdhrumilmistry/vishwakarma/releases) to
   `/usr/local/bin/vishwakarma` and `chmod +x` it.

3. Try it in a terminal on the Mac:

   ```bash
   vishwakarma agent --public-host 192.168.1.20 --token-file ~/.vishwakarma/token
   ```

   `--public-host` is the address people use to reach forwarded ports: the
   Mac's LAN IP or DNS name.

4. Run it at login with a LaunchAgent (not a LaunchDaemon:
   Virtualization.framework only starts guests inside a logged-in user
   session). Copy [packaging/macos/io.vishwakarma.agent.plist](../packaging/macos/io.vishwakarma.agent.plist)
   to `~/Library/LaunchAgents/`, edit the public host and paths, then:

   ```bash
   launchctl load -w ~/Library/LaunchAgents/io.vishwakarma.agent.plist
   ```

   For an unattended Mac (a Mac mini in a rack) turn on automatic login so
   the session exists after a reboot.

5. Register the Mac in the chart and upgrade:

   ```yaml
   macos:
     agents:
       - name: mac-mini-1
         url: http://192.168.1.20:8484
   ```

The macOS kind then shows up in the console. Check the agent from the
cluster with `curl http://192.168.1.20:8484/healthz`.

### k3s running on the Mac itself

k3s cannot run natively on macOS; on a Mac it runs inside a Linux VM (Lima,
Colima, Rancher Desktop, OrbStack). The agent still runs natively on the
Mac, next to that VM, and the server reaches it through the host address
the VM provides:

| k3s runs in | Agent URL from the cluster |
|---|---|
| Lima / Colima / Rancher Desktop | `http://host.lima.internal:8484` |
| OrbStack | `http://host.orb.internal:8484` |
| Docker Desktop's Kubernetes | `http://host.docker.internal:8484` |

Use the Mac's LAN IP as `--public-host` so users outside the Mac can reach
forwarded ports.

### Agent flags

| Flag | Default | Meaning |
|---|---|---|
| `--backend` | `tart` | `tart`, or `simulator` for testing without a Mac |
| `--listen` | `:8484` | Agent API address |
| `--public-host` | (required) | Address users reach forwarded ports on |
| `--token-file` | `$VK_AGENT_TOKEN` | Shared token file |
| `--name` | hostname | Host name shown in the console |
| `--state` | `~/.vishwakarma/agent.json` | VM records, mode 0600 (holds guest passwords) |
| `--port-min`, `--port-max` | `20000`, `20999` | Host ports for forwarding |
| `--bind` | `0.0.0.0` | Address forwarded ports listen on |
| `--max-running` | 2 (tart) | VMs running at once |
| `--allow-images` | any | Comma-separated image prefixes, e.g. `ghcr.io/cirruslabs/` |
| `--tls-cert`, `--tls-key` | | Serve the agent API over TLS |

## What a macOS sandbox gets

- **Image**: any Tart image, by OCI reference. The built-in templates use
  [Cirrus Labs images](https://github.com/cirruslabs/macos-image-templates)
  (`macos-sequoia-base`, `macos-tahoe-base`, `macos-sequoia-xcode`), which
  ship user `admin` / password `admin` with SSH on.
- **Login**: on first boot the agent replaces the image password with a
  generated one (set `keepPassword: true` on a template to skip that) and
  adds your SSH key if you gave one. See them on the Overview tab.
- **Terminal**: an SSH session into the guest, relayed through the agent.
  The guest host key is pinned on first use.
- **Screen**: with `macos.vnc` on (default), Tart's built-in VNC server is
  forwarded. Open it with `open vnc://<mac>:<port>` (Screen Sharing on a Mac)
  or any VNC client, using the VNC password from the Overview tab.
- **Ports**: SSH, VNC and every port you expose are forwarded to host ports
  on the Mac in `--port-min`..`--port-max`. They stay the same across a stop
  and start.
- **Lifecycle**: stop and start keep the VM disk (unlike KubeVirt
  containerDisks). Delete removes the VM and its disk. Expiry works like for
  every other sandbox; the agent also deletes VMs more than 10 minutes past
  expiry by itself, in case the server is gone.
- **Restarts**: VMs keep running if the agent restarts; the agent adopts
  them again from its state file (the VNC port is lost until the VM is
  restarted).

## macOS on Linux (Docker-OSX)

For Linux clusters there is also a **real** macOS option: the
`macos-linux` template runs [Docker-OSX](https://github.com/sickcodes/Docker-OSX)
(macOS under QEMU with OpenCore) as a container. On first start it downloads
the macOS recovery image from Apple (Ventura by default; set `SHORTNAME` in
a template of your own for another version) and boots the **installer**:
open the Screen tab, then:

1. **Disk Utility**: select the `QEMU HARDDISK Media` (64 GB), **Erase** it
   as APFS (any name, for example `Macintosh HD`), then quit Disk Utility.
2. **Reinstall macOS Ventura**: agree to the license and pick that disk.
   The installer downloads macOS from Apple and reboots several times.
3. After the last reboot, pick the installed disk in the OpenCore picker if
   it does not boot on its own, and walk through the setup assistant.
4. Turn on **System Settings > General > Sharing > Remote Login** for SSH
   on port 10022.

- **KVM is used when available.** If a node offers `/dev/kvm` (KubeVirt's
  `devices.kubevirt.io/kvm`), the sandbox requests it and runs at near
  native speed. If none does, it runs under QEMU software emulation instead
  of waiting forever, and the console marks it as emulated: booting takes
  many minutes and installing macOS takes hours. Under emulation the guest
  CPU is offered without AVX and AES-NI: QEMU's emulation of those makes
  macOS's corecrypto self-test panic the kernel at boot. On a VMware or Hyper-V node
  VM, enable nested virtualization (VMware: "Virtualize Intel VT-x/EPT";
  VMware cannot do this while Windows runs Hyper-V, for example for WSL2 or
  Docker Desktop).
- **Memory is reserved in full** (4.5 GiB: a 3 GB guest plus QEMU and its
  translation cache), so the
  sandbox only lands on a node with that much free and cannot push a node
  into swap. On a small single node, trim KubeVirt (see [vms.md](vms.md)).
- **The install persists.** The sandbox gets a 50 GiB volume at `/data`;
  an init container creates the 64 GB virtual disk there once, and QEMU
  uses it (`IMAGE_PATH`). Install macOS once, then stop and start the
  sandbox freely: the OpenCore picker boots the installed system. Deleting
  the sandbox deletes the volume. The node needs about 35 GB free for an
  install (the volume is thin: it grows as macOS writes).
- **Apple's license only permits macOS on Apple hardware.** Running it on
  other hardware breaks the macOS license. The template is off until you
  set `sandboxes.macosOnLinux: true`; that decision is yours.

The server reads nodes (a read-only ClusterRole in the chart) to find out
whether any node has `/dev/kvm`.

### Install once, spawn ready

Installing macOS under emulation takes hours. Do it once, then turn that
sandbox into a base image; new sandboxes from the **macOS <Flavor> on
Linux (installed)** templates boot it directly (no installer, no recovery
download, no boot picker), in minutes once the node has cached the image.

1. Finish the install (and the Setup Assistant, if every copy should have
   your user), then shut macOS down from its Apple menu.
2. Create a **private** repository on Docker Hub. The image holds Apple's
   operating system; publishing it is redistribution.
3. Store a Docker Hub access token in the sandbox namespace (used to push
   the image and to pull it later):

   ```bash
   kubectl -n vishwakarma-sandboxes create secret docker-registry dockerhub      --docker-server=https://index.docker.io/v1/      --docker-username=<user> --docker-password=<access token>
   ```

   Or let the chart create it (`sandboxes.registryCredentials.create=true`
   with `username` and `password`); it is then named `registry-credentials`
   (pass `PUSH_SECRET=registry-credentials` to the snapshot script) and added
   to the sandbox pull secrets for you.

4. Snapshot. A Job compresses the disk and kaniko pushes it from the node;
   nothing large passes through your machine:

   ```bash
   images/macos-base/snapshot.sh macos docker.io/<user>/vishwakarma-macos:ventura
   ```

5. Point the chart at it:

   ```yaml
   sandboxes:
     macosLinuxBaseImages:
       ventura: docker.io/<user>/vishwakarma-macos:ventura
       # tahoe: docker.io/<user>/vishwakarma-macos:tahoe
     imagePullSecrets: [dockerhub]
   ```

   One private repository, one tag per flavor; each entry adds a
   "macOS <Flavor> on Linux (installed)" template.

Each new sandbox copies the disk onto its own volume on first start, so
copies are independent. The source sandbox keeps running as before.

## Trying it without a Mac: the simulator

For a Linux cluster (development, CI, a lab) the chart can run an agent in
the cluster with the simulator backend:

```yaml
macos:
  simulator:
    enabled: true
sandboxes:
  publicHost: 192.168.29.57   # a node IP
```

macOS sandboxes then go through the whole flow (create, pull, boot, stop,
start, extend, delete, expiry), the terminal is a small fake shell, and
forwarded ports (published as NodePorts `30500`-`30509`) answer with a
banner. Everything is marked **Simulated** in the console. There is no real
macOS guest: running macOS on Linux would need KVM and break Apple's
license, so the simulator stops at the agent boundary on purpose.

Run the simulator on any machine for development:

```bash
VK_AGENT_TOKEN=$(openssl rand -hex 20) vishwakarma agent --backend simulator --public-host 127.0.0.1
```

## Security

- The agent token is the only credential between the server and a Mac; it
  is generated by the chart and must be at least 24 characters. Keep the
  agent API on a trusted network or put it behind TLS (`--tls-cert`).
- The agent only manages Tart VMs it created (names start with `vk-`), and
  `--allow-images` can pin images to trusted registries.
- Forwarded ports are open on the Mac's address to anyone who can reach it,
  like NodePorts. Guest passwords are random after first boot.
- Guests share the Mac's network through Tart's NAT. They are not isolated
  from your LAN the way container sandboxes are isolated by NetworkPolicy;
  use the Mac's firewall or a separate network if that matters.

# Virtual machines

VM sandboxes need [KubeVirt](https://kubevirt.io). Vishwakarma checks for the
`kubevirt.io/v1` API every 30 seconds; once it is there, VM templates show up
in the console. Set `vm.enabled: false` to hide them anyway.

## Install KubeVirt

```bash
V=$(curl -s https://api.github.com/repos/kubevirt/kubevirt/releases/latest | grep -o '"tag_name": *"[^"]*"' | cut -d'"' -f4)
kubectl apply -f https://github.com/kubevirt/kubevirt/releases/download/$V/kubevirt-operator.yaml
kubectl apply -f https://github.com/kubevirt/kubevirt/releases/download/$V/kubevirt-cr.yaml
kubectl -n kubevirt wait kv kubevirt --for condition=Available --timeout=10m
```

### Hosts without hardware virtualization

If the nodes have no `/dev/kvm` (a VM without nested virtualization, many
cloud instances), KubeVirt can emulate the CPU with QEMU TCG. Guests are
several times slower but work, which is often enough for functional tests of
an agent:

```bash
kubectl -n kubevirt patch kubevirt kubevirt --type=merge \
  -p '{"spec":{"configuration":{"developerConfiguration":{"useEmulation":true}}}}'
```

Check with `ls /dev/kvm` on a node. On VMware, enable "Expose hardware
assisted virtualization to the guest OS" on the node VM instead to get full
speed. Under emulation start with the CirrOS template to confirm everything
works, then move to Ubuntu or Fedora (allow a few minutes for first boot).

### Small single-node clusters

KubeVirt runs two replicas of most of its components by default, about
2.7 GiB of memory requests. On a single node the second copies add nothing:

```bash
kubectl -n kubevirt patch kubevirt kubevirt --type merge -p '{"spec":{"infra":{"replicas":1}}}'
kubectl -n kubevirt scale deploy virt-operator --replicas=1
```

That frees about 0.9 GiB for sandboxes.

## How VM sandboxes work

- The root disk is a **containerDisk** image (for example
  `quay.io/containerdisks/ubuntu:24.04`). It is ephemeral: changes are lost
  when the VM stops. Any image from
  [quay.io/containerdisks](https://quay.io/organization/containerdisks) or one
  you build with a `disk/` layer works.
- **Cloud-init** creates the template `user` with a generated password and
  your SSH key. Read the login from the Overview tab or
  `GET /api/v1/sandboxes/<name>/credentials`.
- The **Console** tab is the serial console (`ttyS0`). Cloud images print a
  login prompt there.
- Port 22 is exposed by default. With `expose: nodeport` the console shows
  the `ssh` command to use.
- The network is KubeVirt masquerade on the pod network, so the same
  NetworkPolicy isolation applies as for containers.

## Building your own disk image

```dockerfile
FROM scratch
ADD --chown=107:107 my-image.qcow2 /disk/
```

Push it to a registry the cluster can pull from and add a template with
`kind: vm` and `image:` pointing at it.

# Android sandboxes

Android runs as a container with [redroid](https://github.com/remote-android/redroid-doc)
(Remote anDroid): a full Android userspace sharing the node's Linux kernel.
It starts in under a minute, keeps its data on a volume, and you connect
with `adb` and [scrcpy](https://github.com/Genymobile/scrcpy).

## Enable it

redroid needs a **privileged container** and the kernel's **binder**
driver.

1. Allow privileged templates (only templates marked privileged, with their
   own image; users still cannot run arbitrary privileged images):

   ```yaml
   sandboxes:
     allowPrivilegedTemplates: true
   ```

   A privileged container can take over its node, and anyone with a
   sandbox's terminal is root in it. Use this on clusters dedicated to
   testing, or pin Android sandboxes to dedicated nodes with
   `sandboxes.nodeSelector` and `tolerations`.

2. Load binder on every node that may run Android (Ubuntu ships it as a
   module):

   ```bash
   sudo modprobe binder_linux devices=binder,hwbinder,vndbinder
   echo binder_linux | sudo tee /etc/modules-load.d/binder.conf
   echo "options binder_linux devices=binder,hwbinder,vndbinder" | sudo tee /etc/modprobe.d/binder.conf
   ```

   Check with `grep -E 'BINDER(FS|_IPC)' /boot/config-$(uname -r)`. If
   binder is missing, Android exits right after start.

The **Android 12** template then appears in the console.

## Use it

Create it with **Reachable from: On every node (NodePort)** to reach adb
from your machine; the Overview tab shows the address and the command:

```bash
adb connect 192.168.29.57:32652
adb shell getprop ro.build.version.release
scrcpy -s 192.168.29.57:32652      # mirror and control the screen
adb install app.apk
```

The terminal tab is an Android root shell (`/system/bin/sh`), handy for
`getprop`, `pm`, `am` and `logcat`. Add a persistent volume when you create
it to keep `/data` (installed apps, settings) across stop and start.

## Versions

The template uses `redroid/redroid:12.0.0_64only-latest`, which boots on
current Ubuntu kernels. Android 13 and 14 images fail on some host kernels
(init reboots with `bootstrap-apexd-failed`); if your nodes run them, add a
template:

```yaml
sandboxes:
  templates:
    - name: android-14
      displayName: Android 14
      kind: container
      image: redroid/redroid:14.0.0_64only-latest
      args: [androidboot.redroid_gpu_mode=guest]
      shell: /system/bin/sh
      privileged: true
      ports: [5555]
      resources: {cpu: "2", memory: 3Gi}
```

Setting `templates` replaces the whole built-in catalogue, so list the
others you want too. redroid images are x86_64 or arm64 to match the node;
there is no ARM translation on x86 nodes, so apps that ship only ARM
libraries will not install.

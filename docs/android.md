# Android sandboxes

Android runs as a container with [redroid](https://github.com/remote-android/redroid-doc)
(Remote anDroid): a full Android userspace sharing the node's Linux kernel.
It starts in under a minute, keeps its data on a volume, and its **screen
is in the browser**: the console's Screen tab shows the device and takes
mouse and keyboard input. `adb` and [scrcpy](https://github.com/Genymobile/scrcpy)
work too.

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

## Screen in the browser

Every Android sandbox gets a sidecar container (`vishwakarma-android-screen`)
that connects to the device over the pod's loopback, mirrors it with scrcpy
into a virtual display and serves it over VNC. The console's **Screen** tab
shows it with noVNC: click to tap, drag to swipe, type to enter text, and
use the Back, Home and Recents buttons. Nothing extra to install or expose:
only the Vishwakarma server can reach the screen port (the sandbox
NetworkPolicy admits it from the server namespace only).

The sidecar image is x86_64 only, because scrcpy publishes static Linux
builds for x86_64. Override it with `sandboxes.androidScreenImage`.

### Copy and paste

- **Into the device:** press Ctrl+V on the screen (or type in the clipboard
  box below it and click **Paste into device**). The sidecar types the text
  into the focused field, with Enter for line breaks. It uses
  `adb shell input text`, so it is limited to ASCII.
- **From the device:** copy as usual on the device (long press, or select
  and Ctrl+C). The text appears in the clipboard box, with a **Copy** button
  to put it on your computer's clipboard.

This also works on plain HTTP, where browsers do not offer the Clipboard
API.

## Google Play (Play Store)

redroid ships without Google's apps, and Google does not allow
redistributing them, so Vishwakarma cannot publish a Play Store image.
Build one yourself from [MindTheGapps](https://github.com/MindTheGapps)
(pinned by checksum) with the script in this repository:

```bash
images/android-playstore/build.sh                       # vishwakarma/redroid-playstore:12
IMAGE=registry.lan/redroid-playstore:12 images/android-playstore/build.sh
docker push registry.lan/redroid-playstore:12          # a private registry
# or, without a registry, on each node:
docker save vishwakarma/redroid-playstore:12 | ssh node 'sudo k3s ctr -n k8s.io images import -'
```

Then set the image; the **Android 12 with Play Store** template appears:

```yaml
sandboxes:
  allowPrivilegedTemplates: true
  androidPlayStoreImage: registry.lan/redroid-playstore:12
```

The build leaves out Google's SetupWizard, which crash-loops on redroid
(it needs Wi-Fi permissions redroid does not have) and would hide the
launcher. On first boot Play services updates itself for a minute or two
before Play Store opens.

**Signing in.** Google blocks sign-in on uncertified devices until you
register the device's GSF ID at <https://www.google.com/android/uncertified>.
Read the ID (a decimal number) from the sandbox:

```bash
kubectl exec -n vishwakarma-sandboxes deploy/<name> -c sandbox -- \
  cat /data/data/com.google.android.gsf/databases/gservices.db > gservices.db
sqlite3 gservices.db "select value from main where name = 'android_id';"
```

Register it, wait a few minutes, then sign in from the Screen tab. Give the
sandbox a persistent volume so the registration and sign-in survive a stop
and start; a new sandbox has a new ID.

## adb and scrcpy from your machine

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

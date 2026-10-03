#!/bin/sh
# Builds a redroid image with Google Play (Play Store, Play services) from
# MindTheGapps, for the "Android 12 with Play Store" templates.
#
#   ./build.sh                                   # vishwakarma/redroid-playstore:12 (rooted, like stock redroid)
#   UNROOTED=1 ./build.sh                        # vishwakarma/redroid-playstore:12-unrooted
#   IMAGE=registry.lan/redroid-playstore:12 ./build.sh && docker push registry.lan/redroid-playstore:12
#   docker save vishwakarma/redroid-playstore:12 | ssh node 'sudo k3s ctr -n k8s.io images import -'
#
# UNROOTED=1 makes the device look like a production phone to apps: it removes
# su and presents a "user" build signed with release keys (ro.debuggable=0,
# ro.build.type=user, ro.build.tags=release-keys). That gets past ordinary
# root checks; it cannot pass Play Integrity's device or strong checks, which
# need hardware attestation no emulator has.
set -eu

UNROOTED="${UNROOTED:-0}"
if [ "$UNROOTED" = "1" ]; then
	IMAGE="${IMAGE:-vishwakarma/redroid-playstore:12-unrooted}"
else
	IMAGE="${IMAGE:-vishwakarma/redroid-playstore:12}"
fi
BASE="${BASE:-redroid/redroid:12.0.0_64only-latest}"
# Static busybox used only while building (redroid has no shell of its own).
BUSYBOX="${BUSYBOX:-busybox:1.37-musl}"
# https://github.com/MindTheGapps/12.1.0-x86_64/releases
GAPPS_URL="${GAPPS_URL:-https://github.com/MindTheGapps/12.1.0-x86_64/releases/download/MindTheGapps-12.1.0-x86_64-20231025_201056/MindTheGapps-12.1.0-x86_64-20231025_201056.zip}"
GAPPS_SHA256="${GAPPS_SHA256:-bc3d06d2f497189fcc6c9aa62c801572195b98d6c24085505dfa279fbeebcf6e}"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

echo "downloading MindTheGapps"
curl -fsSL -o "$work/gapps.zip" "$GAPPS_URL"
echo "$GAPPS_SHA256  $work/gapps.zip" | sha256sum -c -

mkdir -p "$work/ctx"
# Google's SetupWizard needs Wi-Fi permissions redroid does not have; it
# crash-loops and hides the launcher. Leave it out; AOSP's own provisioning
# then finishes setup, as on the plain redroid image.
(cd "$work/ctx" && unzip -q "$work/gapps.zip" && rm -rf META-INF build.prop system/addon.d system/system_ext/priv-app/SetupWizard)

cat > "$work/ctx/Dockerfile" <<EOF
# syntax=docker/dockerfile:1.7
FROM $BASE
# The Google apps go on the system partition, as a recovery flash would put them.
COPY system/ /system/
EOF

if [ "$UNROOTED" = "1" ]; then
	cat > "$work/ctx/unroot.sh" <<'EOF'
set -eu
bb=/mnt/bb/busybox
$bb rm -f /system/xbin/su
for f in $($bb find /system /vendor -name build.prop); do
	$bb sed -i \
		-e 's/^ro\.debuggable=1$/ro.debuggable=0/' \
		-e 's/userdebug/user/g' \
		-e 's/test-keys/release-keys/g' \
		"$f"
done
EOF
	cat >> "$work/ctx/Dockerfile" <<EOF
# No su, and a production ("user", release-keys) build to apps.
RUN --mount=type=bind,from=$BUSYBOX,source=/bin/busybox,target=/mnt/bb/busybox \\
    --mount=type=bind,source=unroot.sh,target=/mnt/bb/unroot.sh \\
    ["/mnt/bb/busybox", "sh", "/mnt/bb/unroot.sh"]
EOF
fi

docker build -t "$IMAGE" "$work/ctx"
echo "built $IMAGE"
echo "Point the chart at it (sandboxes.androidPlayStoreImage or androidPlayStoreUnrootedImage) and make it pullable by your nodes."

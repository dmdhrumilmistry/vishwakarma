#!/bin/sh
# Turns an installed "macOS on Linux" sandbox into a base image, so new
# sandboxes boot that system directly instead of reinstalling (hours under
# emulation). Runs entirely in the cluster: a Job compresses the sandbox
# disk and kaniko pushes the image to your registry. Nothing large passes
# through your machine.
#
#   images/macos-base/snapshot.sh <sandbox> [image]
#   images/macos-base/snapshot.sh macos docker.io/you/vishwakarma-macos:ventura
#
# Before running:
#   1. Finish the macOS install (and the Setup Assistant if you want your
#      user in every copy), then shut macOS down from its Apple menu.
#   2. Create a registry secret in the sandbox namespace; it is used to push
#      here and to pull when sandboxes start:
#        kubectl -n vishwakarma-sandboxes create secret docker-registry dockerhub \
#          --docker-server=https://index.docker.io/v1/ \
#          --docker-username=<user> --docker-password=<access token>
#   3. Make the repository PRIVATE on Docker Hub first. The image contains
#      Apple's operating system; publishing it is redistribution.
#
# Then add the image under sandboxes.macosLinuxBaseImages (flavor: image)
# and set sandboxes.imagePullSecrets=[dockerhub] in the chart.
#
# NO_PUSH=1 builds the image as a tarball on the sandbox volume instead
# (/data/.snapshot/image.tar) and needs no registry secret. Import it on the
# node with `sudo k3s ctr -n k8s.io images import <tar>` to use it locally,
# and push it later with `ctr images push` or any registry client.
set -eu

NAME="${1:?usage: snapshot.sh <sandbox> [image]}"
IMAGE="${2:-docker.io/dmdhrumilmistry/vishwakarma-macos:ventura}"
NS="${NAMESPACE:-vishwakarma-sandboxes}"
SECRET="${PUSH_SECRET:-dockerhub}"
OSX_IMAGE="${OSX_IMAGE:-sickcodes/docker-osx:latest}"
KANIKO="${KANIKO:-gcr.io/kaniko-project/executor:v1.23.2}"
JOB="$NAME-snapshot"

NO_PUSH="${NO_PUSH:-0}"
[ "$NO_PUSH" = 1 ] || kubectl -n "$NS" get secret "$SECRET" >/dev/null
kubectl -n "$NS" get pvc "$NAME-data" >/dev/null

echo "stopping $NAME so its disk is consistent"
kubectl -n "$NS" scale deploy "$NAME" --replicas=0
kubectl -n "$NS" wait --for=delete pod -l "vishwakarma.io/sandbox=$NAME" --timeout=300s || true

kubectl -n "$NS" delete job "$JOB" --ignore-not-found --wait=true
cat <<EOF | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: $JOB
  namespace: $NS
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 3600
  template:
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      initContainers:
        # Compress the disk (zeros and duplicate clusters drop out) and write
        # the build context next to it on the sandbox volume.
        - name: compress
          image: $OSX_IMAGE
          command:
            - /bin/sh
            - -c
            - |
              set -e
              rm -rf /data/.snapshot && mkdir -p /data/.snapshot/disk
              qemu-img convert -p -c -O qcow2 /data/mac_hdd_ng.img /data/.snapshot/disk/mac_hdd_ng.img
              printf 'FROM busybox:1.37-musl\nCOPY disk/mac_hdd_ng.img /disk/mac_hdd_ng.img\n' > /data/.snapshot/Dockerfile
              ls -la /data/.snapshot/disk
          volumeMounts:
            - {name: data, mountPath: /data}
      containers:
        - name: push
          image: $KANIKO
          args:
            - --context=dir:///data/.snapshot
            - --dockerfile=/data/.snapshot/Dockerfile
            - --destination=$IMAGE
            - --single-snapshot
            - --compressed-caching=false
$(if [ "$NO_PUSH" = 1 ]; then printf '            - --no-push
            - --tar-path=/data/.snapshot/image.tar
'; fi)
          volumeMounts:
            - {name: data, mountPath: /data}
$(if [ "$NO_PUSH" != 1 ]; then printf '            - {name: docker-config, mountPath: /kaniko/.docker}
'; fi)
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: $NAME-data
$(if [ "$NO_PUSH" != 1 ]; then printf '        - name: docker-config
          secret:
            secretName: %s
            items:
              - {key: .dockerconfigjson, path: config.json}
' "$SECRET"; fi)
EOF

echo "compressing and pushing (this takes a while)"
while :; do
	ok=$(kubectl -n "$NS" get job "$JOB" -o jsonpath='{.status.succeeded}')
	bad=$(kubectl -n "$NS" get job "$JOB" -o jsonpath='{.status.failed}')
	[ "${ok:-0}" -ge 1 ] && break
	if [ "${bad:-0}" -ge 1 ]; then
		kubectl -n "$NS" logs "job/$JOB" --all-containers --tail=30
		echo "snapshot failed; $NAME stays stopped (start it from the console)" >&2
		exit 1
	fi
	sleep 30
done
kubectl -n "$NS" logs "job/$JOB" -c push --tail=5

echo "starting $NAME again"
kubectl -n "$NS" scale deploy "$NAME" --replicas=1
if [ "$NO_PUSH" = 1 ]; then
	echo "built $IMAGE as /data/.snapshot/image.tar on the $NAME-data volume"
else
	echo "pushed $IMAGE"
fi
echo "The build context stays at /data/.snapshot on the sandbox volume until you delete it or the sandbox."

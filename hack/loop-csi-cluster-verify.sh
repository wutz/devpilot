#!/usr/bin/env bash
# Copyright 2026 wutz.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Verifies loop.devpilot.wutz.dev on a real multi-node cluster against a pool
# directory that lives on a shared filesystem (VAST NFS, Storage Scale,
# CephFS). It provisions a volume, exercises it from two nodes, expands it,
# snapshots and restores it, and checks the single-writer fence.
#
# Both nodes must already have the shared filesystem mounted at POOL_DIR's
# parent (the script creates POOL_DIR itself). Every node the driver runs on
# needs that mount; use a pool path that is not on the node's local disk.
#
#   NODE_A=gn-1 NODE_B=gn-2 \
#   IMAGE=registry.example/devpilot-loop-csi:v1 \
#   POOL_DIR=/mnt/vast/devpilot-rootdisks ./hack/loop-csi-cluster-verify.sh
#
# Env:
#   NODE_A, NODE_B   schedulable nodes with the shared filesystem mounted
#   IMAGE            devpilot-loop-csi image (required)
#   POOL_NAME        pool name used in the StorageClass (default: shared)
#   POOL_DIR         pool directory on the shared filesystem (default: /mnt/devpilot-loop-pool)
#   SC_NAME          StorageClass to create (default: loop-verify-$POOL_NAME)
#   DIRECT_IO        directIO parameter for StorageClass (default: required)
#   NS               namespace for the test resources (default: devpilot-loop-verify)
#   KEEP=1           keep everything on exit instead of cleaning up
#   PULL_SECRET_SRC  <namespace>/<name> of an image pull secret to copy into NS (optional)
#   PULL_SECRET      name of the copied secret (default: harbor-pull)
set -euo pipefail

cd "$(dirname "$0")/.."

: "${IMAGE:?set IMAGE to the devpilot-loop-csi image reference}"
: "${NODE_A:?set NODE_A to the first node}"
: "${NODE_B:?set NODE_B to the second node}"
POOL_NAME=${POOL_NAME:-shared}
POOL_DIR=${POOL_DIR:-/mnt/devpilot-loop-pool}
SC_NAME=${SC_NAME:-loop-verify-$POOL_NAME}
DIRECT_IO=${DIRECT_IO:-required}
NS=${NS:-devpilot-loop-verify}
KEEP=${KEEP:-0}
PULL_SECRET=${PULL_SECRET:-harbor-pull}
PULL_SECRET_SRC=${PULL_SECRET_SRC:-}
DRIVER=loop.devpilot.wutz.dev
SHELL_IMAGE=${SHELL_IMAGE:-ubuntu:22.04}
LABEL_KEY=devpilot.wutz.dev/loop-verify

PASS=0
FAIL=0

pass() { PASS=$((PASS + 1)); echo "PASS  $*"; }
fail() { FAIL=$((FAIL + 1)); echo "FAIL  $*"; }
note() { echo "NOTE  $*"; }
section() { echo; echo "== $* =="; }

kc() { kubectl -n "$NS" "$@"; }

wait_for() { # wait_for <timeout-seconds> <description> <command...>
	local timeout=$1 desc=$2
	shift 2
	local deadline=$((SECONDS + timeout))
	while ((SECONDS < deadline)); do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 3
	done
	echo "timed out waiting for $desc" >&2
	return 1
}

pod_running_on() { # pod_running_on <pod> <node>
	[[ "$(kubectl -n "$NS" get pod "$1" -o jsonpath='{.spec.nodeName} {.status.phase}' 2>/dev/null)" == "$2 Running" ]]
}

shell_pod() { # shell_pod <node> -> pod name of the privileged helper on that node
	kubectl -n "$NS" get pod -l app.kubernetes.io/component=verify-shell \
		--field-selector "spec.nodeName=$1" -o jsonpath='{.items[0].metadata.name}'
}

on_node() { # on_node <node> <command...> - run a shell command in the node's mount namespace
	local node=$1
	shift
	kc exec "$(shell_pod "$node")" -- nsenter -t 1 -m -- "$@"
}

pool_du() { # pool_du <node> <file> -> allocated bytes of a file
	on_node "$1" du -s --block-size=1 "$2" | cut -f1
}

volume_unpublished() { # volume_unpublished <node>
	! on_node "$1" ls /var/lib/kubelet/plugins/$DRIVER/mounts 2>/dev/null | grep -q "^${POOL_NAME}_"
}

pod_sha() { # pod_sha <pod> <dir> -> stable digest of the tree's names and contents
	kc exec "$1" -- bash -c "cd $2 && find . -type f | sort | xargs -r sha256sum | sha256sum" | cut -d' ' -f1
}

cleanup() {
	if [[ $KEEP == 1 ]]; then
		echo
		echo "KEEP=1: leaving namespace $NS, StorageClass $SC_NAME and pool $POOL_DIR in place"
		return
	fi
	section "cleanup"
	kc delete pod --all --force --grace-period=0 --ignore-not-found --wait=false >/dev/null 2>&1 || true
	kc delete volumesnapshot --all --ignore-not-found --wait=false >/dev/null 2>&1 || true
	kc delete pvc --all --ignore-not-found --wait=false >/dev/null 2>&1 || true
	# The driver has to outlive the snapshots and volumes: their finalizers are
	# only released once DeleteSnapshot and DeleteVolume have run.
	wait_for 120 "snapshots and PVCs to be deleted" bash -c "
		[[ -z \$(kubectl -n $NS get volumesnapshot,pvc -o name 2>/dev/null) ]]" ||
		note "snapshots or PVCs still present; deleting the driver anyway"
	kc delete daemonset,deployment --all --ignore-not-found --wait=false >/dev/null 2>&1 || true
	kubectl delete csidriver "$DRIVER" --ignore-not-found >/dev/null 2>&1 || true
	kubectl delete storageclass "$SC_NAME" --ignore-not-found >/dev/null 2>&1 || true
	kubectl delete volumesnapshotclass "$SC_NAME-snap" --ignore-not-found >/dev/null 2>&1 || true
	kubectl delete clusterrole,clusterrolebinding loop-verify-devpilot-loop-csi-controller loop-verify-devpilot-loop-csi-node --ignore-not-found >/dev/null 2>&1 || true
	kubectl label node "$NODE_A" "$NODE_B" "$LABEL_KEY"- >/dev/null 2>&1 || true
	# Leftover PVs keep their pool directory, so remove both.
	kubectl get pv -o json 2>/dev/null |
		python3 -c '
import json, sys
for pv in json.load(sys.stdin)["items"]:
    if pv.get("spec", {}).get("storageClassName") == sys.argv[1]:
        print(pv["metadata"]["name"])
' "$SC_NAME" | xargs -r kubectl delete pv --wait=false
	for n in "$NODE_A" "$NODE_B"; do
		# Never delete a mount point: the pool directory is ours, its parent
		# mount is not.
		if on_node "$n" test -d "$POOL_DIR" && ! on_node "$n" mountpoint -q "$POOL_DIR" 2>/dev/null; then
			on_node "$n" rm -rf "$POOL_DIR" >/dev/null 2>&1 || true
		fi
	done
	kubectl delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	echo "cleanup requested"
}
trap cleanup EXIT

section "preflight"
cordoned=$(kubectl get nodes "$NODE_A" "$NODE_B" -o jsonpath='{range .items[*]}{.metadata.name}={.spec.unschedulable}{" "}{end}')
echo "nodes: $cordoned"
if grep -q "true" <<<"$cordoned"; then
	fail "both nodes must be schedulable (uncordon them first)"
	exit 1
fi
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl label node "$NODE_A" "$NODE_B" "$LABEL_KEY=true" --overwrite >/dev/null
if [[ -n $PULL_SECRET_SRC ]]; then
	# PULL_SECRET_SRC is <namespace>/<name>; copy it here and attach it to the
	# default service account so the test pods can pull IMAGE as well.
	kubectl -n "${PULL_SECRET_SRC%%/*}" get secret "${PULL_SECRET_SRC##*/}" -o json |
		python3 -c '
import json, sys
s = json.load(sys.stdin)
print(json.dumps({"apiVersion": "v1", "kind": "Secret", "type": s["type"],
                  "metadata": {"name": sys.argv[1]}, "data": s["data"]}))
' "$PULL_SECRET" | kc apply -f - >/dev/null
	kc patch serviceaccount default -p "{\"imagePullSecrets\":[{\"name\":\"$PULL_SECRET\"}]}" >/dev/null
fi
pass "namespace $NS ready and nodes labeled $LABEL_KEY"

# A privileged helper on the pool nodes: creates the pool directory before the
# driver starts (hostPath type Directory needs it) and measures the image file.
cat <<EOF | kc apply -f - >/dev/null
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: verify-shell
spec:
  selector:
    matchLabels: {app.kubernetes.io/component: verify-shell}
  template:
    metadata:
      labels: {app.kubernetes.io/component: verify-shell}
    spec:
      hostPID: true
      hostNetwork: true
      tolerations: [{operator: Exists}]
      nodeSelector: {$LABEL_KEY: "true"}
      containers:
        - name: shell
          image: $SHELL_IMAGE
          command: ["sleep", "infinity"]
          securityContext: {privileged: true}
          volumeMounts: [{name: host, mountPath: /host}]
      volumes:
        - name: host
          hostPath: {path: /}
EOF
for n in "$NODE_A" "$NODE_B"; do
	wait_for 180 "verify-shell on $n" bash -c "
		[[ \$(kubectl -n $NS get pod -l app.kubernetes.io/component=verify-shell \
			--field-selector spec.nodeName=$n -o jsonpath='{.items[0].status.phase}') == Running ]]" ||
		{ fail "verify-shell did not start on $n"; exit 1; }
done
for n in "$NODE_A" "$NODE_B"; do
	on_node "$n" mkdir -p "$POOL_DIR"
done
pass "pool directory $POOL_DIR exists on both nodes"

# The cross-node cases are meaningless on a node-local directory, so refuse
# one: both nodes must see the same mount source, and a marker written on A
# must be readable on B.
MARKER="$POOL_DIR/.verify-marker"
mount_a=$(on_node "$NODE_A" findmnt -T "$POOL_DIR" -n -o SOURCE,FSTYPE)
mount_b=$(on_node "$NODE_B" findmnt -T "$POOL_DIR" -n -o SOURCE,FSTYPE)
note "pool mount on $NODE_A: $mount_a"
note "pool mount on $NODE_B: $mount_b"
token="verify-$$-$RANDOM"
on_node "$NODE_A" sh -c "echo $token > $MARKER && sync"
seen=$(on_node "$NODE_B" cat "$MARKER" 2>/dev/null || true)
on_node "$NODE_A" rm -f "$MARKER" || true
if [[ $mount_a != "$mount_b" || $seen != "$token" ]]; then
	fail "POOL_DIR $POOL_DIR is not one shared filesystem on $NODE_A and $NODE_B; mount the shared filesystem there first"
	exit 1
fi
pass "pool directory is the same filesystem on both nodes ($mount_a)"

section "deploy the driver"
cat >/tmp/loop-verify-values-$$.yaml <<EOF
imagePullSecrets: [{name: $PULL_SECRET}]
loopCSI:
  enabled: true
  image: {repository: ${IMAGE%:*}, tag: ${IMAGE##*:}, pullPolicy: IfNotPresent}
  pools:
    - name: $POOL_NAME
      path: $POOL_DIR
  storageClasses:
    - name: $SC_NAME
      pool: $POOL_NAME
      directIO: "$DIRECT_IO"
      trimOnUnmount: true
  snapshotClass:
    create: true
    name: $SC_NAME-snap
    strategy: auto
    allowLiveCopy: true
  controller:
    nodeSelector: {$LABEL_KEY: "true"}
  node:
    nodeSelector: {$LABEL_KEY: "true"}
EOF
helm template loop-verify ./charts/devpilot -n "$NS" -f /tmp/loop-verify-values-$$.yaml \
	--show-only templates/loop-csi.yaml >/tmp/loop-verify-rendered-$$.yaml
kc apply -f /tmp/loop-verify-rendered-$$.yaml >/dev/null
rm -f /tmp/loop-verify-values-$$.yaml /tmp/loop-verify-rendered-$$.yaml
wait_for 180 "controller" bash -c "
  [[ \$(kubectl -n $NS get pod -l app.kubernetes.io/component=loop-csi-controller -o jsonpath='{.items[0].status.containerStatuses[*].ready}' | tr ' ' '\n' | grep -c true) == 4 ]]"
wait_for 180 "node plugin on both nodes" bash -c "
  [[ \$(kubectl -n $NS get pod -l app.kubernetes.io/component=loop-csi-node --field-selector spec.nodeName=$NODE_A -o jsonpath='{.items[0].status.containerStatuses[*].ready}' | tr ' ' '\n' | grep -c true) == 2 &&
     \$(kubectl -n $NS get pod -l app.kubernetes.io/component=loop-csi-node --field-selector spec.nodeName=$NODE_B -o jsonpath='{.items[0].status.containerStatuses[*].ready}' | tr ' ' '\n' | grep -c true) == 2 ]]"
pass "driver controller and node plugins are ready"

section "T1 provision"
cat <<EOF | kc apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: verify-a}
spec:
  storageClassName: $SC_NAME
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 2Gi}}
EOF
wait_for 120 "PVC bound" bash -c "[[ \$(kubectl -n $NS get pvc verify-a -o jsonpath='{.status.phase}') == Bound ]]"
VOL=$(kc get pvc verify-a -o jsonpath='{.spec.volumeName}')
IMG="$POOL_DIR/$VOL/disk.img"
if on_node "$NODE_A" test -f "$IMG"; then
	pass "T1 disk.img created at $IMG ($(on_node "$NODE_A" du -sh "$IMG" | cut -f1) allocated)"
else
	fail "T1 $IMG not found"
fi

section "T2 mount and write"
cat <<EOF | kc apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: verify-a-a}
spec:
  nodeSelector: {kubernetes.io/hostname: $NODE_A}
  containers:
    - name: shell
      image: $IMAGE
      command: ["sleep", "infinity"]
      volumeMounts: [{name: root, mountPath: /data}]
  volumes:
    - name: root
      persistentVolumeClaim: {claimName: verify-a}
EOF
wait_for 180 "pod on $NODE_A" pod_running_on verify-a-a "$NODE_A"
pass "T2 published on $NODE_A"
kc exec verify-a-a -- bash -c '
  set -e
  mkdir -p /data/project/.cache /data/tmp
  dd if=/dev/urandom of=/data/project/big.bin bs=1M count=256 status=none
  for i in $(seq 1 500); do echo "file-$i" > /data/project/.cache/f$i; done
  ln -s /data/project/big.bin /data/project/link
  df -hT /data | tail -1 > /data/df.txt
  stat -f -c "%T" /data
'
DIGEST_A=$(pod_sha verify-a-a /data/project)
note "T2 digest $DIGEST_A ($(kc exec verify-a-a -- cat /data/df.txt))"
pass "T2 wrote 256 MiB, 500 small files and a symlink"

section "T3 online expansion 2Gi -> 4Gi"
kc patch pvc verify-a -p '{"spec":{"resources":{"requests":{"storage":"4Gi"}}}}' --type=merge >/dev/null
wait_for 300 "filesystem expanded" bash -c "
  size=\$(kubectl -n $NS exec verify-a-a -- df -BG --output=size /data | tail -1 | tr -dc 0-9)
  [[ -n \$size && \$size -ge 3 ]]"
pass "T3 filesystem grew on the mounted volume ($(kc exec verify-a-a -- df -BG --output=size /data | tail -1))"

section "T4 single-writer fence from the second node"
sed "s/NAME/verify-a-b/;s/NODE/$NODE_B/" <<EOF | kc apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: NAME}
spec:
  nodeSelector: {kubernetes.io/hostname: NODE}
  containers:
    - name: shell
      image: $IMAGE
      command: ["sleep", "infinity"]
      volumeMounts: [{name: root, mountPath: /data}]
  volumes:
    - name: root
      persistentVolumeClaim: {claimName: verify-a}
EOF
fenced=0
for _ in $(seq 1 40); do
	if kc get pod verify-a-b -o jsonpath='{.status.phase}' | grep -q Running; then break; fi
	if kc describe pod verify-a-b 2>/dev/null | grep -qi "in use\|FailedPrecondition\|mounted on another node"; then fenced=1; break; fi
	sleep 3
done
if [[ $fenced == 1 ]] && ! pod_running_on verify-a-b "$NODE_B"; then
	pass "T4 second writer on $NODE_B was refused by the driver"
	kc describe pod verify-a-b 2>/dev/null | grep -i "in use\|FailedPrecondition" | tail -1
else
	fail "T4 the volume mounted read-write on two nodes at once"
fi
kc delete pod verify-a-b --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true

section "T5 snapshot and restore"
cat <<EOF | kc apply -f - >/dev/null
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: verify-snap}
spec:
  volumeSnapshotClassName: $SC_NAME-snap
  source: {persistentVolumeClaimName: verify-a}
EOF
wait_for 300 "snapshot ready" bash -c "[[ \$(kubectl -n $NS get volumesnapshot verify-snap -o jsonpath='{.status.readyToUse}') == true ]]"
pass "T5 snapshot readyToUse"
cat <<EOF | kc apply -f - >/dev/null
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: verify-restore}
spec:
  storageClassName: $SC_NAME
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 4Gi}}
  dataSource: {name: verify-snap, kind: VolumeSnapshot, apiGroup: snapshot.storage.k8s.io}
EOF
wait_for 300 "restore PVC bound" bash -c "[[ \$(kubectl -n $NS get pvc verify-restore -o jsonpath='{.status.phase}') == Bound ]]"
sed "s/NAME/verify-restore-a/;s/CLAIM/verify-restore/;s/NODE/$NODE_A/" <<EOF | kc apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: NAME}
spec:
  nodeSelector: {kubernetes.io/hostname: NODE}
  containers:
    - name: shell
      image: $IMAGE
      command: ["sleep", "infinity"]
      volumeMounts: [{name: root, mountPath: /data}]
  volumes:
    - name: root
      persistentVolumeClaim: {claimName: CLAIM}
EOF
wait_for 180 "restore pod" pod_running_on verify-restore-a "$NODE_A"
DIGEST_R=$(pod_sha verify-restore-a /data/project)
if [[ "$DIGEST_A" == "$DIGEST_R" ]]; then
	pass "T5 restored volume matches the source ($DIGEST_R)"
else
	fail "T5 restored tree differs: $DIGEST_A != $DIGEST_R"
fi
kc delete pod verify-restore-a --force --grace-period=0 --ignore-not-found >/dev/null 2>&1 || true

section "T6 cross-node takeover"
kc delete pod verify-a-a --force --grace-period=0 >/dev/null 2>&1 || true
# NodeUnpublish removes the mount under the plugin's state directory and
# detaches the loop device; wait for that state to clear before taking over.
wait_for 120 "volume unpublished on $NODE_A" volume_unpublished "$NODE_A" ||
	note "volume still present in the node state, continuing anyway"
sed "s/NAME/verify-a-c/;s/NODE/$NODE_B/" <<EOF | kc apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: NAME}
spec:
  nodeSelector: {kubernetes.io/hostname: NODE}
  containers:
    - name: shell
      image: $IMAGE
      command: ["sleep", "infinity"]
      volumeMounts: [{name: root, mountPath: /data}]
  volumes:
    - name: root
      persistentVolumeClaim: {claimName: verify-a}
EOF
if wait_for 240 "pod on $NODE_B" pod_running_on verify-a-c "$NODE_B"; then
	DIGEST_B=$(pod_sha verify-a-c /data/project)
	if [[ "$DIGEST_A" == "$DIGEST_B" ]]; then
		pass "T6 volume moved to $NODE_B with identical contents ($DIGEST_B)"
	else
		fail "T6 contents differ after the move: $DIGEST_A != $DIGEST_B"
	fi
else
	fail "T6 volume did not mount on $NODE_B (the previous writer may still hold it)"
fi

section "T7 trim on unmount"
BEFORE=$(pool_du "$NODE_B" "$IMG")
note "T7 allocated before unpublish: $BEFORE bytes"
kc exec verify-a-c -- bash -c 'rm -rf /data/project /data/tmp; sync' >/dev/null 2>&1 || true
kc delete pod verify-a-c --force --grace-period=0 >/dev/null 2>&1 || true
sleep 20
AFTER=$(pool_du "$NODE_B" "$IMG")
note "T7 allocated after unpublish: $AFTER bytes (file size $(on_node "$NODE_B" stat -c %s "$IMG"))"
if ((AFTER < BEFORE)); then
	pass "T7 FITRIM returned space to the pool"
else
	note "T7 the shared filesystem did not reclaim space (expected where punch-hole is unsupported)"
fi

section "summary"
echo "passed: $PASS  failed: $FAIL"
[[ $FAIL == 0 ]]

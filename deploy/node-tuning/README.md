# DevPilot GPU Node Tuning Reference (30–100 GB AI Images)

This directory contains reference node and cluster configurations that complement DevPilot's operator-level image acceleration (`lastNodeName` sticky affinity, `ImagePrewarm`, two-phase GPU-free `--prepull`, and `devpilot-commit` `zstd`/`zstd:chunked` output).

## Quick Checklist

1. **Dedicated NVMe for containerd** (`>= 2 TB` recommended on GPU nodes):
   - Mount `/var/lib/containerd` on a dedicated local NVMe volume to avoid root-disk exhaustion and Kubelet cache thrashing.
2. **Kubelet Image GC & Parallel Pulls** ([kubelet-image-gc.yaml](kubelet-image-gc.yaml)):
   - `imageGCHighThresholdPercent: 85`
   - `imageGCLowThresholdPercent: 70`
   - `imageMinimumGCAge: 24h`
   - `serializeImagePulls: false` + `maxParallelImagePulls: 5`
3. **containerd 2.x + Spegel P2P** ([containerd-config.toml](containerd-config.toml), [spegel-values.yaml](spegel-values.yaml)):
   - **Must set `discard_unpacked_layers = false`** so Spegel can serve compressed blobs to peer nodes over the 25G/100G cluster fabric.
   - Raise `max_concurrent_downloads = 10` so 70+ layer AI images do not queue behind 3-stream defaults.
   - Configure Spegel with `resolveLatestTag: false` (DevPilot pins immutable `@sha256:...` digests before Pod creation).
4. **kube-scheduler `ImageLocality` Weight**:
   - Increase the `ImageLocality` plugin weight in `KubeSchedulerConfiguration` (e.g., `weight: 3` or higher alongside DevPilot's `weight: 80` `lastNodeName` soft affinity) so multi-node clusters favor nodes that already hold large CUDA base layers.
5. **P2 Lazy Pulling with `zstd:chunked`** ([stargz-snapshotter-config.toml](stargz-snapshotter-config.toml)):
   - Deploy `stargz-snapshotter` with `zstd:chunked` and background prefetch enabled (`devpilot-commit --compression=zstd:chunked` emits standard OCI layers with `zstd:chunked` TOC footers and prioritized PyTorch/CUDA landmarks).
   - Kernel overlay stack depth remains `2 <= FILESYSTEM_MAX_STACK_DEPTH`:
     - Snapshotter layer mount (`fuse`/`erofs`, `s_stack_depth = 0`) -> containerd rootfs `overlay` (`s_stack_depth = 1`) -> `devpilot-init` persistent `upper/` `overlay` (`s_stack_depth = 2`).

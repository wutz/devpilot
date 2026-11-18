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

# Runs the loop CSI and loopdev tests that need a real kernel: loop devices,
# ext4, MMP, FIFREEZE/FITRIM and OFD locks. Docker runs them in a privileged
# container; the pool is a Docker volume so it sits on the VM's ext4 disk
# (overlayfs and tmpfs do not give loop devices O_DIRECT).
set -euo pipefail
cd "$(dirname "$0")/.."
GO_IMAGE=${GO_IMAGE:-golang:1.26-bookworm}
docker run --rm --privileged \
  -v "$PWD":/src -w /src \
  -v devpilot-gomod:/go/pkg/mod -v devpilot-gocache:/root/.cache/go-build \
  -v devpilot-loop-pool:/pool \
  -e DEVPILOT_LOOP_E2E_POOL=/pool \
  "$GO_IMAGE" bash -c '
    set -e
    command -v mkfs.ext4 >/dev/null && command -v debugfs >/dev/null || { apt-get update -qq && apt-get install -y -qq e2fsprogs >/dev/null; }
    go test -count=1 -v -run "E2E|Linux" ./internal/loopdev/ ./internal/loopcsi/ "$@"
  ' bash "$@"

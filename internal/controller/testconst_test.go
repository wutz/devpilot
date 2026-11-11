/*
Copyright 2026 wutz.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

// Storage classes and snapshot classes used by the bz1-dev1 cluster fixtures.
const (
	testStorageClassNVMe  = "block-nvme"
	testStorageClassVast  = "block-vast-cs"
	testSnapshotClassRBD  = "csi-rbdplugin-snapclass"
	testSnapshotClassVast = "vast-snapshot-cs"
	testDevBoxImage       = "pytorch/pytorch:2.4.0-cuda12.4-cudnn9-devel"
	testCommitRepository  = "registry.example.com/team/my-devbox"
	testVolumeSize        = "50Gi"
)

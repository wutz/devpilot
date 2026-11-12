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

package dashboard

// devBoxView is the JSON shape the console renders for one devbox.
//
// It is a deliberate subset of the CRD: the console shows power state, where it
// runs, how it is reached, and what it has cost. Anything a reader needs beyond
// that is one kubectl away, and keeping the view narrow keeps the API honest
// about what the console understands.
type devBoxView struct {
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	DisplayName   string `json:"displayName,omitempty"`
	Phase         string `json:"phase"`
	DesiredState  string `json:"desiredState"`
	Message       string `json:"message,omitempty"`
	Image         string `json:"image"`
	ActiveImage   string `json:"activeImage,omitempty"`
	GPUs          int32  `json:"gpus"`
	GPUModel      string `json:"gpuModel,omitempty"`
	CPU           string `json:"cpu,omitempty"`
	Memory        string `json:"memory,omitempty"`
	RootDiskSize  string `json:"rootDiskSize,omitempty"`
	NodeName      string `json:"nodeName,omitempty"`
	PricePerHour  string `json:"pricePerHour,omitempty"`
	BilledSeconds int64  `json:"billedSeconds"`
	TotalCost     string `json:"totalCost,omitempty"`
	// Endpoints maps "ssh", "jupyter" and "port/<name>" to their URLs.
	Endpoints  map[string]string `json:"endpoints,omitempty"`
	StoppedAt  string            `json:"stoppedAt,omitempty"`
	ReleaseAt  string            `json:"releaseAt,omitempty"`
	Conditions []conditionView   `json:"conditions,omitempty"`
	Segments   []segmentView     `json:"segments,omitempty"`
}

// conditionView is one status condition.
type conditionView struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	Since   string `json:"since"`
}

// segmentView is one billing segment.
type segmentView struct {
	ID            int64  `json:"id"`
	NodeName      string `json:"nodeName,omitempty"`
	PricePerHour  string `json:"pricePerHour,omitempty"`
	StartTime     string `json:"startTime"`
	EndTime       string `json:"endTime,omitempty"`
	BilledSeconds int64  `json:"billedSeconds"`
	Cost          string `json:"cost,omitempty"`
	EndReason     string `json:"endReason,omitempty"`
	Running       bool   `json:"running"`
}

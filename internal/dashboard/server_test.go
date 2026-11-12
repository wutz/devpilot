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

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

const (
	// testToken is the token the fake client is handed for every accepted request.
	testToken = "test-token"
	// testNamespace and testBox name the devbox most tests read.
	testNamespace = "devpilot-verify"
	testBox       = "gpu-box"
	// testCost is the price of the fixture's closed billing segment.
	testCost = "4.20"
)

// newServer builds a console over a fake client and records the tokens it saw.
func newServer(t *testing.T, objects ...client.Object) (*Server, *[]string) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := devpilotv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add devpilot scheme: %v", err)
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	seen := &[]string{}
	server := New(scheme, nil, logr.Discard())
	server.ClientFor = func(_ context.Context, token string) (client.Client, error) {
		*seen = append(*seen, token)
		return fakeClient, nil
	}
	return server, seen
}

// call sends a request through the console handler.
func call(server *Server, method, target, body string, authorized bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if authorized {
		request.Header.Set("Authorization", "Bearer "+testToken)
	}
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	return recorder
}

// testDevBox is one devbox with a billing ledger and endpoints.
func testDevBox(namespace, name string) *devpilotv1alpha1.DevBox {
	ended := metav1.Now()
	return &devpilotv1alpha1.DevBox{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: devpilotv1alpha1.DevBoxSpec{
			DisplayName:  "PyTorch 开发机",
			DesiredState: devpilotv1alpha1.DesiredStateRunning,
			PricePerHour: "2.80",
			Image:        "nvcr.io/nvidia/pytorch:25.08-py3",
			Resources: devpilotv1alpha1.DevBoxResources{
				GPUs:     1,
				GPUModel: "RTX 5090",
				CPU:      resource.MustParse("8"),
				Memory:   resource.MustParse("32Gi"),
			},
			RootDisk: devpilotv1alpha1.RootDiskSpec{
				Size:             resource.MustParse("200Gi"),
				StorageClassName: "block-nvme",
			},
		},
		Status: devpilotv1alpha1.DevBoxStatus{
			Phase:              devpilotv1alpha1.PhaseRunning,
			ActiveImage:        "nvcr.io/nvidia/pytorch:25.08-py3",
			NodeName:           "gn-10-243-145-105",
			TotalBilledSeconds: 5400,
			TotalCost:          testCost,
			Endpoints: devpilotv1alpha1.DevBoxEndpoint{
				SSH:     "ssh://devbox@gateway:2222",
				Jupyter: "https://jupyter.example.com:7443/x",
				Ports:   map[string]string{"web": "https://web.example.com:7443/"},
			},
			Conditions: []metav1.Condition{{
				Type:               "Ready",
				Status:             metav1.ConditionTrue,
				Reason:             "Running",
				LastTransitionTime: metav1.Now(),
			}},
			Segments: []devpilotv1alpha1.BillingSegment{{
				ID:            1,
				NodeName:      "gn-10-243-145-105",
				PricePerHour:  "2.80",
				StartTime:     metav1.Now(),
				EndTime:       &ended,
				BilledSeconds: 5400,
				Cost:          testCost,
				EndReason:     devpilotv1alpha1.EndReasonUserStopped,
			}},
		},
	}
}

func TestConsoleRequiresAToken(t *testing.T) {
	server, seen := newServer(t)
	recorder := call(server, http.MethodGet, "/api/devboxes", "", false)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if len(*seen) != 0 {
		t.Fatalf("a client was built without a token: %v", *seen)
	}
}

func TestConsoleRejectsANonBearerScheme(t *testing.T) {
	server, seen := newServer(t)
	request := httptest.NewRequest(http.MethodGet, "/api/devboxes", nil)
	request.Header.Set("Authorization", "Basic {{SECRET_9qm27gry}}")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if len(*seen) != 0 {
		t.Fatalf("a client was built for a non-bearer scheme: %v", *seen)
	}
}

func TestListScopesToTheNamespaceAndFlattensTheView(t *testing.T) {
	server, seen := newServer(t, testDevBox(testNamespace, testBox), testDevBox("other", "cpu-box"))

	recorder := call(server, http.MethodGet, "/api/devboxes?namespace=devpilot-verify", "", true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(*seen) != 1 || (*seen)[0] != testToken {
		t.Fatalf("tokens seen = %v, want [%s]", *seen, testToken)
	}

	var body struct {
		Items []devBoxView `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(body.Items))
	}
	view := body.Items[0]
	if view.Name != testBox || view.DisplayName != "PyTorch 开发机" {
		t.Fatalf("view = %+v", view)
	}
	if view.Phase != "Running" || view.GPUs != 1 || view.GPUModel != "RTX 5090" {
		t.Fatalf("power view = %+v", view)
	}
	for _, key := range []string{"ssh", "jupyter", "port/web"} {
		if view.Endpoints[key] == "" {
			t.Fatalf("endpoints = %v, missing %s", view.Endpoints, key)
		}
	}
	if view.TotalCost != "4.20" || view.BilledSeconds != 5400 {
		t.Fatalf("billing = %s / %d", view.TotalCost, view.BilledSeconds)
	}
	if len(view.Segments) != 1 || view.Segments[0].Running || view.Segments[0].EndReason != "UserStopped" {
		t.Fatalf("segments = %+v", view.Segments)
	}
}

func TestListAcrossNamespacesWhenNoneIsGiven(t *testing.T) {
	server, _ := newServer(t, testDevBox(testNamespace, testBox), testDevBox("other", "cpu-box"))
	recorder := call(server, http.MethodGet, "/api/devboxes", "", true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Items []devBoxView `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(body.Items))
	}
}

func TestGetReturnsOneDevBox(t *testing.T) {
	server, _ := newServer(t, testDevBox(testNamespace, testBox))
	recorder := call(server, http.MethodGet, "/api/devboxes/devpilot-verify/gpu-box", "", true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var view devBoxView
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Name != testBox || view.RootDiskSize != "200Gi" {
		t.Fatalf("view = %+v", view)
	}
}

func TestGetReportsAMissingDevBoxAsNotFound(t *testing.T) {
	server, _ := newServer(t)
	recorder := call(server, http.MethodGet, "/api/devboxes/devpilot-verify/missing", "", true)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", recorder.Code, recorder.Body.String())
	}
}

func TestPowerPatchesTheDesiredState(t *testing.T) {
	server, _ := newServer(t, testDevBox(testNamespace, testBox))
	recorder := call(server, http.MethodPost, "/api/devboxes/devpilot-verify/gpu-box/state",
		`{"desiredState":"Stopped"}`, true)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var view devBoxView
	if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.DesiredState != "Stopped" {
		t.Fatalf("desiredState = %q, want Stopped", view.DesiredState)
	}
}

func TestPowerRejectsAnUnknownState(t *testing.T) {
	server, seen := newServer(t, testDevBox(testNamespace, testBox))
	recorder := call(server, http.MethodPost, "/api/devboxes/devpilot-verify/gpu-box/state",
		`{"desiredState":"Hibernating"}`, true)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if len(*seen) != 0 {
		t.Fatalf("a client was built for a refused request: %v", *seen)
	}
}

func TestPowerRejectsMalformedJSON(t *testing.T) {
	server, _ := newServer(t, testDevBox(testNamespace, testBox))
	recorder := call(server, http.MethodPost, "/api/devboxes/devpilot-verify/gpu-box/state", `{`, true)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
}

func TestCommitDefaultsTheTagAndRecordsTheChoices(t *testing.T) {
	server, _ := newServer(t, testDevBox(testNamespace, testBox))
	recorder := call(server, http.MethodPost, "/api/devboxes/devpilot-verify/gpu-box/commits",
		`{"repository":"cr.example.com/library/gpu-box","consistency":"OfflineOnly","compression":"gzip"}`, true)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var created struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	c, err := server.ClientFor(context.Background(), testToken)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	commit := &devpilotv1alpha1.DevBoxCommit{}
	key := client.ObjectKey{Namespace: created.Namespace, Name: created.Name}
	if err := c.Get(context.Background(), key, commit); err != nil {
		t.Fatalf("get commit: %v", err)
	}
	if commit.Spec.DevBoxName != testBox {
		t.Fatalf("devBoxName = %q", commit.Spec.DevBoxName)
	}
	if commit.Spec.Destination.Repository != "cr.example.com/library/gpu-box" {
		t.Fatalf("repository = %q", commit.Spec.Destination.Repository)
	}
	if len(commit.Spec.Destination.Tags) != 1 || commit.Spec.Destination.Tags[0] != "latest" {
		t.Fatalf("tags = %v, want [latest]", commit.Spec.Destination.Tags)
	}
	if commit.Spec.Consistency != devpilotv1alpha1.ConsistencyOfflineOnly {
		t.Fatalf("consistency = %q", commit.Spec.Consistency)
	}
	if commit.Spec.Compression != devpilotv1alpha1.CompressionGzip {
		t.Fatalf("compression = %q", commit.Spec.Compression)
	}
	if !commit.Spec.CleanPackageCache {
		t.Fatal("cleanPackageCache defaulted off")
	}
	if commit.Labels["devpilot.wutz.dev/created-by"] != "dashboard" {
		t.Fatalf("labels = %v", commit.Labels)
	}
}

func TestCommitRequiresARepository(t *testing.T) {
	server, seen := newServer(t, testDevBox(testNamespace, testBox))
	recorder := call(server, http.MethodPost, "/api/devboxes/devpilot-verify/gpu-box/commits",
		`{"repository":"  "}`, true)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if len(*seen) != 0 {
		t.Fatalf("a client was built for a refused request: %v", *seen)
	}
}

func TestCommitRejectsAnUnknownCompression(t *testing.T) {
	server, _ := newServer(t, testDevBox(testNamespace, testBox))
	recorder := call(server, http.MethodPost, "/api/devboxes/devpilot-verify/gpu-box/commits",
		`{"repository":"cr.example.com/library/gpu-box","compression":"brotli"}`, true)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "zstd or gzip") {
		t.Fatalf("body = %s", body)
	}
}

func TestStaticConsoleIsServed(t *testing.T) {
	server, _ := newServer(t)
	recorder := call(server, http.MethodGet, "/", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "DevPilot 控制台") {
		t.Fatal("index does not look like the console")
	}
}

func TestHealthzNeedsNoToken(t *testing.T) {
	server, _ := newServer(t)
	recorder := call(server, http.MethodGet, "/healthz", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

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

// Package dashboard serves the optional web console.
//
// Every request carries the caller's own bearer token, and the console acts
// with it: the client is built per request from the manager's REST config with
// the token substituted, so the API server applies the caller's RBAC exactly as
// it would for kubectl. The console therefore holds no authority of its own and
// never becomes a way around the member roles.
package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	devpilotv1alpha1 "github.com/wutz/devpilot/api/v1alpha1"
)

//go:embed assets
var assetTree embed.FS

// maxBodyBytes bounds a request body; every action body is a few hundred bytes.
const maxBodyBytes = 64 << 10

// Server is the console's HTTP API and static file handler.
type Server struct {
	// Scheme knows the DevPilot types.
	Scheme *runtime.Scheme
	// BaseConfig is the manager's REST config; the bearer token is replaced
	// per request so the call runs as the caller.
	BaseConfig *rest.Config
	// ClientFor builds the caller's client. It is injectable so tests can use
	// a fake client without an API server.
	ClientFor func(ctx context.Context, token string) (client.Client, error)
	// Log records failures.
	Log logr.Logger
	// Handler serves /api and the embedded console.
	Handler http.Handler
}

// New builds the console handler tree.
func New(scheme *runtime.Scheme, base *rest.Config, log logr.Logger) *Server {
	s := &Server{Scheme: scheme, BaseConfig: base, Log: log}
	s.ClientFor = s.restClient

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /api/devboxes", s.handleList)
	mux.HandleFunc("GET /api/devboxes/{namespace}/{name}", s.handleGet)
	mux.HandleFunc("POST /api/devboxes/{namespace}/{name}/state", s.handleState)
	mux.HandleFunc("POST /api/devboxes/{namespace}/{name}/commits", s.handleCommit)

	assets, err := fs.Sub(assetTree, "assets")
	if err != nil {
		// The embedded tree is fixed at build time; a failure here is a bug.
		panic(fmt.Sprintf("dashboard assets: %v", err))
	}
	mux.Handle("GET /", http.FileServerFS(assets))
	s.Handler = mux
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler.ServeHTTP(w, r)
}

// restClient builds a client that acts as the token's holder.
func (s *Server) restClient(_ context.Context, token string) (client.Client, error) {
	cfg := rest.CopyConfig(s.BaseConfig)
	cfg.BearerToken = token
	cfg.BearerTokenFile = ""
	cfg.Username, cfg.Password = "", ""
	cfg.ExecProvider, cfg.AuthProvider = nil, nil
	return client.New(cfg, client.Options{Scheme: s.Scheme})
}

// caller builds the per-request client or writes the refusal itself.
func (s *Server) caller(w http.ResponseWriter, r *http.Request) (client.Client, bool) {
	token, ok := bearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "an Authorization: Bearer token is required")
		return nil, false
	}
	c, err := s.ClientFor(r.Context(), token)
	if err != nil {
		s.Log.Error(err, "Could not build the caller client")
		writeError(w, http.StatusInternalServerError, "could not build a client for the caller")
		return nil, false
	}
	return c, true
}

// handleList returns the devboxes in one namespace, or in every namespace the
// caller can read when the namespace parameter is empty.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	c, ok := s.caller(w, r)
	if !ok {
		return
	}
	list := &devpilotv1alpha1.DevBoxList{}
	options := []client.ListOption{}
	if namespace := strings.TrimSpace(r.URL.Query().Get("namespace")); namespace != "" {
		options = append(options, client.InNamespace(namespace))
	}
	if err := c.List(r.Context(), list, options...); err != nil {
		s.fail(w, err, "Could not list devboxes")
		return
	}
	views := make([]devBoxView, 0, len(list.Items))
	for i := range list.Items {
		views = append(views, newDevBoxView(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": views})
}

// handleGet returns one devbox with its billing ledger.
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	c, ok := s.caller(w, r)
	if !ok {
		return
	}
	box := &devpilotv1alpha1.DevBox{}
	key := client.ObjectKey{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
	if err := c.Get(r.Context(), key, box); err != nil {
		s.fail(w, err, "Could not read the devbox")
		return
	}
	writeJSON(w, http.StatusOK, newDevBoxView(box))
}

// stateRequest is the body of the power action.
type stateRequest struct {
	DesiredState string `json:"desiredState"`
}

// handleState turns a devbox on or off by moving spec.desiredState.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeBody[stateRequest](w, r)
	if !ok {
		return
	}
	state := devpilotv1alpha1.DesiredState(body.DesiredState)
	if state != devpilotv1alpha1.DesiredStateRunning && state != devpilotv1alpha1.DesiredStateStopped {
		writeError(w, http.StatusBadRequest, `desiredState must be "Running" or "Stopped"`)
		return
	}
	c, ok := s.caller(w, r)
	if !ok {
		return
	}
	box := &devpilotv1alpha1.DevBox{}
	key := client.ObjectKey{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
	if err := c.Get(r.Context(), key, box); err != nil {
		s.fail(w, err, "Could not read the devbox")
		return
	}
	before := box.DeepCopy()
	box.Spec.DesiredState = state
	if err := c.Patch(r.Context(), box, client.MergeFrom(before)); err != nil {
		s.fail(w, err, "Could not change the power state")
		return
	}
	writeJSON(w, http.StatusOK, newDevBoxView(box))
}

// commitRequest is the body of the save-image action.
type commitRequest struct {
	Repository        string   `json:"repository"`
	Tags              []string `json:"tags"`
	Consistency       string   `json:"consistency"`
	Compression       string   `json:"compression"`
	CleanPackageCache *bool    `json:"cleanPackageCache"`
	RebaseDevBox      bool     `json:"rebaseDevBox"`
}

// handleCommit creates a DevBoxCommit for the devbox.
func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeBody[commitRequest](w, r)
	if !ok {
		return
	}
	repository := strings.TrimSpace(body.Repository)
	if repository == "" {
		writeError(w, http.StatusBadRequest, "repository is required")
		return
	}
	tags := make([]string, 0, len(body.Tags))
	for _, tag := range body.Tags {
		if trimmed := strings.TrimSpace(tag); trimmed != "" {
			tags = append(tags, trimmed)
		}
	}
	if len(tags) == 0 {
		tags = []string{"latest"}
	}
	if err := validateCommitChoices(body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	c, ok := s.caller(w, r)
	if !ok {
		return
	}
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	box := &devpilotv1alpha1.DevBox{}
	if err := c.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, box); err != nil {
		s.fail(w, err, "Could not read the devbox")
		return
	}

	commit := &devpilotv1alpha1.DevBoxCommit{
		ObjectMeta: metav1.ObjectMeta{
			// The name carries the second it was requested in: two clicks in
			// the same second collide, and that collision is reported as a
			// conflict rather than silently replacing a build.
			Name:      fmt.Sprintf("%s-%d", name, time.Now().Unix()),
			Namespace: namespace,
			Labels:    map[string]string{"devpilot.wutz.dev/created-by": "dashboard"},
		},
		Spec: devpilotv1alpha1.DevBoxCommitSpec{
			DevBoxName:        name,
			Destination:       devpilotv1alpha1.CommitDestination{Repository: repository, Tags: tags},
			Consistency:       devpilotv1alpha1.CommitConsistency(body.Consistency),
			Compression:       devpilotv1alpha1.CommitCompression(body.Compression),
			CleanPackageCache: body.CleanPackageCache == nil || *body.CleanPackageCache,
			RebaseDevBox:      body.RebaseDevBox,
		},
	}
	if err := c.Create(r.Context(), commit); err != nil {
		s.fail(w, err, "Could not create the commit")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"name":      commit.Name,
		"namespace": commit.Namespace,
		"phase":     commit.Status.Phase,
	})
}

// validateCommitChoices rejects values the CRD would refuse, so the console can
// explain them instead of surfacing a raw validation error.
func validateCommitChoices(body commitRequest) error {
	switch devpilotv1alpha1.CommitConsistency(body.Consistency) {
	case "", devpilotv1alpha1.ConsistencySync,
		devpilotv1alpha1.ConsistencyFreeze, devpilotv1alpha1.ConsistencyOfflineOnly:
	default:
		return fmt.Errorf("consistency must be one of Sync, Freeze or OfflineOnly, got %q", body.Consistency)
	}
	switch devpilotv1alpha1.CommitCompression(body.Compression) {
	case "", devpilotv1alpha1.CompressionZstd, devpilotv1alpha1.CompressionGzip:
	default:
		return fmt.Errorf("compression must be zstd or gzip, got %q", body.Compression)
	}
	return nil
}

// decodeBody reads a JSON body, writing the refusal when it is unusable.
func decodeBody[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var body T
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read the request body")
		return body, false
	}
	if len(raw) == 0 {
		return body, true
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeError(w, http.StatusBadRequest, "request body is not valid JSON")
		return body, false
	}
	return body, true
}

// fail maps a client error onto a status code.
func (s *Server) fail(w http.ResponseWriter, err error, msg string) {
	status := http.StatusInternalServerError
	var apiErr interface{ Status() metav1.Status }
	if errors.As(err, &apiErr) {
		switch code := apiErr.Status().Code; {
		case code == http.StatusNotFound || code == http.StatusForbidden || code == http.StatusConflict:
			status = int(code)
		case code >= 400 && code < 500:
			status = int(code)
		}
	}
	if status == http.StatusInternalServerError {
		s.Log.Error(err, msg)
	}
	writeError(w, status, err.Error())
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// bearerToken extracts an Authorization: Bearer token.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// newDevBoxView flattens a devbox into the shape the console renders.
func newDevBoxView(box *devpilotv1alpha1.DevBox) devBoxView {
	view := devBoxView{
		Namespace:     box.Namespace,
		Name:          box.Name,
		DisplayName:   box.Spec.DisplayName,
		Phase:         string(box.Status.Phase),
		DesiredState:  string(box.Spec.DesiredState),
		Message:       box.Status.Message,
		Image:         box.Spec.Image,
		ActiveImage:   box.Status.ActiveImage,
		GPUs:          box.Spec.Resources.GPUs,
		GPUModel:      box.Spec.Resources.GPUModel,
		CPU:           box.Spec.Resources.CPU.String(),
		Memory:        box.Spec.Resources.Memory.String(),
		RootDiskSize:  box.Spec.RootDisk.Size.String(),
		NodeName:      box.Status.NodeName,
		PricePerHour:  box.Spec.PricePerHour,
		BilledSeconds: box.Status.TotalBilledSeconds,
		TotalCost:     box.Status.TotalCost,
		Endpoints:     endpointsView(box.Status.Endpoints),
		StoppedAt:     rfc3339(box.Status.StoppedAt),
		ReleaseAt:     rfc3339(box.Status.ScheduledReleaseTime),
		Conditions:    conditionsView(box.Status.Conditions),
		Segments:      segmentsView(box.Status.Segments),
	}
	return view
}

// endpointsView lists the reachable URLs, skipping the ones not published.
func endpointsView(endpoints devpilotv1alpha1.DevBoxEndpoint) map[string]string {
	view := map[string]string{}
	if endpoints.SSH != "" {
		view["ssh"] = endpoints.SSH
	}
	if endpoints.Jupyter != "" {
		view["jupyter"] = endpoints.Jupyter
	}
	for name, url := range endpoints.Ports {
		view["port/"+name] = url
	}
	return view
}

// conditionsView flattens the status conditions.
func conditionsView(conditions []metav1.Condition) []conditionView {
	views := make([]conditionView, 0, len(conditions))
	for _, condition := range conditions {
		views = append(views, conditionView{
			Type:    condition.Type,
			Status:  string(condition.Status),
			Reason:  condition.Reason,
			Message: condition.Message,
			Since:   condition.LastTransitionTime.Time.UTC().Format(time.RFC3339),
		})
	}
	return views
}

// segmentsView flattens the billing ledger.
func segmentsView(segments []devpilotv1alpha1.BillingSegment) []segmentView {
	views := make([]segmentView, 0, len(segments))
	for _, segment := range segments {
		views = append(views, segmentView{
			ID:            segment.ID,
			NodeName:      segment.NodeName,
			PricePerHour:  segment.PricePerHour,
			StartTime:     segment.StartTime.Time.UTC().Format(time.RFC3339),
			EndTime:       rfc3339(segment.EndTime),
			BilledSeconds: segment.BilledSeconds,
			Cost:          segment.Cost,
			EndReason:     string(segment.EndReason),
			Running:       segment.EndTime == nil,
		})
	}
	return views
}

// rfc3339 renders an optional timestamp.
func rfc3339(t *metav1.Time) string {
	if t == nil {
		return ""
	}
	return t.Time.UTC().Format(time.RFC3339)
}

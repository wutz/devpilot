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

package loopcsi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// IdentityServer implements the CSI identity service.
type IdentityServer struct {
	csi.UnimplementedIdentityServer
	Version string
}

// GetPluginInfo returns the driver name.
func (i *IdentityServer) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: DriverName, VendorVersion: i.Version}, nil
}

// GetPluginCapabilities advertises the controller service and online
// expansion.
func (i *IdentityServer) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{Capabilities: []*csi.PluginCapability{
		{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_CONTROLLER_SERVICE}}},
		{Type: &csi.PluginCapability_VolumeExpansion_{VolumeExpansion: &csi.PluginCapability_VolumeExpansion{Type: csi.PluginCapability_VolumeExpansion_ONLINE}}},
	}}, nil
}

// Probe reports readiness.
func (i *IdentityServer) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}

// Serve listens on a unix:// endpoint and serves the given services until
// ctx is cancelled. The logger is read from ctx via logr.FromContextOrDiscard.
func Serve(ctx context.Context, endpoint string, identity csi.IdentityServer,
	controller csi.ControllerServer, node csi.NodeServer) error {
	log := logr.FromContextOrDiscard(ctx)
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "unix" {
		return fmt.Errorf("endpoint %q must be unix:///path", endpoint)
	}
	path := u.Path
	if path == "" {
		path = u.Host
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	srv := grpc.NewServer(grpc.UnaryInterceptor(logInterceptor(log)))
	csi.RegisterIdentityServer(srv, identity)
	if controller != nil {
		csi.RegisterControllerServer(srv, controller)
	}
	if node != nil {
		csi.RegisterNodeServer(srv, node)
	}
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	log.Info("Serving CSI", "endpoint", endpoint)
	return srv.Serve(lis)
}

func logInterceptor(log logr.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		l := log.WithValues("method", info.FullMethod, "duration", time.Since(start).Round(time.Millisecond).String())
		switch {
		case err != nil:
			l.Error(err, "CSI call failed")
		case isQuiet(info.FullMethod):
			l.V(4).Info("CSI call")
		default:
			l.V(2).Info("CSI call")
		}
		return resp, err
	}
}

func isQuiet(method string) bool {
	switch method {
	case "/csi.v1.Identity/Probe", "/csi.v1.Node/NodeGetCapabilities", "/csi.v1.Node/NodeGetVolumeStats",
		"/csi.v1.Identity/GetPluginInfo", "/csi.v1.Identity/GetPluginCapabilities":
		return true
	}
	return false
}

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

// devpilot-loop-csi is the CSI driver for DevBox root disks stored as ext4
// images on a shared filesystem. One binary serves both roles:
//
//	--mode=controller  next to csi-provisioner, csi-resizer, csi-snapshotter
//	--mode=node        in a privileged DaemonSet next to node-driver-registrar
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/wutz/devpilot/internal/loopcsi"
)

// version is set with -ldflags "-X main.version=...".
var version = "dev"

type poolFlags []loopcsi.Pool

func (p *poolFlags) String() string {
	parts := make([]string, 0, len(*p))
	for _, x := range *p {
		parts = append(parts, x.Name+"="+x.Path)
	}
	return strings.Join(parts, ",")
}

func (p *poolFlags) Set(v string) error {
	pool, err := loopcsi.ParsePool(v)
	if err != nil {
		return err
	}
	*p = append(*p, pool)
	return nil
}

func main() {
	var (
		endpoint string
		mode     string
		nodeID   string
		stateDir string
		pools    poolFlags
		node     loopcsi.NodeServer
	)
	flag.StringVar(&endpoint, "endpoint", "unix:///csi/csi.sock", "CSI endpoint.")
	flag.StringVar(&mode, "mode", "", "controller or node.")
	flag.StringVar(&nodeID, "node-id", os.Getenv("NODE_NAME"), "Node name (node mode).")
	flag.StringVar(&stateDir, "state-dir", "/var/lib/kubelet/plugins/"+loopcsi.DriverName,
		"Node-local state and private mounts; must be under the kubelet root with Bidirectional propagation (node mode).")
	flag.Var(&pools, "pool",
		"Shared-filesystem pool as name=/path, repeatable. The path must match across nodes and controller.")
	flag.DurationVar(&node.LeaseInterval, "lease-interval", loopcsi.DefaultLeaseInterval,
		"How often a node renews the lease of a read-write volume.")
	flag.DurationVar(&node.LeaseTTL, "lease-ttl", loopcsi.DefaultLeaseTTL,
		"How long a lease without renewal blocks other nodes.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	log := zap.New(zap.UseFlagOptions(&opts)).WithValues("mode", mode)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := run(logr.NewContext(ctx, log), mode, endpoint, nodeID, stateDir, pools, &node); err != nil {
		log.Error(err, "Exiting")
		os.Exit(1)
	}
}

func run(
	ctx context.Context,
	mode, endpoint, nodeID, stateDir string,
	list poolFlags,
	node *loopcsi.NodeServer,
) error {
	log := logr.FromContextOrDiscard(ctx)
	if len(list) == 0 {
		return fmt.Errorf("at least one --pool is required")
	}
	pools, err := loopcsi.NewPools(list)
	if err != nil {
		return err
	}
	for _, p := range pools {
		if st, err := os.Stat(p.Path); err != nil || !st.IsDir() {
			return fmt.Errorf("pool %s path %s is not a directory: %v", p.Name, p.Path, err)
		}
	}
	identity := &loopcsi.IdentityServer{Version: version}
	switch mode {
	case "controller":
		ctrl := &loopcsi.ControllerServer{Pools: pools, LeaseTTL: node.LeaseTTL, Log: log.WithName("controller")}
		return loopcsi.Serve(ctx, endpoint, identity, ctrl, nil)
	case "node":
		if nodeID == "" {
			return fmt.Errorf("--node-id (or NODE_NAME) is required in node mode")
		}
		host, err := loopcsi.NewHost() //nolint:staticcheck // non-Linux host stub always returns an error
		if err != nil {                //nolint:staticcheck // non-Linux host stub always returns an error
			return err
		}
		node.NodeID = nodeID
		node.Pools = pools
		node.StateDir = stateDir
		node.Host = host
		node.Log = log.WithName("node")
		if err := node.Start(ctx); err != nil {
			return fmt.Errorf("recover node state: %w", err)
		}
		return loopcsi.Serve(ctx, endpoint, identity, nil, node)
	}
	return fmt.Errorf("--mode must be controller or node, not %q", mode)
}

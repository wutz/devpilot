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
	"errors"
	"net/http"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// shutdownTimeout bounds the wait for in-flight requests on shutdown.
const shutdownTimeout = 10 * time.Second

// consoleRunnable runs the console on every replica. A plain RunnableFunc
// would be leader-elected, so only the leader would listen and a Service in
// front of several replicas would refuse connections to the rest. The console
// holds no state and acts as its caller, so any replica can serve it.
type consoleRunnable struct {
	manager.Runnable
}

// NeedLeaderElection opts the console out of leader election.
func (consoleRunnable) NeedLeaderElection() bool {
	return false
}

// NewRunnable binds the console to address for as long as the manager runs.
//
// It is added to the manager rather than started beside it so that a bind
// failure takes the manager down and the listener stops with it.
func NewRunnable(server *Server, address string) manager.Runnable {
	return consoleRunnable{Runnable: manager.RunnableFunc(func(ctx context.Context) error {
		httpServer := &http.Server{
			Addr:              address,
			Handler:           server,
			ReadHeaderTimeout: 10 * time.Second,
		}

		done := make(chan error, 1)
		go func() {
			// A closed listener during shutdown is the expected exit, not a
			// failure worth logging.
			if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				done <- err
				return
			}
			done <- nil
		}()

		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			server.Log.Info("Stopping the web console")
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
			defer cancel()
			return httpServer.Shutdown(shutdownCtx)
		}
	})}
}

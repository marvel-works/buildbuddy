package healthcheck

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"golang.org/x/sync/errgroup"
)

var (
	maxShutdownDuration = flag.Duration("max_shutdown_duration", 25*time.Second, "Time to wait for shutdown")
)

const (
	healthCheckPeriod  = 3 * time.Second // The time to wait between health checks.
	healthCheckTimeout = 2 * time.Second // How long a health check may take, max.
)

type Checker interface {
	Check(ctx context.Context) error
}
type CheckerFunc func(ctx context.Context) error

func (f CheckerFunc) Check(ctx context.Context) error {
	return f(ctx)
}

type ShutDownFunc func(ctx context.Context) error

type HealthChecker struct {
	serverType    string
	done          chan bool
	quit          chan os.Signal
	shutdownFuncs []ShutDownFunc

	lock         sync.RWMutex // protects: readyToServe, shuttingDown
	readyToServe bool
	shuttingDown bool
	checkers     map[string]Checker
}

func NewHealthChecker(serverType string) *HealthChecker {
	hc := HealthChecker{
		serverType:    serverType,
		done:          make(chan bool),
		quit:          make(chan os.Signal, 1),
		shutdownFuncs: make([]ShutDownFunc, 0),
		readyToServe:  true,
		checkers:      make(map[string]Checker, 0),
	}
	signal.Notify(hc.quit, os.Interrupt, syscall.SIGTERM)
	go hc.handleShutdownFuncs()
	go func() {
		for {
			hc.runHealthChecks(context.Background())
			time.Sleep(healthCheckPeriod)
		}
	}()
	return &hc
}

func (h *HealthChecker) handleShutdownFuncs() {
	<-h.quit

	h.lock.Lock()
	h.readyToServe = false
	h.shuttingDown = true
	h.lock.Unlock()

	// We use fmt here and below because this code is called from the
	// signal handler and log.Printf can be a little wonky.
	fmt.Printf("Caught interrupt signal; shutting down...\n")
	ctx, cancel := context.WithTimeout(context.Background(), *maxShutdownDuration)
	defer cancel()

	eg, egCtx := errgroup.WithContext(ctx)
	for _, fn := range h.shutdownFuncs {
		f := fn
		eg.Go(func() error {
			if err := f(egCtx); err != nil {
				fmt.Printf("Error gracefully shutting down: %s\n", err)
			}
			return nil
		})
	}
	eg.Wait()
	if err := ctx.Err(); err != nil {
		fmt.Printf("MaxShutdownDuration exceeded. Non-graceful exit.\n")
	}
	time.Sleep(10 * time.Millisecond)
	fmt.Printf("Server %q stopped.\n", h.serverType)
	close(h.done)
}

func (h *HealthChecker) RegisterShutdownFunction(f ShutDownFunc) {
	h.shutdownFuncs = append(h.shutdownFuncs, f)
}

func (h *HealthChecker) AddHealthCheck(name string, f Checker) {
	// Mark the service as unhealthy until the healthcheck runs
	// and it becomes healthy.
	h.lock.Lock()
	h.checkers[name] = f
	h.readyToServe = false
	h.lock.Unlock()
}

func (h *HealthChecker) WaitForGracefulShutdown() {
	<-h.done
}

func (h *HealthChecker) runHealthChecks(ctx context.Context) {
	h.lock.RLock()
	bail := h.shuttingDown
	h.lock.RUnlock()
	if bail {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	eg, ctx := errgroup.WithContext(ctx)
	for name, ck := range h.checkers {
		eg.Go(func() error {
			if err := ck.Check(ctx); err != nil {
				return status.UnavailableErrorf("Service %s is unhealthy: %s", name, err)
			}
			return nil
		})
	}
	err := eg.Wait()
	newReadinessState := true
	if err != nil {
		newReadinessState = false
		log.Printf("Checker err: %s", err)
	}

	previousReadinessState := false
	h.lock.Lock()
	if !h.shuttingDown {
		previousReadinessState = h.readyToServe
		h.readyToServe = newReadinessState
	}
	h.lock.Unlock()

	if newReadinessState != previousReadinessState {
		log.Printf("HealthChecker transitioning from ready: %t => ready: %t", previousReadinessState, newReadinessState)
	}
}

func (h *HealthChecker) ReadinessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqServerType := serverType(r)
		if reqServerType == h.serverType {
			h.lock.RLock()
			ready := h.readyToServe
			h.lock.RUnlock()

			if ready {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("OK"))
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return
		}
		err := fmt.Errorf("Server type: '%s' unknown (did not match: %q)", reqServerType, h.serverType)
		log.Printf("Readiness check returning error: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	})
}

func (h *HealthChecker) LivenessHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqServerType := serverType(r)
		if reqServerType == h.serverType {
			w.Write([]byte("OK"))
			return
		}
		err := fmt.Errorf("Server type: '%s' unknown (did not match: %q)", reqServerType, h.serverType)
		log.Printf("Liveness check returning error: %s", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	})
}

// serverType is dervied from either the headers or a query parameter
func serverType(r *http.Request) string {
	if r.Header.Get("server-type") != "" {
		return r.Header.Get("server-type")
	}
	// GCP load balancer healthchecks do not allow sending headers.
	return r.URL.Query().Get("server-type")
}

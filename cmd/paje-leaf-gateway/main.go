// Command paje-leaf-gateway exposes only scoped code-change@v1 leaf operations.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/araihu/paje/internal/leafgatewayconfig"
	"github.com/araihu/paje/internal/processguard"
	"github.com/araihu/paje/internal/submission"
	"github.com/araihu/paje/internal/submission/auth"
	"github.com/araihu/paje/internal/submission/filesystem"
	submissionhatchet "github.com/araihu/paje/internal/submission/hatchet"
	"github.com/araihu/paje/internal/submission/httpapi"
	"github.com/araihu/paje/internal/template"
	templatecodechange "github.com/araihu/paje/internal/template/codechange"
	hatchetclient "github.com/hatchet-dev/hatchet/pkg/client"
	hatchet "github.com/hatchet-dev/hatchet/sdks/go"
)

type producerFactory func(string) (submissionhatchet.Client, func(context.Context) error, error)

type requestTracker struct {
	handler http.Handler

	mu       sync.Mutex
	active   int
	stopping bool
	drained  chan struct{}
}

func newRequestTracker(handler http.Handler) *requestTracker {
	return &requestTracker{handler: handler, drained: make(chan struct{})}
}

func (tracker *requestTracker) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	tracker.mu.Lock()
	if tracker.stopping {
		tracker.mu.Unlock()
		http.Error(writer, "service is shutting down", http.StatusServiceUnavailable)
		return
	}
	tracker.active++
	tracker.mu.Unlock()
	defer func() {
		tracker.mu.Lock()
		tracker.active--
		if tracker.stopping && tracker.active == 0 {
			close(tracker.drained)
		}
		tracker.mu.Unlock()
	}()
	tracker.handler.ServeHTTP(writer, request)
}

func (tracker *requestTracker) stopAccepting() {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.stopping {
		return
	}
	tracker.stopping = true
	if tracker.active == 0 {
		close(tracker.drained)
	}
}

func (tracker *requestTracker) wait(ctx context.Context) error {
	select {
	case <-tracker.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv, processguard.Harden, newProducer); err != nil {
		log.Printf("paje leaf gateway: %v", err)
		os.Exit(1)
	}
}

func run(
	ctx context.Context,
	getenv func(string) string,
	harden func() error,
	newClient producerFactory,
) error {
	if harden == nil {
		return errors.New("start leaf gateway: process hardener is required")
	}
	if err := harden(); err != nil {
		return fmt.Errorf("start leaf gateway: %w", err)
	}
	cfg, err := leafgatewayconfig.Load(getenv)
	if err != nil {
		return err
	}
	if newClient == nil {
		newClient = newProducer
	}
	producer, closeProducer, err := newClient(cfg.HatchetProducerToken)
	if err != nil {
		return fmt.Errorf("start leaf gateway: create Hatchet producer: %w", err)
	}
	closeWithTimeout := func() error {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer cancel()
		return closeProducer(closeCtx)
	}
	handler, err := buildHandler(cfg, producer)
	if err != nil {
		_ = closeWithTimeout()
		return err
	}
	trackedHandler := newRequestTracker(handler)
	server := &http.Server{
		Addr: cfg.ListenAddress, Handler: trackedHandler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout, ReadTimeout: cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout, IdleTimeout: cfg.IdleTimeout,
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.ListenAndServe() }()

	select {
	case serveErr := <-serveResult:
		trackedHandler.stopAccepting()
		terminateErr := server.Close()
		drainCtx, drainCancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		drainErr := trackedHandler.wait(drainCtx)
		drainCancel()
		if drainErr != nil {
			return errors.Join(serveErr, terminateErr, fmt.Errorf("drain leaf gateway requests: %w", drainErr))
		}
		closeErr := closeWithTimeout()
		if errors.Is(serveErr, http.ErrServerClosed) {
			return errors.Join(terminateErr, closeErr)
		}
		return errors.Join(fmt.Errorf("serve leaf gateway: %w", serveErr), terminateErr, closeErr)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		shutdownErr := server.Shutdown(shutdownCtx)
		cancel()
		trackedHandler.stopAccepting()
		var terminateErr error
		if shutdownErr != nil {
			terminateErr = server.Close()
		}
		serveErr := <-serveResult
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		drainCtx, drainCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		drainErr := trackedHandler.wait(drainCtx)
		drainCancel()
		if drainErr != nil {
			return errors.Join(shutdownErr, terminateErr, serveErr, fmt.Errorf("drain leaf gateway requests: %w", drainErr))
		}
		return errors.Join(shutdownErr, terminateErr, serveErr, closeWithTimeout())
	}
}

func buildHandler(cfg leafgatewayconfig.Config, producer submissionhatchet.Client) (http.Handler, error) {
	authenticator, err := auth.LoadPolicy(cfg.TokenPolicyFile, time.Now)
	if err != nil {
		return nil, fmt.Errorf("build leaf gateway: %w", err)
	}
	store, err := filesystem.New(cfg.SubmissionRoot)
	if err != nil {
		return nil, fmt.Errorf("build leaf gateway: %w", err)
	}
	trigger, err := submissionhatchet.New(producer)
	if err != nil {
		return nil, fmt.Errorf("build leaf gateway: %w", err)
	}
	templates, err := template.NewRegistry(templatecodechange.Definition{})
	if err != nil {
		return nil, fmt.Errorf("build leaf gateway: %w", err)
	}
	service, err := submission.New(submission.Dependencies{
		Templates: templates, Store: store, Trigger: trigger, Clock: time.Now, SystemMaxDepth: 1,
	})
	if err != nil {
		return nil, fmt.Errorf("build leaf gateway: %w", err)
	}
	handler, err := httpapi.New(httpapi.Dependencies{
		Service: service, Authenticator: authenticator,
		Ready: func(context.Context) error { return nil },
	})
	if err != nil {
		return nil, fmt.Errorf("build leaf gateway: %w", err)
	}
	return handler, nil
}

func newProducer(token string) (submissionhatchet.Client, func(context.Context) error, error) {
	client, err := hatchet.NewClient(hatchetclient.WithToken(token))
	if err != nil {
		return nil, nil, err
	}
	adapter, err := submissionhatchet.NewSDKClient(client)
	if err != nil {
		_ = client.Close(context.Background())
		return nil, nil, err
	}
	return adapter, client.Close, nil
}

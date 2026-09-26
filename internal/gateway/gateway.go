package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/florianilch/lightpanda-gateway/internal/browser"
)

var (
	// ErrQueueFull means the Gateway queue has no room for a Gateway Begin call.
	ErrQueueFull = errors.New("queue full")
	// ErrQueueTimeout means a Gateway Begin call exceeded its queue timeout.
	ErrQueueTimeout = errors.New("queue timeout")
	// ErrDraining means Gateway shutdown has started and admission has stopped.
	ErrDraining = errors.New("gateway is shutting down")
	// ErrResourceTimeout means a CDPBrowser or ScriptCall exceeded its operation lifetime.
	ErrResourceTimeout = errors.New("resource timeout")
	// ErrShutdown is the cause Gateway passes when it asks a resource to stop.
	ErrShutdown = errors.New("gateway shutting down")
)

// Config contains Gateway settings.
type Config struct {
	// MaxResources is the maximum number of Gateway resources that may be in use.
	MaxResources int
	// MaxQueueLength is the maximum number of Gateway Begin calls that may wait in
	// the Gateway queue. Zero disables queueing. A call that must wait returns
	// ErrQueueFull when the queue is full. Later Gateway Begin calls cannot bypass calls
	// already waiting in the queue, but FIFO is not guaranteed. Negative values are invalid.
	MaxQueueLength int
	// QueueTimeout limits how long a Gateway Begin call may wait in the Gateway queue.
	// Zero disables the timeout. It has no effect when queueing is disabled. Negative
	// values are invalid.
	QueueTimeout time.Duration
	// MaxResourceLifetime is the maximum operation lifetime for a CDPBrowser or
	// ScriptCall. After it expires, Gateway asks the resource to stop. The resource
	// keeps its slot until it finishes. Zero disables the limit. A caller may request
	// a shorter lifetime. Negative values are invalid.
	MaxResourceLifetime time.Duration
	// CDPStartupTimeout limits CDPBrowser.Start, including time spent waiting for
	// other browser startups and backend connection attempts. It ends when the
	// CDPBrowser is ready for Attach. It must be positive.
	CDPStartupTimeout time.Duration
	// CDPIdleTimeout limits inactivity in an established CDP tunnel. The timer starts
	// after a successful WebSocket upgrade. Zero disables it. Negative values are invalid.
	CDPIdleTimeout time.Duration

	// BrowserBinary is the absolute path to the Lightpanda executable.
	BrowserBinary string
	// BrowserLaunchArgs contains optional Lightpanda arguments. Each entry is one
	// command-line argument. Arguments that replace values set by package browser or
	// filter browser logs are rejected.
	BrowserLaunchArgs []string
	// MaxStdoutBytes and MaxStderrBytes limit how much leading output from a script
	// execution is returned. Zero keeps no bytes from that stream. Negative values
	// are invalid.
	MaxStdoutBytes int
	MaxStderrBytes int
}

// gatewayResource lets Gateway stop a resource and learn when it no longer needs its
// Gateway slot.
type gatewayResource interface {
	// requestStop asks the resource to stop without waiting. Gateway may call it more
	// than once.
	requestStop(cause error)
	// Done returns a channel that closes when the resource no longer needs its Gateway
	// slot.
	Done() <-chan struct{}
}

// Gateway admits and tracks Gateway resources.
type Gateway struct {
	launcher *browser.Launcher

	queueTimeout time.Duration

	// Each entry in resourceSlots represents one held Gateway slot. The channel
	// capacity is Config.MaxResources.
	resourceSlots chan struct{}

	// Each entry in queueSlots represents one Gateway Begin call waiting in the
	// Gateway queue. The channel capacity is Config.MaxQueueLength.
	queueSlots chan struct{}

	maxResourceLifetime time.Duration
	cdpStartupTimeout   time.Duration
	cdpIdleTimeout      time.Duration
	maxStdoutBytes      int
	maxStderrBytes      int

	// cdpAddresses coordinates browser startups and backend connection attempts. It
	// allows only one browser startup at a time, blocks startup during backend
	// connection attempts, and records the CDPBrowser for each reported address.
	cdpAddresses *cdpAddressCoordinator

	// cdpTransport supplies the dial and HTTP settings for backend connections
	// opened by CDPBrowser.Attach.
	cdpTransport *http.Transport

	drainOnce sync.Once

	// draining closes when Gateway shutdown starts. Gateway admission then fails.
	draining chan struct{}

	// done closes after every admitted resource has returned its Gateway slot.
	done chan struct{}

	// mu protects resourcesInUse. acquireResourceSlot holds it while checking the
	// queue and reserving a slot, so a call cannot bypass a nonempty queue.
	//
	// admit and beginDrain use it too. Together, they record a resource before draining
	// starts or reject it after draining starts.
	mu sync.Mutex

	// resourceWG tracks the goroutine that waits for each admitted resource and
	// returns its slot. beginDrain waits for those goroutines before closing done.
	resourceWG sync.WaitGroup

	// resourcesInUse maps each admitted resource to its operation-lifetime timer.
	// A nil timer means Gateway set no lifetime limit.
	resourcesInUse map[gatewayResource]*time.Timer
}

// New validates cfg and returns a Gateway.
func New(cfg *Config) (*Gateway, error) {
	if cfg == nil {
		return nil, errors.New("gateway: config is nil")
	}
	if cfg.MaxResources < 1 {
		return nil, errors.New("gateway: MaxResources must be >= 1")
	}
	if cfg.MaxQueueLength < 0 {
		return nil, errors.New("gateway: MaxQueueLength must not be negative")
	}
	if cfg.QueueTimeout < 0 {
		return nil, errors.New("gateway: QueueTimeout must not be negative")
	}
	if cfg.MaxResourceLifetime < 0 {
		return nil, errors.New("gateway: MaxResourceLifetime must not be negative")
	}
	if cfg.CDPStartupTimeout <= 0 {
		return nil, errors.New("gateway: CDPStartupTimeout must be greater than zero")
	}
	if cfg.CDPIdleTimeout < 0 {
		return nil, errors.New("gateway: CDPIdleTimeout must not be negative")
	}
	if cfg.MaxStdoutBytes < 0 || cfg.MaxStderrBytes < 0 {
		return nil, errors.New("gateway: output limits must not be negative")
	}

	launcher, err := browser.NewLauncher(cfg.BrowserBinary, cfg.BrowserLaunchArgs)
	if err != nil {
		return nil, err
	}

	return &Gateway{
		launcher:            launcher,
		queueTimeout:        cfg.QueueTimeout,
		resourceSlots:       make(chan struct{}, cfg.MaxResources),
		queueSlots:          make(chan struct{}, cfg.MaxQueueLength),
		maxResourceLifetime: cfg.MaxResourceLifetime,
		cdpStartupTimeout:   cfg.CDPStartupTimeout,
		cdpIdleTimeout:      cfg.CDPIdleTimeout,
		maxStdoutBytes:      cfg.MaxStdoutBytes,
		maxStderrBytes:      cfg.MaxStderrBytes,
		cdpAddresses:        newCDPAddressCoordinator(),
		cdpTransport:        newCDPBackendTransport(),
		draining:            make(chan struct{}),
		done:                make(chan struct{}),
		resourcesInUse:      make(map[gatewayResource]*time.Timer),
	}, nil
}

// ResourcesInUse returns the number of Gateway slots currently held.
func (g *Gateway) ResourcesInUse() int { return len(g.resourceSlots) }

// MaxResources returns the maximum number of Gateway slots.
func (g *Gateway) MaxResources() int { return cap(g.resourceSlots) }

// QueueLength returns the number of Gateway queue positions currently held.
func (g *Gateway) QueueLength() int { return len(g.queueSlots) }

// Shutdown stops admission and waits for admitted resources to return their
// slots. Use Stop to ask those resources to stop. If ctx ends first, Shutdown
// returns ctx.Err(). Gateway remains draining and keeps waiting.
func (g *Gateway) Shutdown(ctx context.Context) error {
	g.beginDrain()
	// Prefer completed shutdown when done and ctx are both ready.
	select {
	case <-g.done:
		return nil
	default:
	}
	select {
	case <-g.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop begins Gateway shutdown, stops admission, and asks tracked resources to stop.
// It returns without waiting. Use Shutdown to wait for their slots to return.
func (g *Gateway) Stop() {
	g.beginDrain()

	// Copy resources while holding g.mu, then unlock before calling requestStop.
	// requestStop may take resource locks and cancel contexts.
	g.mu.Lock()
	resources := make([]gatewayResource, 0, len(g.resourcesInUse))
	for resource := range g.resourcesInUse {
		resources = append(resources, resource)
	}
	g.mu.Unlock()

	for _, resource := range resources {
		resource.requestStop(ErrShutdown)
	}
}

func (g *Gateway) tryAcquireQueueSlot() bool {
	select {
	case g.queueSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *Gateway) releaseQueueSlot() { <-g.queueSlots }

func (g *Gateway) tryAcquireResourceSlot() bool {
	select {
	case g.resourceSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (g *Gateway) releaseResourceSlot() { <-g.resourceSlots }

// acquireResourceSlot reserves one Gateway slot. A call can take a free slot
// directly only when the queue is empty. Otherwise it reserves a place in the queue
// and waits. It returns ErrQueueFull when no queue position is available.
// On success, the caller must either record a resource or return the slot.
func (g *Gateway) acquireResourceSlot(ctx context.Context) error {
	admissionError := func(deadline time.Time) error {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		if g.isDraining() {
			return ErrDraining
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return ErrQueueTimeout
		}
		return nil
	}

	if err := admissionError(time.Time{}); err != nil {
		return err
	}

	// Hold g.mu while checking the queue and reserving a slot. Otherwise a later call
	// could take a free slot ahead of a call already in the queue. Both slot
	// acquisitions are nonblocking.
	g.mu.Lock()
	if err := admissionError(time.Time{}); err != nil {
		g.mu.Unlock()
		return err
	}
	if len(g.queueSlots) == 0 && g.tryAcquireResourceSlot() {
		g.mu.Unlock()

		// Recheck cancellation and shutdown after taking the slot. If either happened
		// concurrently, return the slot instead of reporting success.
		if err := admissionError(time.Time{}); err != nil {
			g.releaseResourceSlot()
			return err
		}
		return nil
	}
	if !g.tryAcquireQueueSlot() {
		g.mu.Unlock()
		return ErrQueueFull
	}
	var deadline time.Time
	if g.queueTimeout > 0 {
		deadline = time.Now().Add(g.queueTimeout)
	}
	g.mu.Unlock()
	defer g.releaseQueueSlot()

	var timer *time.Timer
	var timerC <-chan time.Time
	if !deadline.IsZero() {
		timer = time.NewTimer(time.Until(deadline))
		timerC = timer.C
		defer timer.Stop()
	}

	// The slot case can win when cancellation, shutdown, or timeout is also ready.
	// Check again and return the slot instead of admitting the call.
	select {
	case g.resourceSlots <- struct{}{}:
		if err := admissionError(deadline); err != nil {
			g.releaseResourceSlot()
			return err
		}
		return nil
	case <-g.draining:
		return ErrDraining
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timerC:
		return ErrQueueTimeout
	}
}

// admit reserves a Gateway slot, then checks ctx and shutdown before recording r. If
// either check fails, it returns the slot. Once r is recorded, canceling ctx does not
// stop it.
//
// Gateway owns the operation-lifetime timer. An admitted resource keeps its Gateway
// slot until r.Done closes. Shutdown waits until all admitted resources release their
// slots, unless its context ends first.
func (g *Gateway) admit(ctx context.Context, r gatewayResource, lifetime time.Duration) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if err := g.acquireResourceSlot(ctx); err != nil {
		return err
	}

	g.mu.Lock()
	if cause := context.Cause(ctx); cause != nil {
		g.mu.Unlock()
		g.releaseResourceSlot()
		return cause
	}
	if g.isDraining() {
		g.mu.Unlock()
		g.releaseResourceSlot()
		return ErrDraining
	}
	// Start the lifetime at admission, not when the timer is created.
	admittedAt := time.Now()
	g.resourcesInUse[r] = nil
	if lifetime > 0 {
		remaining := lifetime - time.Since(admittedAt)
		g.resourcesInUse[r] = time.AfterFunc(remaining, func() {
			r.requestStop(fmt.Errorf(
				"%w: exceeded %s",
				ErrResourceTimeout,
				lifetime,
			))
		})
	}
	// Register while holding g.mu. beginDrain takes the same mutex before waiting. Its
	// wait cannot start until this registration is complete.
	g.resourceWG.Go(func() {
		<-r.Done()
		g.release(r)
	})
	g.mu.Unlock()
	return nil
}

// release removes r, stops its timer, and returns its Gateway slot. Gateway calls it
// after r.Done closes, so it does not return the slot too early.
func (g *Gateway) release(r gatewayResource) {
	g.mu.Lock()
	timer, ok := g.resourcesInUse[r]
	if !ok {
		g.mu.Unlock()
		panic("gateway: release of resource not in use")
	}
	delete(g.resourcesInUse, r)
	g.mu.Unlock()

	if timer != nil {
		// Timer.Stop does not wait for an active callback. A callback racing with release
		// may call requestStop after r.Done closes; requestStop is idempotent.
		timer.Stop()
	}
	g.releaseResourceSlot()
}

// beginDrain stops admission once. It closes done after all admitted resources have
// released their Gateway slots.
func (g *Gateway) beginDrain() {
	g.drainOnce.Do(func() {
		g.mu.Lock()
		close(g.draining)
		g.mu.Unlock()

		go func() {
			g.resourceWG.Wait()
			close(g.done)
		}()
	})
}

func (g *Gateway) isDraining() bool {
	select {
	case <-g.draining:
		return true
	default:
		return false
	}
}

// effectiveResourceLifetime returns the shorter nonzero limit. It returns zero
// only when both limits are zero.
func effectiveResourceLifetime(maximum, requested time.Duration) time.Duration {
	switch {
	case requested == 0:
		return maximum
	case maximum == 0:
		return requested
	default:
		return min(maximum, requested)
	}
}

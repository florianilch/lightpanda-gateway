package gateway

import (
	"context"
	"errors"
	"math"
	"sync"

	"golang.org/x/sync/semaphore"
)

// cdpAddressGateWeight is the number of permits in the address gate. A backend
// connection attempt takes one permit. Browser startup takes all permits, so it
// waits for existing attempts and prevents new ones from starting.
const cdpAddressGateWeight int64 = math.MaxInt64

// cdpAddressCoordinator coordinates browser startups and backend connection attempts.
// Connection attempts run concurrently, but each browser startup has exclusive access
// to the address gate. It records the CDPBrowser that owns each reported CDP address.
type cdpAddressCoordinator struct {
	gate *semaphore.Weighted

	// mu protects the owners map.
	mu sync.Mutex
	// owners maps each reported CDP address to its current CDPBrowser.
	owners map[string]*CDPBrowser
}

func newCDPAddressCoordinator() *cdpAddressCoordinator {
	return &cdpAddressCoordinator{
		gate:   semaphore.NewWeighted(cdpAddressGateWeight),
		owners: make(map[string]*CDPBrowser),
	}
}

// acquireStartup waits for exclusive access to the address gate. The gate stays held
// until Start publishes endpoint ownership or monitorProcess observes process exit. If
// process start fails first, Start releases it. The release is idempotent because Start
// and monitorProcess may race.
func (c *cdpAddressCoordinator) acquireStartup(ctx context.Context) (func(), error) {
	if err := c.gate.Acquire(ctx, cdpAddressGateWeight); err != nil {
		return nil, context.Cause(ctx)
	}
	if cause := context.Cause(ctx); cause != nil {
		c.gate.Release(cdpAddressGateWeight)
		return nil, cause
	}
	return sync.OnceFunc(func() { c.gate.Release(cdpAddressGateWeight) }), nil
}

// register publishes browser as the owner of addr. The caller must hold the address
// gate exclusively with acquireStartup.
//
// The OS can reuse a browser's endpoint before its old ownership record is removed.
// The gate keeps backend connection attempts waiting until the new browser owns addr,
// preventing them from dialing the wrong browser.
func (c *cdpAddressCoordinator) register(browser *CDPBrowser, addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// An exited browser process can still have a map entry when the OS reuses its
	// address. Replace that entry. unregister deletes an entry only if it still points
	// to the browser being stopped.
	c.owners[addr] = browser
}

// unregister removes addr only if browser is still its owner. The owner check prevents
// an older browser from removing a newer browser's ownership record after port reuse.
func (c *cdpAddressCoordinator) unregister(browser *CDPBrowser, addr string) {
	if addr == "" {
		return
	}
	c.mu.Lock()
	if c.owners[addr] == browser {
		delete(c.owners, addr)
	}
	c.mu.Unlock()
}

// acquireDial reserves one permit for a backend connection attempt and rechecks
// ownership after waiting. A browser startup can replace the owner while the attempt
// waits; the check prevents it from dialing a different browser after port reuse. The
// caller must call the returned function when the attempt ends. It is safe to call it
// more than once.
func (c *cdpAddressCoordinator) acquireDial(ctx context.Context, browser *CDPBrowser, addr string) (func(), error) {
	if err := c.gate.Acquire(ctx, 1); err != nil {
		return nil, context.Cause(ctx)
	}
	if cause := context.Cause(ctx); cause != nil {
		c.gate.Release(1)
		return nil, cause
	}

	c.mu.Lock()
	// Recheck that browser still owns addr after waiting for the gate. The browser may
	// have stopped and unregistered addr during the wait.
	ownerOK := c.owners[addr] == browser
	c.mu.Unlock()
	if !ownerOK {
		c.gate.Release(1)
		return nil, errors.New("CDP browser endpoint is no longer active")
	}
	return sync.OnceFunc(func() { c.gate.Release(1) }), nil
}

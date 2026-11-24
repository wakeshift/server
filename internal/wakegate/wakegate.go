// Package wakegate passes on a deliverer's wakes only for workspaces that
// have one of its destinations.
//
// The pipeline leader wakes the MCP Events and agent webhook deliverers for
// every agent it delivers to (core.Core's Delivered), and each wake used to
// cost a claim transaction per deliverer in the agent's workspace, though
// most workspaces have no webhook at all. A Gate collects wakes and, for
// each batch, reads once which of their workspaces have a destination (a
// platform-level target table, by its primary key), then passes on only
// those. The read starts after every wake in its batch arrived, so it sees
// every target committed before the delivery that caused the wake: a
// webhook subscribed on another instance a moment ago is woken as before.
// Wakes that come in while a read runs wait for the next one, so a burst
// costs one read, not one per agent. A read that fails passes on the whole
// batch (as before this gate, the claim decides).
package wakegate

import (
	"context"
	"sync"
)

// Has reports which of tenants have a destination.
type Has func(ctx context.Context, tenants []string) (map[string]bool, error)

type wake struct{ tenant, agent string }

// Gate is a set of wakes not yet checked. The zero value is not usable;
// call New.
type Gate struct {
	mu      sync.Mutex
	pending map[wake]bool
	order   []wake
	ready   chan struct{}
}

// New returns an empty Gate.
func New() *Gate {
	return &Gate{pending: map[wake]bool{}, ready: make(chan struct{}, 1)}
}

// Add wakes agent in tenant once Run has checked the workspace. It never
// blocks.
func (g *Gate) Add(tenant, agent string) {
	w := wake{tenant, agent}
	g.mu.Lock()
	if !g.pending[w] {
		g.pending[w] = true
		g.order = append(g.order, w)
	}
	g.mu.Unlock()
	select {
	case g.ready <- struct{}{}:
	default:
	}
}

// take returns the wakes added so far, and empties the set.
func (g *Gate) take() []wake {
	g.mu.Lock()
	defer g.mu.Unlock()
	batch := g.order
	g.order = nil
	clear(g.pending)
	return batch
}

// Run checks wakes in batches until ctx ends, calling pass for each wake
// whose workspace has (or may have) a destination.
func (g *Gate) Run(ctx context.Context, has Has, pass func(tenant, agent string)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.ready:
		}
		g.Check(ctx, has, pass)
	}
}

// Check checks the wakes added so far, once: Run's step, and tests'.
func (g *Gate) Check(ctx context.Context, has Has, pass func(tenant, agent string)) {
	batch := g.take()
	if len(batch) == 0 {
		return
	}
	seen := map[string]bool{}
	var tenants []string
	for _, w := range batch {
		if !seen[w.tenant] {
			seen[w.tenant] = true
			tenants = append(tenants, w.tenant)
		}
	}
	with, err := has(ctx, tenants)
	for _, w := range batch {
		if err != nil || with[w.tenant] {
			pass(w.tenant, w.agent)
		}
	}
}

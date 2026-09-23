package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"
)

// Single-instance queues hold no database connections or Redis leases. The
// process-wide cap also bounds memory retained by queued request bodies.
const gatewayQueueTimeout = 30 * time.Second

type accountBusy struct{ ID int64 }

func (e *accountBusy) Error() string { return "upstream accounts are busy" }

func (a *App) wakeGatewayLocked() {
	if a.gatewayWake != nil {
		close(a.gatewayWake)
	}
	a.gatewayWake = make(chan struct{})
}

// StopAdmission rejects new work and wakes queues before graceful HTTP shutdown.
// Already dispatched work retains its slots through settlement.
func (a *App) StopAdmission() {
	a.gatewayMu.Lock()
	defer a.gatewayMu.Unlock()
	a.gatewayStopped = true
	a.wakeGatewayLocked()
	for _, cancel := range a.websockets {
		cancel()
	}
}

func (a *App) admissionWake() (<-chan struct{}, error) {
	a.gatewayMu.Lock()
	defer a.gatewayMu.Unlock()
	if a.gatewayStopped || a.instanceLost.Load() {
		return nil, &apiError{503, "gateway is stopping or has lost its instance lock"}
	}
	if a.gatewayWake == nil {
		a.gatewayWake = make(chan struct{})
	}
	return a.gatewayWake, nil
}

// Retry admission on release, or periodically for configuration changes. The
// callback must acquire its slot only on success and retain nothing on failure.
// ponytail: broadcast wakes all waiters (capped at 128); use per-resource wakeups
// if measured database traffic from concurrent releases becomes significant.
func (a *App) waitAdmission(ctx context.Context, kind string, id int64, timeout time.Duration, attempt func(context.Context) (bool, error), ping func() error) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	key := fmt.Sprintf("%s:%d", kind, id)
	queued := false
	defer func() {
		if queued {
			a.gatewayMu.Lock()
			a.gatewayWaiting[key]--
			if a.gatewayWaiting[key] == 0 {
				delete(a.gatewayWaiting, key)
			}
			a.gatewayQueued--
			a.gatewayMu.Unlock()
		}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.Canceled) {
				return queued, &apiError{499, "request canceled while waiting for concurrency"}
			}
			return queued, &apiError{429, kind + " concurrency wait timed out"}
		}
		wake, err := a.admissionWake()
		if err != nil {
			return queued, err
		}
		ready, err := attempt(ctx)
		if !ready && ctx.Err() != nil {
			continue // Normalize deadline/cancellation errors from database checks.
		}
		if err != nil || ready {
			return queued, err
		}
		if !queued {
			limit := 100
			if kind == "user" {
				limit = 20
			}
			a.gatewayMu.Lock()
			if a.gatewayWaiting[key] >= limit || a.gatewayQueued >= 128 {
				a.gatewayMu.Unlock()
				return false, &apiError{429, kind + " concurrency queue is full"}
			}
			if a.gatewayWaiting == nil {
				a.gatewayWaiting = map[string]int{}
			}
			a.gatewayWaiting[key]++
			a.gatewayQueued++
			a.gatewayMu.Unlock()
			queued = true
		}
		select {
		case <-ctx.Done():
		case <-wake:
		case <-tick.C:
		case <-heartbeat.C:
			if ping != nil {
				if err := ping(); err != nil {
					return queued, err
				}
			}
		}
	}
}

func (a *App) acquireGatewayUser(r *http.Request, g *gatewayIdentity, model string, ping func() error) (*gatewayIdentity, error) {
	initial := g
	first := true
	_, err := a.waitAdmission(r.Context(), "user", g.UserID, gatewayQueueTimeout, func(ctx context.Context) (bool, error) {
		if !first {
			if err := a.checkInstance(ctx); err != nil {
				return false, err
			}
			var err error
			g, err = a.gatewayAuth(r.WithContext(ctx), true)
			if err != nil {
				return false, err
			}
			if g.Key.ID != initial.Key.ID || g.Key.GroupID != initial.Key.GroupID || g.UserID != initial.UserID || !sameRoutingGroup(g, initial) {
				return false, conflict("API key assignment changed while queued; retry the request")
			}
		}
		first = false
		if !g.Group.allows(model) {
			return false, denied()
		}
		if g.Concurrency <= 0 {
			return false, &apiError{429, "user concurrency is disabled"}
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		ready := a.takeSlot("user", g.UserID, g.Concurrency)
		if ready && ctx.Err() != nil {
			a.releaseSlot("user", g.UserID)
			return false, ctx.Err()
		}
		return ready, nil
	}, ping)
	if err != nil {
		return initial, err
	}
	return g, nil
}

// Policies and routing are already captured at account selection. If they change
// during a wait, require a new request rather than combining old and new policy.
func (a *App) revalidateQueuedRequest(r *http.Request, g *gatewayIdentity, group gatewayGroup, in textRequest, routingModel string) error {
	if err := a.checkInstance(r.Context()); err != nil {
		return err
	}
	fresh, err := a.gatewayAuth(r, true)
	if err != nil {
		return err
	}
	if fresh.Key.ID != g.Key.ID || fresh.Key.GroupID != g.Key.GroupID || fresh.UserID != g.UserID || !reflect.DeepEqual(fresh.Group, group) || !sameRoutingGroup(fresh, g) {
		return conflict("gateway policy changed while queued; retry the request")
	}
	if !fresh.Group.allows(in.Model) {
		return denied()
	}
	routing := fresh.dispatchGroup()
	if routing.Platform == "composite" {
		config, err := a.loadComposite(r.Context(), routing.ID)
		if err != nil {
			return err
		}
		decision := config.resolve(routing.ID, in.Model, in.compositeEndpoint())
		if !decision.Matched || decision.TargetPlatform != g.Group.Platform || decision.UpstreamModel != routingModel {
			return conflict("composite route changed while queued; retry the request")
		}
	}
	if fresh.Group.Platform == "composite" {
		if err = a.checkPlatformQuota(r.Context(), g.UserID, g.Group.Platform); err != nil {
			return err
		}
	}
	a.gatewayMu.Lock()
	over := fresh.Concurrency <= 0 || a.gatewayActive[fmt.Sprintf("user:%d", g.UserID)] > fresh.Concurrency
	a.gatewayMu.Unlock()
	if over {
		return &apiError{429, "user concurrency limit changed while queued"}
	}
	g.Key, g.Concurrency, g.RPM = fresh.Key, fresh.Concurrency, fresh.RPM
	return nil
}

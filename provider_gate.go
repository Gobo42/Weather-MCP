package main

import (
	"context"
	"sync"
	"time"
)

// One gate is shared by current and history requests for every location.
type ProviderGate struct {
	mutex                  sync.Mutex
	spacing                time.Duration
	nextStart, notBefore   time.Time
	active, waitingCurrent int
	wake                   chan struct{}
	persist                func(context.Context, time.Time) error
}

func newProviderGate(spacing time.Duration) *ProviderGate {
	return &ProviderGate{spacing: spacing, wake: make(chan struct{})}
}
func (gate *ProviderGate) signal() { close(gate.wake); gate.wake = make(chan struct{}) }
func (gate *ProviderGate) NotBefore() time.Time {
	gate.mutex.Lock()
	defer gate.mutex.Unlock()
	return gate.notBefore
}
func (gate *ProviderGate) DeferUntil(deadline time.Time) {
	gate.mutex.Lock()
	defer gate.mutex.Unlock()
	if deadline.After(gate.notBefore) {
		gate.notBefore = deadline
		gate.signal()
	}
}
func (gate *ProviderGate) release() {
	gate.mutex.Lock()
	defer gate.mutex.Unlock()
	gate.active--
	gate.signal()
}
func (gate *ProviderGate) Acquire(ctx context.Context, current bool) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	gate.mutex.Lock()
	if current {
		gate.waitingCurrent++
	}
	defer func() {
		if current {
			gate.waitingCurrent--
			gate.signal()
		}
		gate.mutex.Unlock()
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		now := time.Now()
		if gate.notBefore.After(now) {
			return nil, &ProviderError{Code: "provider_cooldown", Retryable: true, RetryAt: gate.notBefore}
		}
		if gate.canStart(current, now) {
			gate.active++
			gate.nextStart = now.Add(gate.spacing)
			return gate.release, nil
		}
		gate.wait(ctx, now)

	}
}

func (gate *ProviderGate) canStart(current bool, now time.Time) bool {
	return gate.active < 2 && (current || gate.waitingCurrent == 0) && !gate.nextStart.After(now)
}

// Wait releases the lock while blocked and returns with the lock held.
func (gate *ProviderGate) wait(ctx context.Context, now time.Time) {
	wake := gate.wake
	var timer *time.Timer
	var tick <-chan time.Time
	if gate.nextStart.After(now) {
		timer = time.NewTimer(time.Until(gate.nextStart))
		tick = timer.C
	}
	gate.mutex.Unlock()
	select {
	case <-ctx.Done():
	case <-wake:
	case <-tick:
	}
	if timer != nil {
		timer.Stop()
	}
	gate.mutex.Lock()
}

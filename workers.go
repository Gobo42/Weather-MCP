package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

func prepareCollectors(ctx context.Context, config Config, store *Store, logger *slog.Logger) ([]*Collector, error) {
	if err := store.RegisterLocations(ctx, config); err != nil {
		return nil, err
	}
	cooldown, err := store.ProviderCooldown(ctx)
	if err != nil {
		return nil, err
	}
	gate := newProviderGate(time.Second)
	gate.DeferUntil(cooldown)
	gate.persist = store.PersistProviderCooldown
	var workers []*Collector
	for _, location := range config.locations() {
		workerStore := store.ForLocation(location)
		provider := newProvider(workerStore.config)
		if len(workers) > 0 {
			provider.client = workers[0].Provider.client
		}
		provider.gate = gate
		provider.logger = logger.With("location_id", location.ID)
		workerStore.logger = provider.logger
		workers = append(workers, &Collector{Config: workerStore.config, Store: workerStore, Provider: provider, Logger: provider.logger})
	}
	return workers, nil
}
func runCollectors(ctx context.Context, workers []*Collector, once bool) error {
	var group sync.WaitGroup
	results := make(chan error, len(workers))
	for _, worker := range workers {
		group.Add(1)
		go func() { defer group.Done(); results <- worker.Run(ctx, once) }()
	}
	group.Wait()
	close(results)
	var first error
	for err := range results {
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

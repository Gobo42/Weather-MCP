package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	slog.SetDefault(newLogger(os.Stderr, "info", false))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := command(ctx, os.Args[1:]); err != nil {
		slog.Error("command_failed", "error_code", errorCode(err))
		os.Exit(1)
	}
}
func collectorLock(path string) (func(), error) {
	if err := safePath(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, failure("state_lock_unavailable")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		file.Close()
		return nil, failure("private_lock_required")
	}
	if syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		file.Close()
		return nil, failure("collector_already_running")
	}
	return func() { syscall.Flock(int(file.Fd()), syscall.LOCK_UN); file.Close() }, nil
}
func command(ctx context.Context, args []string) error {
	if len(args) == 0 || len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Println("Weather Agent\n\nUsage:\n  weather-agent check [--config FILE] [--debug]\n  weather-agent collect [--config FILE] [--once] [--debug]\n  weather-agent set-location --plus-code TEXT [--config FILE] [--location ID] [--debug]\n\nDefault config: /etc/weather-agent/collector.json")
		return nil
	}
	if args[0] == "set-location" {
		return setLocationCommand(ctx, args[1:], os.Stdin, os.Stdout)
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	filename := flags.String("config", "/etc/weather-agent/collector.json", "Collector JSON file")
	debug := flags.Bool("debug", false, "Enable diagnostic logging for this command")
	once := flags.Bool("once", false, "Collect one current sample and attempt one eligible history chunk")
	if err := flags.Parse(args[1:]); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return failure("invalid_arguments")
	}
	if flags.NArg() != 0 || (args[0] != "check" && args[0] != "collect") || *once && args[0] != "collect" {
		return failure("invalid_arguments")
	}
	config, err := loadConfig(*filename)
	if err != nil {
		return err
	}
	logger := newLogger(os.Stderr, config.LogLevel, *debug)
	slog.SetDefault(logger)
	store, err := openStore(config)
	if err != nil {
		return err
	}
	defer store.Close()
	if err = store.Check(ctx, config); err != nil {
		return err
	}
	if args[0] == "check" {
		logger.Debug("configuration_checked")
		fmt.Println("configuration, schema and collector grants: ok")
		return nil
	}
	if err = privateDirectory(config.StateDir, true); err != nil {
		return err
	}
	release, err := collectorLock(filepath.Join(config.StateDir, "collector.lock"))
	if err != nil {
		return err
	}
	defer release()
	logger.Info("service_started", "locations", len(config.locations()), "once", *once)
	defer logger.Info("service_stopped")
	workers, err := prepareCollectors(ctx, config, store, logger)
	if err != nil {
		return err
	}
	return runCollectors(ctx, workers, *once)
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xrdavies/lite-api/internal/app"
	"github.com/xrdavies/lite-api/schema"
)

func main() {
	if err := run(); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) != 2 {
		return errors.New("usage: lite-api init-db | bootstrap | serve | upstream-check")
	}
	cfg := app.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch os.Args[1] {
	case "upstream-check":
		checkCtx, checkCancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer checkCancel()
		result, err := app.CheckUpstream(checkCtx, os.Getenv("UPSTREAM_BASE_URL"), os.Getenv("UPSTREAM_API_KEY"), os.Getenv("UPSTREAM_MODEL"))
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	case "init-db", "bootstrap":
		db, err := app.OpenDatabase(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer db.Close()
		if os.Args[1] == "init-db" {
			if err = schema.Initialize(ctx, db); err != nil {
				return err
			}
		} else {
			if err = schema.Validate(ctx, db); err != nil {
				return err
			}
			if err = app.Bootstrap(ctx, db, os.Getenv("ADMIN_EMAIL"), os.Getenv("ADMIN_PASSWORD")); err != nil {
				return err
			}
		}
		fmt.Println(os.Args[1] + " completed")
		return nil
	case "serve":
		a, err := app.New(ctx, cfg)
		if err != nil {
			return err
		}
		defer a.Close()
		server := &http.Server{Addr: cfg.ListenAddr, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
		stopped, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		done := make(chan error, 1)
		go func() { done <- server.ListenAndServe() }()
		slog.Info("lite-api listening", "address", cfg.ListenAddr)
		select {
		case err = <-done:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-stopped.Done():
		}
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdown); err != nil {
			server.Close()
			return err
		}
		return nil
	default:
		return errors.New("unknown command")
	}
}

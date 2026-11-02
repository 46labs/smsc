// Command smsc runs the SMPP SMSC mock.
//
//	SMSC_ADDR          SMPP listener, default :2775
//	SMSC_CONTROL_ADDR  control API, default :8080
//	SMSC_ACCOUNTS      system_id:password pairs, comma separated
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/46labs/smsc/pkg/smsc"
)

func main() {
	if err := run(); err != nil {
		slog.Error("smsc", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	accounts, err := parseAccounts(os.Getenv("SMSC_ACCOUNTS"))
	if err != nil {
		return err
	}
	srv := smsc.New(ctx, accounts)

	ln, err := net.Listen("tcp", env("SMSC_ADDR", ":2775"))
	if err != nil {
		return err
	}
	control := &http.Server{Addr: env("SMSC_CONTROL_ADDR", ":8080"), Handler: srv.Control(), ReadHeaderTimeout: 10 * time.Second}

	errs := make(chan error, 2)
	go func() { errs <- srv.Serve(ln) }()
	go func() {
		if err := control.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()
	slog.Info("smsc", "smpp", ln.Addr().String(), "control", control.Addr, "accounts", len(accounts))

	select {
	case err = <-errs:
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if cerr := ln.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
		slog.Warn("smsc: close listener", "error", cerr)
	}
	if cerr := control.Shutdown(shutdown); cerr != nil {
		slog.Warn("smsc: shutdown control", "error", cerr)
	}
	return err
}

func parseAccounts(v string) (map[string]string, error) {
	out := map[string]string{}
	for pair := range strings.SplitSeq(v, ",") {
		if pair = strings.TrimSpace(pair); pair == "" {
			continue
		}
		id, pw, ok := strings.Cut(pair, ":")
		if !ok || id == "" {
			return nil, errors.New("SMSC_ACCOUNTS: want system_id:password pairs")
		}
		out[id] = pw
	}
	if len(out) == 0 {
		return nil, errors.New("SMSC_ACCOUNTS is empty")
	}
	return out, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

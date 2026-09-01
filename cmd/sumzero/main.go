// Command sumzero operates a sumzero ledger from the shell.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/YusufDrymz/sumzero/httpapi"
	"github.com/YusufDrymz/sumzero/ledger"
)

const usage = `sumzero — double-entry ledger for Postgres

usage:
  sumzero serve  [--dsn URL] [--addr HOST:PORT] [--idempotency-ttl DUR]
  sumzero verify [--dsn URL] [--json]

  serve    run the REST API. POST /v1/transfers requires an Idempotency-Key.
  verify   recompute every balance from the postings and re-walk the hash
           chain. Exits non-zero if the ledger does not check out.

The DSN is read from --dsn or DATABASE_URL.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sumzero:", err)
		os.Exit(2)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}

	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "verify":
		return verify(args[1:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q (try --help)", args[0])
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("DATABASE_URL"), "postgres connection string")
	addr := fs.String("addr", envOr("SUMZERO_ADDR", ":8080"), "listen address")
	ttl := fs.Duration("idempotency-ttl", 24*time.Hour, "how long a completed idempotency key is replayable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dsn == "" {
		return fmt.Errorf("no database: pass --dsn or set DATABASE_URL")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("cannot reach the database: %w", err)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	srv := &http.Server{
		Addr:              *addr,
		Handler:           httpapi.New(pool, ledger.NewIdempotencyStore(pool, *ttl), log),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", *addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	// Let in-flight requests finish: a transfer cut off mid-write would leave
	// the caller not knowing whether the money moved.
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func verify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("DATABASE_URL"), "postgres connection string")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dsn == "" {
		return fmt.Errorf("no database: pass --dsn or set DATABASE_URL")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	report, err := ledger.New(pool).Verify(ctx)
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	} else {
		printReport(report)
	}

	// A failed verification is a result, not a crash: exit 1 so cron and CI can
	// act on it, and keep exit 2 for "the check could not run".
	if !report.OK() {
		os.Exit(1)
	}
	return nil
}

func printReport(r ledger.Report) {
	fmt.Printf("%d accounts, %d transfers, %d postings (%s)\n",
		r.Accounts, r.Transfers, r.Postings, r.Took)

	if r.OK() {
		fmt.Println("ok: balances match the postings and the chain is intact")
		return
	}

	fmt.Printf("\n%d problem(s):\n", len(r.Problems))
	for _, p := range r.Problems {
		fmt.Printf("  %s\n", p)
	}
}

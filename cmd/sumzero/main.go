// Command sumzero operates a sumzero ledger from the shell.
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
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
  sumzero reconcile --account ID --file bank.csv --from DATE --to DATE [--json]

  serve      run the REST API. POST /v1/transfers requires an Idempotency-Key.
  verify     recompute every balance from the postings and re-walk the hash
             chain. Exits non-zero if the ledger does not check out.
  reconcile  compare an account against an external CSV (reference,amount
             [,date]) and report what matches, what differs and what is missing
             on either side. Exits 1 when the two sides disagree.

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
	case "reconcile":
		return reconcile(args[1:])
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
	keys := ledger.NewIdempotencyStore(pool, *ttl)
	go housekeeping(ctx, ledger.New(pool), keys, log)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           httpapi.New(pool, keys, log),
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

// housekeeping expires past-due holds every minute and drops expired
// idempotency keys every hour. Holds matter more: until a hold is expired it
// still reserves money, so a slow sweep is a slow refund of availability.
func housekeeping(ctx context.Context, lg *ledger.Ledger, keys *ledger.IdempotencyStore, log *slog.Logger) {
	holds := time.NewTicker(time.Minute)
	defer holds.Stop()
	sweep := time.NewTicker(time.Hour)
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-holds.C:
			if n, err := lg.ExpireHolds(ctx); err != nil {
				log.Warn("hold expiry failed", "err", err)
			} else if n > 0 {
				log.Info("holds expired", "count", n)
			}
		case <-sweep.C:
			if n, err := keys.Sweep(ctx); err != nil {
				log.Warn("idempotency sweep failed", "err", err)
			} else if n > 0 {
				log.Info("idempotency keys swept", "deleted", n)
			}
		}
	}
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

func reconcile(args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("DATABASE_URL"), "postgres connection string")
	account := fs.String("account", "", "account id to reconcile")
	file := fs.String("file", "", "CSV with columns: reference,amount[,date]; amount in minor units, signed")
	from := fs.String("from", "", "window start (RFC 3339 or YYYY-MM-DD)")
	to := fs.String("to", "", "window end, inclusive (RFC 3339 or YYYY-MM-DD)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dsn == "" {
		return fmt.Errorf("no database: pass --dsn or set DATABASE_URL")
	}
	if *account == "" || *file == "" || *from == "" || *to == "" {
		return fmt.Errorf("--account, --file, --from and --to are required")
	}
	start, err := parseDate(*from, false)
	if err != nil {
		return fmt.Errorf("--from: %w", err)
	}
	end, err := parseDate(*to, true)
	if err != nil {
		return fmt.Errorf("--to: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	lg := ledger.New(pool)
	acc, err := lg.Account(ctx, *account)
	if err != nil {
		return err
	}
	entries, err := readExternalCSV(*file, acc.Currency)
	if err != nil {
		return err
	}

	report, err := lg.Reconcile(ctx, *account, start, end, entries)
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
		printReconcile(report)
	}
	if !report.Clean() {
		os.Exit(1)
	}
	return nil
}

// readExternalCSV parses "reference,amount[,date]". A header row is skipped if
// its first cell says "reference". Amounts are signed minor units — there is no
// decimal parsing on purpose; "1.234,56" versus "1,234.56" is how money gets
// lost at a file boundary.
func readExternalCSV(path, currency string) ([]ledger.ExternalEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	rd := csv.NewReader(f)
	rd.FieldsPerRecord = -1
	rd.TrimLeadingSpace = true

	var out []ledger.ExternalEntry
	for line := 1; ; line++ {
		rec, err := rd.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if line == 1 && strings.EqualFold(strings.TrimSpace(rec[0]), "reference") {
			continue
		}
		if len(rec) < 2 {
			return nil, fmt.Errorf("%s:%d: need at least reference,amount", path, line)
		}
		amount, err := strconv.ParseInt(strings.TrimSpace(rec[1]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: amount %q must be signed minor units", path, line, rec[1])
		}
		e := ledger.ExternalEntry{Reference: strings.TrimSpace(rec[0]), Amount: ledger.Amount(amount, currency)}
		if len(rec) >= 3 && strings.TrimSpace(rec[2]) != "" {
			if e.Date, err = parseDate(strings.TrimSpace(rec[2]), false); err != nil {
				return nil, fmt.Errorf("%s:%d: %w", path, line, err)
			}
		}
		out = append(out, e)
	}
}

// parseDate accepts RFC 3339 or a bare day. A bare day used as a window end
// means the whole day, so it resolves to the last instant of it.
func parseDate(s string, endOfDay bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither RFC 3339 nor YYYY-MM-DD", s)
	}
	if endOfDay {
		t = t.Add(24*time.Hour - time.Microsecond)
	}
	return t, nil
}

func printReconcile(r ledger.ReconcileReport) {
	fmt.Printf("%s (%s) %s → %s\n", r.Account, r.Currency,
		r.From.Format("2006-01-02"), r.To.Format("2006-01-02"))
	fmt.Printf("  matched %d · amount mismatch %d · missing in ledger %d · missing externally %d\n",
		len(r.Matched), len(r.AmountMismatch), len(r.MissingInLedger), len(r.MissingExternally))
	fmt.Printf("  ledger %d · external %d · difference %d\n", r.LedgerTotal, r.ExternalTotal, r.Difference)

	if r.Clean() {
		fmt.Println("ok: both sides agree")
		return
	}
	for _, m := range r.AmountMismatch {
		fmt.Printf("  ~ %-24s ledger %d, external %d\n", m.Reference, m.Ledger.Amount, m.External.Amount)
	}
	for _, e := range r.MissingInLedger {
		fmt.Printf("  + %-24s %d  (external only: money moved, not recorded)\n", e.Reference, e.Amount.Amount)
	}
	for _, l := range r.MissingExternally {
		fmt.Printf("  - %-24s %d  (ledger only: recorded, money did not move)\n", l.Reference, l.Amount.Amount)
	}
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

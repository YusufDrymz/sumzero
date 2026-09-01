// Package httpapi exposes a sumzero ledger over HTTP.
//
// The handlers are a thin layer: every rule lives in the ledger package, so the
// embedded and the served ledger cannot drift apart.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	idempotent "github.com/YusufDrymz/go-idempotent"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/YusufDrymz/sumzero/ledger"
)

// IdempotencyHeader is the header a write must carry.
const IdempotencyHeader = "Idempotency-Key"

// server holds the dependencies of the HTTP layer.
type server struct {
	lg   *ledger.Ledger
	pool *pgxpool.Pool
	log  *slog.Logger
}

// Config is what the HTTP layer needs beyond the database.
type Config struct {
	Keys idempotent.Store
	Log  *slog.Logger

	// Token, when set, is required as "Authorization: Bearer <token>" on every
	// /v1 route. Health and readiness stay open. Empty means no auth — the
	// caller is expected to put a gateway in front, and /v1/verify is
	// disabled because an unauthenticated full scan is a denial-of-service
	// invitation.
	Token string
}

// New builds the router. Writes go through the idempotency middleware; reads do
// not need it.
func New(pool *pgxpool.Pool, cfg Config) http.Handler {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	s := &server{lg: ledger.New(pool), pool: pool, log: log}

	mux := http.NewServeMux()

	// Liveness answers "is the process up", readiness answers "can it serve".
	// Conflating them makes a database blip restart healthy containers.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", s.ready)

	mux.HandleFunc("POST /v1/accounts", s.createAccount)
	mux.HandleFunc("GET /v1/accounts/{id}", s.getAccount)
	mux.HandleFunc("POST /v1/accounts/{id}/archive", s.archiveAccount)
	mux.HandleFunc("GET /v1/accounts/{id}/balance", s.getBalance)
	mux.HandleFunc("GET /v1/accounts/{id}/available", s.getAvailable)
	mux.HandleFunc("GET /v1/holds/{reference}", s.getHold)
	mux.HandleFunc("GET /v1/accounts/{id}/statement", s.getStatement)
	mux.HandleFunc("POST /v1/accounts/{id}/reconcile", s.reconcile)
	mux.HandleFunc("GET /v1/transfers/{reference}", s.getTransfer)
	if cfg.Token != "" {
		mux.HandleFunc("GET /v1/verify", s.verify)
	} else {
		mux.HandleFunc("GET /v1/verify", func(w http.ResponseWriter, r *http.Request) {
			writeErrorCode(w, http.StatusForbidden, "verify_disabled",
				"GET /v1/verify is only served when the API has a token; run `sumzero verify` instead")
		})
	}

	// The write paths: the key is mandatory here, and the middleware replays a
	// stored response when the same key comes back.
	keyed := idempotent.New(cfg.Keys)
	for pattern, h := range map[string]http.HandlerFunc{
		"POST /v1/transfers":                     s.postTransfer,
		"POST /v1/transfers/{reference}/reverse": s.reverseTransfer,
		"POST /v1/holds":                         s.postHold,
		"POST /v1/holds/{reference}/capture":     s.captureHold,
		"POST /v1/holds/{reference}/release":     s.releaseHold,
	} {
		mux.Handle(pattern, requireIdempotencyKey(keyed(h)))
	}

	if cfg.Token == "" {
		return mux
	}
	return requireBearer(cfg.Token, mux)
}

// requireBearer guards every /v1 route with a constant-time token check.
// /healthz and /readyz stay open: the orchestrator probing them has no token
// and needs none.
func requireBearer(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sumzero"`)
			writeErrorCode(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireIdempotencyKey rejects a write that arrives without a key.
//
// The middleware passes unkeyed requests through by design, which is right for
// a general-purpose library and wrong here: a money movement without a retry
// story is a bug waiting for a timeout.
func requireIdempotencyKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(IdempotencyHeader)
		if key == "" {
			badRequest(w, "missing_idempotency_key",
				"writes require an "+IdempotencyHeader+" header")
			return
		}
		// The library fingerprints the body, not the route. Scoping the key to
		// the route keeps "same key, same body, different endpoint" from
		// replaying the wrong endpoint's answer.
		r.Header.Set(IdempotencyHeader, r.Method+" "+r.URL.Path+" "+key)
		next.ServeHTTP(w, r)
	})
}

func (s *server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.pool.Ping(r.Context()); err != nil {
		s.log.WarnContext(r.Context(), "readiness check failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type accountRequest struct {
	ID       string             `json:"id"`
	Type     ledger.AccountType `json:"type"`
	Currency string             `json:"currency"`

	// AllowNegative defaults to true on the wire as in the engine: a client
	// that wants the guard says so.
	AllowNegative *bool `json:"allow_negative"`
}

func (s *server) createAccount(w http.ResponseWriter, r *http.Request) {
	var req accountRequest
	if !decode(w, r, &req) {
		return
	}

	a := ledger.Account{ID: req.ID, Type: req.Type, Currency: req.Currency, AllowNegative: true}
	if req.AllowNegative != nil {
		a.AllowNegative = *req.AllowNegative
	}
	if err := s.lg.CreateAccount(r.Context(), a); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *server) getAccount(w http.ResponseWriter, r *http.Request) {
	a, err := s.lg.Account(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *server) archiveAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.lg.Archive(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type balanceResponse struct {
	Account string       `json:"account"`
	Balance ledger.Money `json:"balance"`
	AsOf    *time.Time   `json:"as_of,omitempty"`
}

func (s *server) getBalance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	if raw := r.URL.Query().Get("as_of"); raw != "" {
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			badRequest(w, "invalid_as_of", "as_of must be an RFC 3339 timestamp")
			return
		}
		bal, err := s.lg.BalanceAsOf(r.Context(), id, at)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, balanceResponse{Account: id, Balance: bal, AsOf: &at})
		return
	}

	bal, err := s.lg.Balance(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, balanceResponse{Account: id, Balance: bal})
}

func (s *server) getAvailable(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	avail, err := s.lg.Available(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, balanceResponse{Account: id, Balance: avail})
}

type holdRequest struct {
	Account   string       `json:"account"`
	Reference string       `json:"reference"`
	Amount    ledger.Money `json:"amount"`
	ExpiresAt *time.Time   `json:"expires_at"`
}

func (s *server) postHold(w http.ResponseWriter, r *http.Request) {
	var req holdRequest
	if !decode(w, r, &req) {
		return
	}
	hr := ledger.HoldRequest{Account: req.Account, Reference: req.Reference, Amount: req.Amount}
	if req.ExpiresAt != nil {
		hr.ExpiresAt = *req.ExpiresAt
	}
	h, err := s.lg.Hold(r.Context(), hr)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, h)
}

func (s *server) getHold(w http.ResponseWriter, r *http.Request) {
	h, err := s.lg.HoldByReference(r.Context(), r.PathValue("reference"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// captureHold takes the same body as POST /v1/transfers: the transfer that
// moves the money. The hold reference comes from the path.
func (s *server) captureHold(w http.ResponseWriter, r *http.Request) {
	var req transferRequest
	if !decode(w, r, &req) {
		return
	}
	t := req.transfer()
	id, err := s.lg.Capture(r.Context(), r.PathValue("reference"), t)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, transferResponse{ID: id, Transfer: t})
}

func (s *server) releaseHold(w http.ResponseWriter, r *http.Request) {
	if err := s.lg.Release(r.Context(), r.PathValue("reference")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) getStatement(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var opt ledger.StatementOptions

	for _, f := range []struct {
		name string
		dst  *time.Time
	}{{"from", &opt.From}, {"to", &opt.To}} {
		if raw := q.Get(f.name); raw != "" {
			at, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				badRequest(w, "invalid_"+f.name, f.name+" must be an RFC 3339 timestamp")
				return
			}
			*f.dst = at
		}
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := parsePositiveInt(raw)
		if err != nil {
			badRequest(w, "invalid_limit", "limit must be a positive integer")
			return
		}
		opt.Limit = n
	}

	entries, err := s.lg.Statement(r.Context(), r.PathValue("id"), opt)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

type transferRequest struct {
	Reference   string           `json:"reference"`
	Description string           `json:"description"`
	PostedAt    *time.Time       `json:"posted_at"`
	Postings    []ledger.Posting `json:"postings"`
}

type transferResponse struct {
	ID       int64            `json:"id"`
	Transfer *ledger.Transfer `json:"transfer"`
}

func (req transferRequest) transfer() *ledger.Transfer {
	t := &ledger.Transfer{
		Reference:   req.Reference,
		Description: req.Description,
		Postings:    req.Postings,
	}
	if req.PostedAt != nil {
		t.PostedAt = *req.PostedAt
	}
	return t
}

func (s *server) postTransfer(w http.ResponseWriter, r *http.Request) {
	var req transferRequest
	if !decode(w, r, &req) {
		return
	}
	t := req.transfer()

	id, err := s.lg.Post(r.Context(), t)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, transferResponse{ID: id, Transfer: t})
}

type reverseRequest struct {
	Reference   string `json:"reference"`
	Description string `json:"description"`
}

func (s *server) reverseTransfer(w http.ResponseWriter, r *http.Request) {
	var req reverseRequest
	if !decode(w, r, &req) {
		return
	}
	id, err := s.lg.Reverse(r.Context(), r.PathValue("reference"), req.Reference, req.Description)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	t, _, err := s.lg.TransferByReference(r.Context(), req.Reference)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, transferResponse{ID: id, Transfer: &t})
}

func (s *server) getTransfer(w http.ResponseWriter, r *http.Request) {
	t, id, err := s.lg.TransferByReference(r.Context(), r.PathValue("reference"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, transferResponse{ID: id, Transfer: &t})
}

type reconcileRequest struct {
	From    time.Time              `json:"from"`
	To      time.Time              `json:"to"`
	Entries []ledger.ExternalEntry `json:"entries"`
}

// reconcile is a POST because the external record travels in the body, but it
// writes nothing: the report is the whole output.
func (s *server) reconcile(w http.ResponseWriter, r *http.Request) {
	var req reconcileRequest
	if !decode(w, r, &req) {
		return
	}
	if req.From.IsZero() || req.To.IsZero() || req.To.Before(req.From) {
		badRequest(w, "invalid_window", "from and to must be RFC 3339 timestamps with from <= to")
		return
	}
	if len(req.Entries) > 50_000 {
		badRequest(w, "too_many_entries", "reconcile at most 50000 entries per request")
		return
	}

	report, err := s.lg.Reconcile(r.Context(), r.PathValue("id"), req.From, req.To, req.Entries)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *server) verify(w http.ResponseWriter, r *http.Request) {
	report, err := s.lg.Verify(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if !report.OK() {
		// The check ran and the books are wrong: that is not a server fault,
		// but a caller polling this endpoint must not read it as healthy.
		status = http.StatusConflict
	}
	writeJSON(w, status, report)
}

// fail logs unexpected errors and writes the mapped response.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if isInternal(err) {
		s.log.ErrorContext(r.Context(), "request failed",
			"method", r.Method, "path", r.URL.Path, "err", err)
	}
	writeError(w, err)
}

func isInternal(err error) bool {
	for _, m := range statusOf {
		if errors.Is(err, m.err) {
			return false
		}
	}
	return true
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)) // reconcile files are the big ones
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		badRequest(w, "invalid_json", err.Error())
		return false
	}
	return true
}

func parsePositiveInt(s string) (int, error) {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int(c-'0')
		if n > 1_000_000 {
			return 0, errors.New("too large")
		}
	}
	if n == 0 {
		return 0, errors.New("must be positive")
	}
	return n, nil
}

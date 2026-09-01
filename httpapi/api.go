// Package httpapi exposes a sumzero ledger over HTTP.
//
// The handlers are a thin layer: every rule lives in the ledger package, so the
// embedded and the served ledger cannot drift apart.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	idempotent "github.com/YusufDrymz/go-idempotent"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/YusufDrymz/sumzero/ledger"
)

// IdempotencyHeader is the header a write must carry.
const IdempotencyHeader = "Idempotency-Key"

// Server holds the dependencies of the HTTP layer.
type Server struct {
	lg   *ledger.Ledger
	pool *pgxpool.Pool
	log  *slog.Logger
}

// New builds the router. Writes go through the idempotency middleware; reads do
// not need it.
func New(pool *pgxpool.Pool, keys idempotent.Store, log *slog.Logger) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{lg: ledger.New(pool), pool: pool, log: log}

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
	mux.HandleFunc("GET /v1/accounts/{id}/statement", s.getStatement)
	mux.HandleFunc("GET /v1/transfers/{reference}", s.getTransfer)
	mux.HandleFunc("GET /v1/verify", s.verify)

	// The write path: the key is mandatory here, and the middleware replays a
	// stored response when the same key comes back.
	post := http.HandlerFunc(s.postTransfer)
	mux.Handle("POST /v1/transfers",
		requireIdempotencyKey(idempotent.New(keys)(post)))

	return mux
}

// requireIdempotencyKey rejects a write that arrives without a key.
//
// The middleware passes unkeyed requests through by design, which is right for
// a general-purpose library and wrong here: a money movement without a retry
// story is a bug waiting for a timeout.
func requireIdempotencyKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(IdempotencyHeader) == "" {
			badRequest(w, "missing_idempotency_key",
				"POST /v1/transfers requires an "+IdempotencyHeader+" header")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
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
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var req accountRequest
	if !decode(w, r, &req) {
		return
	}

	a := ledger.Account{ID: req.ID, Type: req.Type, Currency: req.Currency}
	if err := s.lg.CreateAccount(r.Context(), a); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	a, err := s.lg.Account(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) archiveAccount(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) getBalance(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) getStatement(w http.ResponseWriter, r *http.Request) {
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
	if entries == nil {
		entries = []ledger.Entry{}
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

func (s *Server) postTransfer(w http.ResponseWriter, r *http.Request) {
	var req transferRequest
	if !decode(w, r, &req) {
		return
	}

	t := &ledger.Transfer{
		Reference:   req.Reference,
		Description: req.Description,
		Postings:    req.Postings,
	}
	if req.PostedAt != nil {
		t.PostedAt = *req.PostedAt
	}

	id, err := s.lg.Post(r.Context(), t)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, transferResponse{ID: id, Transfer: t})
}

func (s *Server) getTransfer(w http.ResponseWriter, r *http.Request) {
	t, id, err := s.lg.TransferByReference(r.Context(), r.PathValue("reference"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, transferResponse{ID: id, Transfer: &t})
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
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
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
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
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
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

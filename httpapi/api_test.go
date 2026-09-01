package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	idempotent "github.com/YusufDrymz/go-idempotent"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/YusufDrymz/sumzero/httpapi"
	"github.com/YusufDrymz/sumzero/ledger"
)

func newServer(t *testing.T) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	if testing.Short() {
		t.Skip("e2e: skipped in short mode")
	}
	ctx := context.Background()

	image := os.Getenv("SUMZERO_TEST_PG_IMAGE")
	if image == "" {
		image = "postgres:17-alpine"
	}
	ctr, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("sumzero"),
		tcpostgres.WithUsername("sumzero"),
		tcpostgres.WithPassword("sumzero"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("e2e: container start failed in CI: %v", err)
		}
		t.Skipf("e2e: docker unavailable, skipping: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	for _, f := range []string{"0001_init.sql", "0002_idempotency.sql"} {
		sql, err := os.ReadFile(filepath.Join("..", "migrations", f))
		require.NoError(t, err)
		_, err = pool.Exec(ctx, string(sql))
		require.NoError(t, err)
	}

	srv := httptest.NewServer(httpapi.New(pool, ledger.NewIdempotencyStore(pool, time.Hour), nil))
	t.Cleanup(srv.Close)
	return srv, pool
}

type call struct {
	status int
	body   []byte
	header http.Header
}

func do(t *testing.T, srv *httptest.Server, method, path, key string, payload any) call {
	t.Helper()
	var buf bytes.Buffer
	if payload != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(payload))
	}
	req, err := http.NewRequest(method, srv.URL+path, &buf)
	require.NoError(t, err)
	if key != "" {
		req.Header.Set(httpapi.IdempotencyHeader, key)
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body := make([]byte, 0, 1024)
	buf.Reset()
	_, err = buf.ReadFrom(resp.Body)
	require.NoError(t, err)
	body = append(body, buf.Bytes()...)
	return call{status: resp.StatusCode, body: body, header: resp.Header}
}

func openBooks(t *testing.T, srv *httptest.Server) {
	t.Helper()
	for _, a := range []map[string]string{
		{"id": "cash", "type": "asset", "currency": "TRY"},
		{"id": "revenue", "type": "income", "currency": "TRY"},
		{"id": "usd_cash", "type": "asset", "currency": "USD"},
	} {
		r := do(t, srv, "POST", "/v1/accounts", "", a)
		require.Equal(t, http.StatusCreated, r.status, string(r.body))
	}
}

func sale(reference string, minor int64) map[string]any {
	amount := map[string]string{"amount": fmt.Sprint(minor), "currency": "TRY"}
	return map[string]any{
		"reference": reference,
		"postings": []map[string]any{
			{"account": "cash", "amount": amount, "direction": "debit"},
			{"account": "revenue", "amount": amount, "direction": "credit"},
		},
	}
}

func TestTransferLifecycle(t *testing.T) {
	srv, _ := newServer(t)
	openBooks(t, srv)

	created := do(t, srv, "POST", "/v1/transfers", "key-1", sale("order-1", 10000))
	require.Equal(t, http.StatusCreated, created.status, string(created.body))

	got := do(t, srv, "GET", "/v1/transfers/order-1", "", nil)
	require.Equal(t, http.StatusOK, got.status)

	bal := do(t, srv, "GET", "/v1/accounts/cash/balance", "", nil)
	require.Equal(t, http.StatusOK, bal.status)
	// Money crosses the wire as a string so no JSON parser can round it.
	require.Contains(t, string(bal.body), `"amount":"10000"`)

	st := do(t, srv, "GET", "/v1/accounts/cash/statement", "", nil)
	require.Equal(t, http.StatusOK, st.status)
	require.Contains(t, string(st.body), "order-1")

	v := do(t, srv, "GET", "/v1/verify", "", nil)
	require.Equal(t, http.StatusOK, v.status, string(v.body))
}

func TestIdempotency(t *testing.T) {
	srv, _ := newServer(t)
	openBooks(t, srv)

	t.Run("write without a key is refused", func(t *testing.T) {
		r := do(t, srv, "POST", "/v1/transfers", "", sale("no-key", 100))
		require.Equal(t, http.StatusBadRequest, r.status)
		require.Contains(t, string(r.body), "missing_idempotency_key")
	})

	t.Run("same key replays the first response", func(t *testing.T) {
		first := do(t, srv, "POST", "/v1/transfers", "k-replay", sale("order-replay", 2500))
		require.Equal(t, http.StatusCreated, first.status)

		second := do(t, srv, "POST", "/v1/transfers", "k-replay", sale("order-replay", 2500))
		require.Equal(t, first.status, second.status)
		require.JSONEq(t, string(first.body), string(second.body), "retry must get the original response")

		// And the money moved exactly once.
		bal := do(t, srv, "GET", "/v1/accounts/cash/balance", "", nil)
		require.Contains(t, string(bal.body), `"amount":"2500"`)
	})

	t.Run("key reused with a different body is rejected", func(t *testing.T) {
		r := do(t, srv, "POST", "/v1/transfers", "k-replay", sale("order-other", 999))
		require.Equal(t, http.StatusUnprocessableEntity, r.status)
	})

	t.Run("concurrent retries post once", func(t *testing.T) {
		const n = 6
		var wg sync.WaitGroup
		results := make([]call, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i] = do(t, srv, "POST", "/v1/transfers", "k-race", sale("order-race", 700))
			}(i)
		}
		wg.Wait()

		// A replayed response carries the same 201 as the original, so status
		// alone cannot tell them apart. The replay header can.
		var wrote, replayed, busy int
		for _, r := range results {
			switch {
			case r.status == http.StatusCreated && r.header.Get(idempotent.ReplayHeader) == "":
				wrote++
			case r.status == http.StatusCreated:
				replayed++
			case r.status == http.StatusConflict:
				busy++ // another request held the key while this one arrived
			default:
				t.Fatalf("unexpected status %d: %s", r.status, r.body)
			}
		}
		require.Equal(t, 1, wrote, "exactly one request may write the transfer")
		require.Equal(t, n-1, replayed+busy)

		// The books are the real proof: 2500 from the replay test plus 700 here.
		bal := do(t, srv, "GET", "/v1/accounts/cash/balance", "", nil)
		require.Contains(t, string(bal.body), `"amount":"3200"`)
	})
}

func TestErrorMapping(t *testing.T) {
	srv, _ := newServer(t)
	openBooks(t, srv)

	require.Equal(t, http.StatusCreated,
		do(t, srv, "POST", "/v1/transfers", "k-dup", sale("taken", 100)).status)

	tests := []struct {
		name    string
		method  string
		path    string
		key     string
		payload any
		status  int
		code    string
	}{
		{
			name: "unbalanced transfer", method: "POST", path: "/v1/transfers", key: "k-a",
			payload: map[string]any{"reference": "bad", "postings": []map[string]any{
				{"account": "cash", "amount": map[string]string{"amount": "100", "currency": "TRY"}, "direction": "debit"},
				{"account": "revenue", "amount": map[string]string{"amount": "99", "currency": "TRY"}, "direction": "credit"},
			}},
			status: http.StatusUnprocessableEntity, code: "unbalanced",
		},
		{
			name: "currency does not match the account", method: "POST", path: "/v1/transfers", key: "k-b",
			payload: map[string]any{"reference": "fx", "postings": []map[string]any{
				{"account": "usd_cash", "amount": map[string]string{"amount": "100", "currency": "TRY"}, "direction": "debit"},
				{"account": "revenue", "amount": map[string]string{"amount": "100", "currency": "TRY"}, "direction": "credit"},
			}},
			status: http.StatusUnprocessableEntity, code: "currency_mismatch",
		},
		{
			name: "unknown account", method: "POST", path: "/v1/transfers", key: "k-c",
			payload: map[string]any{"reference": "ghost", "postings": []map[string]any{
				{"account": "ghost", "amount": map[string]string{"amount": "100", "currency": "TRY"}, "direction": "debit"},
				{"account": "revenue", "amount": map[string]string{"amount": "100", "currency": "TRY"}, "direction": "credit"},
			}},
			status: http.StatusNotFound, code: "unknown_account",
		},
		{
			name: "reference already posted", method: "POST", path: "/v1/transfers", key: "k-d",
			payload: sale("taken", 100),
			status:  http.StatusConflict, code: "duplicate_reference",
		},
		{
			name: "amount sent as a JSON number", method: "POST", path: "/v1/transfers", key: "k-e",
			payload: map[string]any{"reference": "float", "postings": []map[string]any{
				{"account": "cash", "amount": map[string]any{"amount": 100, "currency": "TRY"}, "direction": "debit"},
				{"account": "revenue", "amount": map[string]any{"amount": 100, "currency": "TRY"}, "direction": "credit"},
			}},
			status: http.StatusBadRequest, code: "invalid_json",
		},
		{
			name: "account already exists", method: "POST", path: "/v1/accounts",
			payload: map[string]string{"id": "cash", "type": "asset", "currency": "TRY"},
			status:  http.StatusConflict, code: "account_exists",
		},
		{
			name: "unknown account type", method: "POST", path: "/v1/accounts",
			payload: map[string]string{"id": "weird", "type": "vibes", "currency": "TRY"},
			status:  http.StatusBadRequest, code: "invalid_account_type",
		},
		{
			name: "missing account", method: "GET", path: "/v1/accounts/nope",
			status: http.StatusNotFound, code: "unknown_account",
		},
		{
			name: "missing transfer", method: "GET", path: "/v1/transfers/nope",
			status: http.StatusNotFound, code: "unknown_transfer",
		},
		{
			name: "bad as_of", method: "GET", path: "/v1/accounts/cash/balance?as_of=yesterday",
			status: http.StatusBadRequest, code: "invalid_as_of",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := do(t, srv, tt.method, tt.path, tt.key, tt.payload)
			require.Equal(t, tt.status, r.status, string(r.body))
			require.Contains(t, string(r.body), `"code":"`+tt.code+`"`)
		})
	}
}

func TestHealthAndReadiness(t *testing.T) {
	srv, pool := newServer(t)

	require.Equal(t, http.StatusOK, do(t, srv, "GET", "/healthz", "", nil).status)
	require.Equal(t, http.StatusOK, do(t, srv, "GET", "/readyz", "", nil).status)

	// With the database gone, liveness must still pass and readiness must fail:
	// a dead dependency should stop traffic, not restart the process.
	pool.Close()
	require.Equal(t, http.StatusOK, do(t, srv, "GET", "/healthz", "", nil).status)
	require.Equal(t, http.StatusServiceUnavailable, do(t, srv, "GET", "/readyz", "", nil).status)
}

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/YusufDrymz/sumzero/ledger"
)

// errorBody is the response shape for every failure. One shape, always, so a
// client can parse errors without branching on the endpoint.
type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// status maps a domain error to an HTTP status and a stable code.
//
// The split that matters: 400 means the request was malformed, 422 means it was
// well-formed but the books refuse it, 409 means it collided with something
// that already exists. A client retries none of these blindly.
var statusOf = []struct {
	err    error
	status int
	code   string
}{
	{ledger.ErrUnknownAccount, http.StatusNotFound, "unknown_account"},
	{ledger.ErrUnknownTransfer, http.StatusNotFound, "unknown_transfer"},
	{ledger.ErrAccountExists, http.StatusConflict, "account_exists"},
	{ledger.ErrDuplicateReference, http.StatusConflict, "duplicate_reference"},
	{ledger.ErrUnknownHold, http.StatusNotFound, "unknown_hold"},
	{ledger.ErrHoldNotActive, http.StatusConflict, "hold_not_active"},
	{ledger.ErrInsufficientFunds, http.StatusUnprocessableEntity, "insufficient_funds"},
	{ledger.ErrCaptureMismatch, http.StatusUnprocessableEntity, "capture_mismatch"},
	{ledger.ErrAccountArchived, http.StatusUnprocessableEntity, "account_archived"},
	{ledger.ErrCurrencyMismatch, http.StatusUnprocessableEntity, "currency_mismatch"},
	{ledger.ErrUnbalanced, http.StatusUnprocessableEntity, "unbalanced"},
	{ledger.ErrTooFewPostings, http.StatusBadRequest, "too_few_postings"},
	{ledger.ErrMissingReference, http.StatusBadRequest, "missing_reference"},
	{ledger.ErrNonPositiveAmount, http.StatusBadRequest, "non_positive_amount"},
	{ledger.ErrInvalidDirection, http.StatusBadRequest, "invalid_direction"},
	{ledger.ErrInvalidCurrency, http.StatusBadRequest, "invalid_currency"},
	{ledger.ErrInvalidAccount, http.StatusBadRequest, "invalid_account"},
	{ledger.ErrInvalidType, http.StatusBadRequest, "invalid_account_type"},
}

func writeError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "internal"
	for _, m := range statusOf {
		if errors.Is(err, m.err) {
			status, code = m.status, m.code
			break
		}
	}

	message := err.Error()
	if status == http.StatusInternalServerError {
		// Never leak a driver or SQL message to a caller; the log keeps the
		// detail, the client gets a code it can act on.
		message = "internal error"
	}

	var body errorBody
	body.Error.Code, body.Error.Message = code, message
	writeJSON(w, status, body)
}

func badRequest(w http.ResponseWriter, code, message string) {
	var body errorBody
	body.Error.Code, body.Error.Message = code, message
	writeJSON(w, http.StatusBadRequest, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

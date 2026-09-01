package ledger

import (
	"bytes"
	"context"
	"fmt"
	"time"
)

// Problem is one thing verification found wrong.
type Problem struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
}

func (p Problem) String() string { return p.Kind + " " + p.Subject + ": " + p.Detail }

// Report is the outcome of a verification pass.
type Report struct {
	Accounts  int       `json:"accounts"`
	Transfers int       `json:"transfers"`
	Postings  int       `json:"postings"`
	Problems  []Problem `json:"problems"` // never nil in JSON: see Verify
	Took      string    `json:"took"`
}

// OK reports whether the ledger passed every check.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Verify re-proves the ledger from its postings.
//
// Nothing here trusts a cached number: balances are summed from postings, every
// transfer is re-checked for sum zero, and the hash chain is walked from the
// first row. A ledger you cannot re-derive is a ledger you are only hoping is
// right.
func (l *Ledger) Verify(ctx context.Context) (Report, error) {
	start := time.Now()
	// An empty slice rather than nil, so the JSON says [] and a consumer can
	// count it without a null check.
	r := Report{Problems: []Problem{}}

	if err := l.db.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM accounts),
		       (SELECT count(*) FROM transfers),
		       (SELECT count(*) FROM postings)`).
		Scan(&r.Accounts, &r.Transfers, &r.Postings); err != nil {
		return r, err
	}

	for _, check := range []func(context.Context, *Report) error{
		l.checkTransfersBalance,
		l.checkBalancesMatchPostings,
		l.checkTrialBalance,
		l.checkChain,
	} {
		if err := check(ctx, &r); err != nil {
			return r, err
		}
	}

	r.Took = time.Since(start).Round(time.Millisecond).String()
	return r, nil
}

// checkTransfersBalance re-runs the sum-zero rule over stored rows. The engine
// enforces it on write; this asks whether the rows on disk still satisfy it,
// which is a different question once anyone has had a psql prompt.
func (l *Ledger) checkTransfersBalance(ctx context.Context, r *Report) error {
	rows, err := l.db.Query(ctx, `
		SELECT t.id, t.reference, p.currency,
		       sum(CASE p.direction WHEN 'debit' THEN p.amount ELSE -p.amount END) AS net
		FROM transfers t JOIN postings p ON p.transfer_id = t.id
		GROUP BY t.id, t.reference, p.currency
		HAVING sum(CASE p.direction WHEN 'debit' THEN p.amount ELSE -p.amount END) <> 0
		ORDER BY t.id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id        int64
			reference string
			currency  string
			net       int64
		)
		if err := rows.Scan(&id, &reference, &currency, &net); err != nil {
			return err
		}
		r.Problems = append(r.Problems, Problem{
			Kind:    "unbalanced-transfer",
			Subject: fmt.Sprintf("transfer %d (%s)", id, reference),
			Detail:  fmt.Sprintf("%s off by %d", currency, net),
		})
	}
	return rows.Err()
}

// checkBalancesMatchPostings compares the materialised balance against the sum
// of postings. Any difference means the cache drifted and reads are lying.
func (l *Ledger) checkBalancesMatchPostings(ctx context.Context, r *Report) error {
	rows, err := l.db.Query(ctx, `
		SELECT b.account_id, b.balance, coalesce(p.total, 0)
		FROM account_balances b
		LEFT JOIN (
			SELECT account_id,
			       sum(CASE direction WHEN 'debit' THEN amount ELSE -amount END) AS total
			FROM postings GROUP BY account_id
		) p ON p.account_id = b.account_id
		WHERE b.balance <> coalesce(p.total, 0)
		ORDER BY b.account_id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			account       string
			cached, real_ int64
		)
		if err := rows.Scan(&account, &cached, &real_); err != nil {
			return err
		}
		r.Problems = append(r.Problems, Problem{
			Kind:    "balance-drift",
			Subject: account,
			Detail:  fmt.Sprintf("cached %d, postings say %d (off by %d)", cached, real_, cached-real_),
		})
	}
	return rows.Err()
}

// checkTrialBalance sums every account per currency. In a ledger built only
// from balanced transfers this is zero; anything else means postings were added
// outside the engine.
func (l *Ledger) checkTrialBalance(ctx context.Context, r *Report) error {
	tb, err := l.TrialBalance(ctx)
	if err != nil {
		return err
	}
	for currency, sum := range tb {
		if sum != 0 {
			r.Problems = append(r.Problems, Problem{
				Kind:    "trial-balance",
				Subject: currency,
				Detail:  fmt.Sprintf("accounts sum to %d, expected 0", sum),
			})
		}
	}
	return nil
}

// checkChain walks the transfers in order and recomputes each digest from the
// stored postings. It catches a rewritten row, a deleted one, and a stored hash
// that no longer describes its own transfer.
func (l *Ledger) checkChain(ctx context.Context, r *Report) error {
	rows, err := l.db.Query(ctx, `
		SELECT t.id, t.reference, t.description, t.posted_at, t.prev_hash, t.hash,
		       p.account_id, p.amount, p.currency, p.direction
		FROM transfers t LEFT JOIN postings p ON p.transfer_id = t.id
		ORDER BY t.id, p.id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var (
		cur        *Transfer
		curID      int64
		postedAt   time.Time
		prevHash   []byte
		storedHash []byte
		expected   []byte
	)

	flush := func() {
		if cur == nil {
			return
		}
		if !bytes.Equal(prevHash, expected) {
			r.Problems = append(r.Problems, Problem{
				Kind:    "chain-break",
				Subject: fmt.Sprintf("transfer %d (%s)", curID, cur.Reference),
				Detail:  "prev_hash does not match the previous transfer's hash",
			})
		}
		if got := chainDigest(prevHash, cur, postedAt); !bytes.Equal(got, storedHash) {
			r.Problems = append(r.Problems, Problem{
				Kind:    "hash-mismatch",
				Subject: fmt.Sprintf("transfer %d (%s)", curID, cur.Reference),
				Detail:  "stored hash does not describe the stored postings",
			})
		}
		expected = storedHash
	}

	for rows.Next() {
		var (
			id                     int64
			reference, description string
			at                     time.Time
			prev, hash             []byte
			account, currency, dir *string
			amount                 *int64
		)
		if err := rows.Scan(&id, &reference, &description, &at, &prev, &hash,
			&account, &amount, &currency, &dir); err != nil {
			return err
		}

		if cur == nil || id != curID {
			flush()
			cur = &Transfer{Reference: reference, Description: description}
			curID, postedAt, prevHash, storedHash = id, at, prev, hash
		}
		if account != nil {
			cur.Postings = append(cur.Postings, Posting{
				Account: *account,
				Amount:  Money{Amount: *amount, Currency: *currency},
				Dir:     Direction(*dir),
			})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	flush()
	return nil
}

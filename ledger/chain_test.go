package ledger

import (
	"bytes"
	"testing"
	"time"
)

var chainTime = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func TestChainDigestIsStable(t *testing.T) {
	tx := (&Transfer{Reference: "ord-1", Description: "sale"}).
		Debit("cash", try(10000)).
		Credit("revenue", try(10000))

	a := chainDigest(nil, tx, chainTime)
	b := chainDigest(nil, tx, chainTime)
	if !bytes.Equal(a, b) {
		t.Fatal("same transfer produced two digests")
	}
}

func TestChainDigestDetectsTampering(t *testing.T) {
	base := func() *Transfer {
		return (&Transfer{Reference: "ord-1", Description: "sale"}).
			Debit("cash", try(10000)).
			Credit("revenue", try(10000))
	}
	want := chainDigest([]byte("prev"), base(), chainTime)

	tests := []struct {
		name string
		tx   *Transfer
		prev []byte
		at   time.Time
	}{
		{
			name: "amount changed by one kurus",
			tx: (&Transfer{Reference: "ord-1", Description: "sale"}).
				Debit("cash", try(10001)).Credit("revenue", try(10001)),
		},
		{
			name: "account swapped",
			tx: (&Transfer{Reference: "ord-1", Description: "sale"}).
				Debit("petty_cash", try(10000)).Credit("revenue", try(10000)),
		},
		{
			name: "legs reordered",
			tx: (&Transfer{Reference: "ord-1", Description: "sale"}).
				Credit("revenue", try(10000)).Debit("cash", try(10000)),
		},
		{
			name: "byte moved between reference and description",
			tx: (&Transfer{Reference: "ord-", Description: "1sale"}).
				Debit("cash", try(10000)).Credit("revenue", try(10000)),
		},
		{
			name: "backdated",
			tx:   base(),
			at:   chainTime.Add(-time.Hour),
		},
		{
			name: "predecessor rewritten",
			tx:   base(),
			prev: []byte("prev-tampered"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := tt.prev
			if prev == nil {
				prev = []byte("prev")
			}
			at := tt.at
			if at.IsZero() {
				at = chainTime
			}
			if bytes.Equal(want, chainDigest(prev, tt.tx, at)) {
				t.Fatal("tampered transfer kept the same digest")
			}
		})
	}
}

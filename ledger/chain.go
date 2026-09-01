package ledger

import (
	"crypto/sha256"
	"encoding/binary"
	"time"
)

// chainDigest computes the digest of a transfer, chained to the previous one.
//
// The encoding is length-prefixed rather than concatenated: without lengths,
// reference "ab"+description "c" and reference "a"+description "bc" hash the
// same, and a chain that can be fooled by moving a byte between fields is not
// worth keeping. Postings are hashed in stored order.
func chainDigest(prev []byte, t *Transfer, postedAt time.Time) []byte {
	h := sha256.New()
	h.Write(prev)

	writeField(h, []byte(t.Reference))
	writeField(h, []byte(t.Description))

	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(postedAt.UTC().UnixNano()))
	h.Write(ts[:])

	for _, p := range t.Postings {
		writeField(h, []byte(p.Account))
		writeField(h, []byte(p.Amount.Currency))
		writeField(h, []byte(p.Dir))

		var amt [8]byte
		binary.BigEndian.PutUint64(amt[:], uint64(p.Amount.Amount))
		h.Write(amt[:])
	}

	return h.Sum(nil)
}

type byteWriter interface{ Write([]byte) (int, error) }

func writeField(h byteWriter, b []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	h.Write(n[:])
	h.Write(b)
}

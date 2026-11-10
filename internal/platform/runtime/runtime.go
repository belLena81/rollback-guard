package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

type Clock struct{}

func (Clock) Now() time.Time {
	return time.Now().UTC()
}

type IDGenerator struct {
	counter atomic.Uint64
}

func (g *IDGenerator) NewID(prefix string) string {
	var random [4]byte
	_, _ = rand.Read(random[:]) //nolint:errcheck // documented never to fail
	return fmt.Sprintf("%s-%d-%s", prefix, g.counter.Add(1), hex.EncodeToString(random[:]))
}

// RandomSource reads from crypto/rand.
type RandomSource struct{}

// Fill fills b from crypto/rand. A short read is reported as an error so a
// caller never uses a partly filled buffer.
func (RandomSource) Fill(b []byte) error {
	n, err := rand.Read(b)
	if err != nil {
		return fmt.Errorf("read secure random bytes: %w", err)
	}
	if n != len(b) {
		return fmt.Errorf("read secure random bytes: short read of %d of %d bytes", n, len(b))
	}
	return nil
}

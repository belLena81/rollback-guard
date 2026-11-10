package fake

import (
	"sync"

	"github.com/belLena81/rollback-guard/internal/ports"
)

// RandomSource is a deterministic ports.RandomSource for tests. Each Fill
// writes a distinct, repeatable pattern derived from a call counter, or
// returns Err when it is set.
type RandomSource struct {
	mu    sync.Mutex
	calls int
	Err   error
}

func (r *RandomSource) Fill(b []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Err != nil {
		return r.Err
	}
	r.calls++
	seed := byte(r.calls % 256) //nolint:gosec // reduced modulo 256 first
	for i := range b {
		b[i] = seed*31 + byte(i%256) + 1 //nolint:gosec // reduced modulo 256 first
	}
	return nil
}

var _ ports.RandomSource = (*RandomSource)(nil)

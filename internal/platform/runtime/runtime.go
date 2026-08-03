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
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, g.counter.Add(1))
	}
	return fmt.Sprintf("%s-%d-%s", prefix, g.counter.Add(1), hex.EncodeToString(random[:]))
}

package runtime

import (
	"bytes"
	"testing"
)

func TestRandomSourceFillsWholeBufferWithDistinctValues(t *testing.T) {
	var source RandomSource
	first, second := make([]byte, 32), make([]byte, 32)
	if err := source.Fill(first); err != nil {
		t.Fatal(err)
	}
	if err := source.Fill(second); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, make([]byte, 32)) || bytes.Equal(first, second) {
		t.Fatal("random source returned zero or repeated bytes")
	}
}

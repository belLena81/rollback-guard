package domain_test

import (
	"errors"
	"testing"

	"github.com/belLena81/rollback-guard/internal/domain"
)

func TestParseBootstrapAdmin(t *testing.T) {
	tests := []struct {
		value string
		want  domain.BootstrapAdmin
		ok    bool
	}{
		{value: "alice:42", want: domain.BootstrapAdmin{Login: "alice", GitHubID: 42}, ok: true},
		{value: " Bob-1 : 7 ", want: domain.BootstrapAdmin{Login: "Bob-1", GitHubID: 7}, ok: true},
		{value: "alice"},
		{value: "alice:"},
		{value: ":42"},
		{value: "alice:0"},
		{value: "alice:-3"},
		{value: "alice:+3"},
		{value: "alice:007"},
		{value: "alice:4x"},
		{value: "alice:99999999999999999999"},
		{value: "al ice:4"},
		{value: "alice:4:5"},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			got, err := domain.ParseBootstrapAdmin(tt.value)
			if !tt.ok {
				if !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("err = %v, want validation error", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

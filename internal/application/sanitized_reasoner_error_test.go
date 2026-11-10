package application

import (
	"context"
	"errors"
	"testing"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

func TestSanitizedReasonerErrorCoversEveryFallbackReason(t *testing.T) {
	tests := []struct {
		reason domain.DiagnosisFallbackReason
		want   error
	}{
		{domain.FallbackProviderError, ports.ErrReasonerUnavailable},
		{domain.FallbackProviderTimeout, context.DeadlineExceeded},
		{domain.FallbackProviderRefusal, ports.ErrReasonerRefused},
		{domain.FallbackInvalidOutput, ports.ErrReasonerInvalidResponse},
		{domain.DiagnosisFallbackReason(0), ports.ErrReasonerUnavailable},
		{domain.FallbackInvalidOutput + 1, ports.ErrReasonerUnavailable},
	}
	for _, test := range tests {
		if got := sanitizedReasonerError(test.reason); !errors.Is(got, test.want) {
			t.Errorf("sanitizedReasonerError(%d) = %v, want %v", test.reason, got, test.want)
		}
	}
}

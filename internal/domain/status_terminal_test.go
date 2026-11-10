package domain_test

import (
	"testing"

	"github.com/belLena81/rollback-guard/internal/domain"
)

func TestStatusTerminalClassifiesEveryStatus(t *testing.T) {
	tests := []struct {
		status domain.Status
		want   bool
	}{
		{domain.StatusCreated, false},
		{domain.StatusInvestigating, false},
		{domain.StatusAwaitingApproval, false},
		{domain.StatusApproved, false},
		{domain.StatusExecuting, false},
		{domain.StatusVerifying, false},
		{domain.StatusResolved, true},
		{domain.StatusCanceled, true},
		{domain.StatusDeclined, true},
		{domain.StatusRejected, true},
		{domain.StatusExpired, true},
		{domain.StatusFailed, true},
		{domain.Status(0), false},
		{domain.StatusFailed + 1, false},
	}
	for _, test := range tests {
		if got := test.status.Terminal(); got != test.want {
			t.Errorf("Status(%d) %s Terminal() = %t, want %t", test.status, test.status, got, test.want)
		}
	}
}

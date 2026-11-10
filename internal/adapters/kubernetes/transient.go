package kubernetes

import (
	"github.com/belLena81/rollback-guard/internal/platform/resilience"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// MarkTransientAPIError flags Kubernetes API errors that describe an
// overloaded or unavailable API server (timeouts, throttling, 503, 500).
// Authorization, not-found, conflict, and invalid-request errors are returned
// unchanged because retrying cannot change them.
func MarkTransientAPIError(err error) error {
	if err == nil {
		return nil
	}
	if apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsInternalError(err) {
		return resilience.MarkTransient(err) //nolint:wrapcheck // decorator: original error passes through for classification
	}
	return err
}

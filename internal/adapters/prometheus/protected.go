package prometheus

import (
	"context"
	"errors"
	"time"

	"github.com/belLena81/rollback-guard/internal/platform/resilience"
	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

type protectedAPI struct {
	next      QueryAPI
	protector *resilience.Protector
}

// NewProtectedQueryAPI retries transient Prometheus failures (server errors,
// timeouts, malformed responses, connection failures) and opens a breaker
// after consecutive ones. Queries are read-only, so repeating one is always
// safe. Bad queries and client errors are returned unchanged.
func NewProtectedQueryAPI(next QueryAPI, protector *resilience.Protector) (QueryAPI, error) {
	if next == nil || protector == nil {
		return nil, errors.New("prometheus query API and protector are required")
	}
	return protectedAPI{next: next, protector: protector}, nil
}

type queryResult struct {
	value    model.Value
	warnings v1.Warnings
}

func (a protectedAPI) Query(ctx context.Context, query string, ts time.Time, opts ...v1.Option) (model.Value, v1.Warnings, error) {
	result, err := resilience.Call(ctx, a.protector, func(ctx context.Context) (queryResult, error) {
		value, warnings, err := a.next.Query(ctx, query, ts, opts...)
		return queryResult{value: value, warnings: warnings}, markTransientQueryError(err)
	})
	return result.value, result.warnings, err //nolint:wrapcheck // decorator: original error passes through for classification
}

// markTransientQueryError flags the Prometheus API error types that describe
// an unhealthy server rather than a rejected request.
func markTransientQueryError(err error) error {
	var apiErr *v1.Error
	if errors.As(err, &apiErr) {
		switch apiErr.Type {
		case v1.ErrServer, v1.ErrTimeout, v1.ErrBadResponse:
			return resilience.MarkTransient(err) //nolint:wrapcheck // decorator: original error passes through for classification
		case v1.ErrBadData, v1.ErrExec, v1.ErrCanceled, v1.ErrClient:
		}
	}
	return err
}

package application

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// Audit listing bounds. The page size is capped so one request cannot read
// the whole table.
const (
	DefaultAuditPageSize = 50
	MaxAuditPageSize     = 200
)

// AuditPage is one newest-first page of the auth audit log. NextCursor is
// empty on the last page.
type AuditPage struct {
	Events     []domain.AuthAuditEvent
	NextCursor string
}

// ListAuditEvents returns one page of the auth audit log, newest first.
// Administrators only. limit must be within 1..MaxAuditPageSize; cursor is
// empty for the first page, otherwise a NextCursor of an earlier page. The
// read runs in a read-only transaction.
func (s *AdminService) ListAuditEvents(ctx context.Context, actor domain.Actor, limit int, cursor string) (AuditPage, error) {
	if err := s.access.Authorize(ctx, actor, domain.ActionAdminister); err != nil {
		return AuditPage{}, fmt.Errorf("authorize list audit: %w", err)
	}
	if limit < 1 || limit > MaxAuditPageSize {
		return AuditPage{}, &domain.ValidationError{Field: "limit", Reason: fmt.Sprintf("must be between 1 and %d", MaxAuditPageSize)}
	}
	after, err := decodeAuditCursor(cursor)
	if err != nil {
		return AuditPage{}, err
	}
	var events []domain.AuthAuditEvent
	err = s.uow.WithinAuthReadTransaction(ctx, func(repos ports.AuthReadRepositories) error {
		// One extra row tells whether another page exists.
		var readErr error
		events, readErr = repos.Audit().ListPage(after, limit+1)
		if readErr != nil {
			return fmt.Errorf("list audit events: %w", readErr)
		}
		return nil
	})
	if err != nil {
		return AuditPage{}, fmt.Errorf("list audit events: %w", err)
	}
	page := AuditPage{Events: events}
	if len(events) > limit {
		page.Events = events[:limit]
		last := page.Events[limit-1]
		page.NextCursor = encodeAuditCursor(ports.AuthAuditCursor{OccurredAt: last.OccurredAt(), ID: last.ID()})
	}
	return page, nil
}

func encodeAuditCursor(c ports.AuthAuditCursor) string {
	raw := strconv.FormatInt(c.OccurredAt.UnixNano(), 10) + "|" + string(c.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeAuditCursor(cursor string) (ports.AuthAuditCursor, error) {
	if cursor == "" {
		return ports.AuthAuditCursor{}, nil
	}
	invalid := &domain.ValidationError{Field: "cursor", Reason: "is not a valid cursor"}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return ports.AuthAuditCursor{}, invalid
	}
	nanos, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return ports.AuthAuditCursor{}, invalid
	}
	n, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return ports.AuthAuditCursor{}, invalid
	}
	return ports.AuthAuditCursor{OccurredAt: time.Unix(0, n).UTC(), ID: domain.AuthAuditEventID(id)}, nil
}

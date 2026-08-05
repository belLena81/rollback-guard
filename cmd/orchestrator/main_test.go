package main

import (
	"context"
	"errors"
	"testing"
)

func TestForwardCoordinatorErrorSuppressesSignalCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	errCh := make(chan error, 1)

	forwardCoordinatorError(ctx, errCh, context.Canceled)

	select {
	case err := <-errCh:
		t.Fatalf("expected signal cancellation to be graceful, forwarded %v", err)
	default:
	}
}

func TestForwardCoordinatorErrorReportsUnexpectedCancellation(t *testing.T) {
	errCh := make(chan error, 1)

	forwardCoordinatorError(context.Background(), errCh, context.Canceled)

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	default:
		t.Fatal("expected unexpected coordinator cancellation to be forwarded")
	}
}

func TestForwardCoordinatorErrorReportsUnexpectedError(t *testing.T) {
	expected := errors.New("coordinator failed")
	errCh := make(chan error, 1)

	forwardCoordinatorError(context.Background(), errCh, expected)

	select {
	case err := <-errCh:
		if !errors.Is(err, expected) {
			t.Fatalf("expected %v, got %v", expected, err)
		}
	default:
		t.Fatal("expected coordinator error to be forwarded")
	}
}

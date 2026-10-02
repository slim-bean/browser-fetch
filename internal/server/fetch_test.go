package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/slim-bean/browser-fetch/internal/browser"
	"github.com/slim-bean/browser-fetch/internal/metrics"
)

func TestClassifyWorkerCancellationIsNotCallerTimeout(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		err    error
		code   string
		status int
	}{
		{"worker cancelled", context.Background(), &browser.NavError{Err: context.Canceled}, metrics.OutcomeUnavail, http.StatusServiceUnavailable},
		{"allocator cancelled", context.Background(), context.Canceled, metrics.OutcomeUnavail, http.StatusServiceUnavailable},
		{"caller cancelled", cancelled, context.Canceled, metrics.OutcomeTimeout, 499},
		{"wrapped caller cancellation", cancelled, &browser.NavError{Err: context.Canceled}, metrics.OutcomeTimeout, 499},
		{"navigation deadline", context.Background(), &browser.NavError{Err: context.DeadlineExceeded}, metrics.OutcomeTimeout, http.StatusGatewayTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, status, _ := classify(tc.err, tc.ctx)
			if code != tc.code || status != tc.status {
				t.Fatalf("classify = (%s, %d), want (%s, %d)", code, status, tc.code, tc.status)
			}
		})
	}
}

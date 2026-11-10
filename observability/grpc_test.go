package observability

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type selectedStatusCause struct {
	cause error
	code  codes.Code
}

func (e *selectedStatusCause) Error() string { return "selected response" }
func (e *selectedStatusCause) Unwrap() error { return e.cause }
func (e *selectedStatusCause) GRPCStatus() *status.Status {
	return status.New(e.code, "selected response")
}

// Cause retention must not make request diagnostics disagree with the actual RPC status.
func TestGRPCDiagnosticsPreferSelectedStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code codes.Code
		kind string
	}{
		{"peer deadline", &selectedStatusCause{context.DeadlineExceeded, codes.Unavailable}, codes.Unavailable, "unavailable"},
		{"peer canceled", &selectedStatusCause{context.Canceled, codes.Internal}, codes.Internal, "internal"},
		{"explicit unknown", &selectedStatusCause{context.DeadlineExceeded, codes.Unknown}, codes.Unknown, "internal"},
		{"wrapped status", fmt.Errorf("outer: %w", &selectedStatusCause{context.DeadlineExceeded, codes.Aborted}), codes.Aborted, "conflict"},
		{"bare canceled", context.Canceled, codes.Canceled, "canceled"},
		{"wrapped deadline", fmt.Errorf("caller: %w", context.DeadlineExceeded), codes.DeadlineExceeded, "deadline_exceeded"},
		{"plain error", errors.New("plain failure"), codes.Unknown, "internal"},
		{"success", nil, codes.OK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := grpcCode(tc.err); got != tc.code {
				t.Errorf("logged code=%s want %s", got, tc.code)
			}
			if got := grpcErrorKind(tc.err); got != tc.kind {
				t.Errorf("logged kind=%s want %s", got, tc.kind)
			}
		})
	}
}

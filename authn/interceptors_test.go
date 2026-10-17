package authn

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type verifierFunc func(context.Context) (VerifiedCaller, error)

func (f verifierFunc) Verify(ctx context.Context) (VerifiedCaller, error) { return f(ctx) }

var testIdentity = api.CallerIdentity{Issuer: "https://issuer.example/Exact", Subject: " User "}

func TestConstruction(t *testing.T) {
	for _, mode := range []Mode{"", "unknown", Required} {
		if _, _, err := NewInterceptors(mode, nil); err == nil {
			t.Fatalf("mode %q accepted without a verifier", mode)
		}
	}
	if _, ok := Caller(context.Background()); ok {
		t.Fatal("context without verification has a caller")
	}
}

func TestVerification(t *testing.T) {
	valid := VerifiedCaller{Identity: testIdentity, ValidUntil: time.Now().Add(time.Hour)}
	for _, tc := range []struct {
		name   string
		caller VerifiedCaller
		err    error
		code   codes.Code
	}{
		{"exact identity", valid, nil, codes.OK},
		{"empty issuer", VerifiedCaller{Identity: api.CallerIdentity{Subject: "s"}, ValidUntil: valid.ValidUntil}, nil, codes.Unauthenticated},
		{"empty subject", VerifiedCaller{Identity: api.CallerIdentity{Issuer: "i"}, ValidUntil: valid.ValidUntil}, nil, codes.Unauthenticated},
		{"zero expiry", VerifiedCaller{Identity: testIdentity}, nil, codes.Unauthenticated},
		{"expired", VerifiedCaller{Identity: testIdentity, ValidUntil: time.Now().Add(-time.Second)}, nil, codes.Unauthenticated},
		{"verifier error", valid, errors.New("private credential detail"), codes.Unauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, s, err := NewInterceptors(Required, verifierFunc(func(context.Context) (VerifiedCaller, error) { return tc.caller, tc.err }))
			if err != nil {
				t.Fatal(err)
			}
			check := func(ctx context.Context) {
				if tc.code != codes.OK {
					t.Fatal("rejected caller reached handler")
				}
				if got, ok := Caller(ctx); !ok || got != testIdentity {
					t.Fatal("exact verified identity not propagated")
				}
			}
			_, err = u(context.Background(), nil, nil, func(ctx context.Context, _ any) (any, error) { check(ctx); return nil, nil })
			assertCode(t, err, tc.code)
			err = s(nil, &testStream{ctx: context.Background()}, nil, func(_ any, ss grpc.ServerStream) error { check(ss.Context()); return nil })
			assertCode(t, err, tc.code)
		})
	}
}

func assertCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if status.Code(err) != want {
		t.Fatalf("status = %v, want %v", status.Code(err), want)
	}
	if want == codes.Unauthenticated && status.Convert(err).Message() != "authentication required" {
		t.Fatal("authentication error is not generic")
	}
}

type testStream struct {
	grpc.ServerStream
	ctx  context.Context
	recv func() error
}

func (s *testStream) Context() context.Context { return s.ctx }
func (s *testStream) RecvMsg(any) error        { return s.recv() }

func TestOff(t *testing.T) {
	u, s, err := NewInterceptors(Off, verifierFunc(func(context.Context) (VerifiedCaller, error) {
		t.Fatal("off mode invoked verifier")
		return VerifiedCaller{}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	check := func(ctx context.Context) {
		if _, ok := Caller(ctx); ok {
			t.Fatal("off mode installed an identity")
		}
	}
	_, err = u(context.Background(), nil, nil, func(ctx context.Context, _ any) (any, error) { check(ctx); return nil, nil })
	assertCode(t, err, codes.OK)
	err = s(nil, &testStream{ctx: context.Background()}, nil, func(_ any, ss grpc.ServerStream) error { check(ss.Context()); return nil })
	assertCode(t, err, codes.OK)
}

func TestStreamLifetime(t *testing.T) {
	for _, kind := range []string{"expiry", "caller deadline", "caller cancellation", "caller status cause"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx, cancelCause := context.WithCancelCause(ctx)
			defer cancelCause(nil)
			expiry := time.Now().Add(time.Hour)
			want := codes.Canceled
			if kind == "caller deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 10*time.Millisecond)
				defer stop()
				want = codes.DeadlineExceeded
			}
			if kind == "expiry" {
				expiry = time.Now().Add(10 * time.Millisecond)
				want = codes.Unauthenticated
			}
			_, s, err := NewInterceptors(Required, verifierFunc(func(context.Context) (VerifiedCaller, error) { return VerifiedCaller{testIdentity, expiry}, nil }))
			if err != nil {
				t.Fatal(err)
			}
			err = s(nil, &testStream{ctx: ctx}, nil, func(_ any, ss grpc.ServerStream) error {
				deadline, ok := ss.Context().Deadline()
				if !ok || deadline.After(expiry) {
					t.Fatal("stream deadline exceeds verified expiry")
				}
				if kind == "caller cancellation" {
					cancel()
				}
				if kind == "caller status cause" {
					cancelCause(status.Error(codes.Unauthenticated, "authentication required"))
				}
				<-ss.Context().Done()
				// Expired operations must not reach the underlying stream.
				assertCode(t, ss.SendMsg(nil), want)
				assertCode(t, ss.RecvMsg(nil), want)
				return nil // Cancellation must win even if the handler returns success.
			})
			assertCode(t, err, want)
		})
	}
}

func TestReceiveReturningAfterExpiry(t *testing.T) {
	_, s, err := NewInterceptors(Required, verifierFunc(func(context.Context) (VerifiedCaller, error) {
		return VerifiedCaller{testIdentity, time.Now().Add(10 * time.Millisecond)}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	inner := &testStream{ctx: context.Background()}
	err = s(nil, inner, nil, func(_ any, ss grpc.ServerStream) error {
		inner.recv = func() error { <-ss.Context().Done(); return nil }
		assertCode(t, ss.RecvMsg(nil), codes.Unauthenticated)
		return nil
	})
	assertCode(t, err, codes.Unauthenticated)
}

func TestUnaryCancellationDuringHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u, _, err := NewInterceptors(Required, verifierFunc(func(context.Context) (VerifiedCaller, error) {
		return VerifiedCaller{testIdentity, time.Now().Add(time.Hour)}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = u(ctx, nil, nil, func(context.Context, any) (any, error) { cancel(); return nil, nil })
	assertCode(t, err, codes.Canceled)
}

func TestCanceledVerification(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	u, s, err := NewInterceptors(Required, verifierFunc(func(context.Context) (VerifiedCaller, error) { return VerifiedCaller{}, errors.New("private") }))
	if err != nil {
		t.Fatal(err)
	}
	_, err = u(ctx, nil, nil, func(context.Context, any) (any, error) { t.Fatal("canceled call reached handler"); return nil, nil })
	assertCode(t, err, codes.Canceled)
	err = s(nil, &testStream{ctx: ctx}, nil, func(any, grpc.ServerStream) error { t.Fatal("canceled stream reached handler"); return nil })
	assertCode(t, err, codes.Canceled)
}

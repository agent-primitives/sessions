package main

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harnesswire"
)

func TestMaxRecvMsgSize(t *testing.T) {
	for _, tt := range []struct {
		name, value string
		want        int
		wantErr     bool
	}{
		{name: "unset keeps the gRPC default", value: "", want: 0},
		{name: "16 MiB", value: "16777216", want: 16 << 20},
		{name: "below the default", value: "1048576", want: 1 << 20},
		{name: "zero", value: "0", wantErr: true},
		{name: "negative", value: "-1", wantErr: true},
		{name: "not a number", value: "abc", wantErr: true},
		{name: "unit suffix", value: "16MiB", wantErr: true},
		{name: "leading space", value: " 1", wantErr: true},
		{name: "trailing newline", value: "1\n", wantErr: true},
		{name: "overflow", value: "99999999999999999999", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HARNESS_MAX_RECV_MSG_SIZE", tt.value)
			got, err := maxRecvMsgSize()
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "HARNESS_MAX_RECV_MSG_SIZE") {
					t.Fatalf("maxRecvMsgSize() = %d, %v; want an error naming HARNESS_MAX_RECV_MSG_SIZE", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("maxRecvMsgSize() = %d, %v; want %d", got, err, tt.want)
			}
		})
	}
}

// Start carries the session's whole prior journal in one frame, so a long history is limited by
// the server's receive limit. A Start over the limit is refused before the harness runs.
func TestStartHistoryReceiveLimit(t *testing.T) {
	const threeMiB, fiveMiB = 3 << 20, 5 << 20
	for _, tt := range []struct {
		name    string
		setting string
		history int
		want    codes.Code
	}{
		{"default accepts 3 MiB", "", threeMiB, codes.OK},
		{"default refuses 5 MiB", "", fiveMiB, codes.ResourceExhausted},
		{"16 MiB setting accepts 5 MiB", "16777216", fiveMiB, codes.OK},
		{"1 MiB setting refuses 3 MiB", "1048576", threeMiB, codes.ResourceExhausted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HARNESS_MAX_RECV_MSG_SIZE", tt.setting)
			limit, err := maxRecvMsgSize()
			if err != nil {
				t.Fatal(err)
			}
			h := &countingHarness{}
			client := dialServer(t, newServer(h, limit))

			start := &api.Start{ExecutionID: "e-1", SessionUID: "s", History: history(tt.history)}
			err = client.Run(t.Context(), start, discardSink{})
			if got := status.Code(err); got != tt.want {
				t.Fatalf("Run = %v (code %s), want %s", err, got, tt.want)
			}
			wantRuns := int32(0)
			if tt.want == codes.OK {
				wantRuns = 1
			}
			if got := h.runs.Load(); got != wantRuns {
				t.Fatalf("harness ran %d times, want %d", got, wantRuns)
			}
		})
	}
}

// history returns OUTPUT events of 100 KiB each whose text adds up to about size bytes, the shape
// of a long transcript.
func history(size int) []api.Event {
	const each = 100 << 10
	text := strings.Repeat("x", each)
	events := make([]api.Event, size/each)
	for i := range events {
		events[i] = api.Event{
			ExecutionID: "prior",
			Kind:        api.EventOutput,
			Message:     api.TextMessage("assistant", text),
		}
	}
	return events
}

// countingHarness ends every turn at once and counts the turns that reached it.
type countingHarness struct{ runs atomic.Int32 }

func (*countingHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "count"}, nil
}

func (h *countingHarness) Run(context.Context, *api.Start, api.EventSink) error {
	h.runs.Add(1)
	return nil
}

// dialServer serves srv on an in-memory listener and returns a harnesswire client with gRPC's
// default dial options, so only the server's limit varies.
func dialServer(t *testing.T, srv *grpc.Server) api.Harness {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///harness",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return harnesswire.NewClientHarness(v1.NewHarnessClient(conn))
}

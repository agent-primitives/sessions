package observability

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

const marker = "SYNTHETIC-SECRET-7f3a"

// assertNoMarker fails if marker appears anywhere a span can carry text.
func assertNoMarker(t *testing.T, spans []sdktrace.ReadOnlySpan) {
	t.Helper()
	for _, s := range spans {
		check := func(where, v string) {
			if strings.Contains(v, marker) {
				t.Errorf("span %s leaked the marker in %s: %q", s.Name(), where, v)
			}
		}
		check("status", s.Status().Description)
		for _, a := range s.Attributes() {
			check(string(a.Key), a.Value.String())
		}
		for _, ev := range s.Events() {
			for _, a := range ev.Attributes {
				check(ev.Name+"."+string(a.Key), a.Value.String())
			}
		}
	}
}

type failingHealth struct {
	healthpb.UnimplementedHealthServer
}

func (failingHealth) Check(context.Context, *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	return nil, status.Error(codes.Internal, "payload "+marker)
}

func TestGRPCSpansDropStatusMessages(t *testing.T) {
	rec := recordSpans(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(ServerTracing())
	healthpb.RegisterHealthServer(srv, failingHealth{})
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()), ClientTracing())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{})
	if !strings.Contains(err.Error(), marker) {
		t.Fatalf("the caller's error must be unchanged, got %v", err)
	}
	srv.Stop()

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want client and server", len(spans))
	}
	for _, s := range spans {
		if s.Status().Code.String() != "Error" {
			t.Errorf("span %s status = %v, want Error", s.Name(), s.Status())
		}
	}
	assertNoMarker(t, spans)
}

func TestHTTPSpansStripURLCredentials(t *testing.T) {
	rec := recordSpans(t)
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ts.Close()
	client := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport,
		otelhttp.WithTracerProvider(ThirdPartyTracerProvider()))}

	u := strings.Replace(ts.URL, "http://", "http://user:"+marker+"@", 1) + "/v1/chat?api-key=" + marker + "#" + marker
	resp, err := client.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// A failed request records err.Error(), which quotes the URL.
	if _, err := client.Get("http://127.0.0.1:1/x?api-key=" + marker); err == nil {
		t.Fatal("expected a connection error")
	}

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	assertNoMarker(t, spans)
	var sawURL bool
	for _, a := range spans[0].Attributes() {
		if a.Key == "url.full" {
			sawURL = true
			if want := ts.URL + "/v1/chat"; a.Value.AsString() != want {
				t.Errorf("url.full = %q, want %q", a.Value.AsString(), want)
			}
		}
	}
	if !sawURL {
		t.Error("url.full was dropped instead of redacted")
	}
}

// panicInSpan ends span with a direct defer, the pattern the SDK's End recovers panics from.
func panicInSpan(tracer trace.Tracer, name string) {
	defer func() { _ = recover() }()
	_, span := tracer.Start(context.Background(), name)
	defer span.End()
	panic("panic " + marker)
}

func TestSpansDoNotRecordPanicValues(t *testing.T) {
	rec := recordSpans(t)
	panicInSpan(Tracer(), "own")
	panicInSpan(ThirdPartyTracerProvider().Tracer("third"), "third")
	_, span := ThirdPartyTracerProvider().Tracer("third").Start(context.Background(), "recorded")
	span.RecordError(errors.New("boom " + marker))
	span.End()

	spans := rec.Ended()
	if len(spans) != 3 {
		t.Fatalf("got %d spans, want 3", len(spans))
	}
	assertNoMarker(t, spans)
	ev := spans[2].Events()
	if len(ev) != 1 || ev[0].Name != "exception" {
		t.Fatalf("RecordError events = %+v, want one exception event", ev)
	}
}

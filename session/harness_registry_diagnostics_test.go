package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/observability"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func captureRegistryDiagnostics(t *testing.T) (*bytes.Buffer, *slog.Logger) {
	t.Helper()
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output, logger
}

func registryDiagnosticRecords(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("decode diagnostic: %v; log=%s", err, output)
		}
		if id, _ := record["request_id"].(string); id == "" {
			t.Errorf("diagnostic missing correlation: %v", record)
		}
		records = append(records, record)
	}
	if len(records) == 0 {
		t.Fatal("no operator diagnostic")
	}
	return records
}

// A direct, known store boundary retains its full safe cause, unlike arbitrary backend errors.
func TestHarnessRegistryStoreFailureDiagnostic(t *testing.T) {
	for _, stage := range []string{"lookup", "commit", "startup"} {
		t.Run(stage, func(t *testing.T) {
			output, _ := captureRegistryDiagnostics(t)
			factory := echoFactory
			var h *registryHost
			if stage == "commit" {
				factory = func(name string, spec *v1.HarnessSpec) (*placement.Placer, func(), error) {
					if err := h.store.Close(); err != nil {
						t.Fatal(err)
					}
					return echoFactory(name, spec)
				}
			}
			h = startRegistryHostWith(t, ":memory:", factory)
			if stage != "commit" {
				if err := h.store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			method := v1.HarnessRegistry_RegisterHarness_FullMethodName
			if stage == "startup" {
				method = "NewHarnessRegistry"
				b := local.New(sessionPolicyHarness{})
				t.Cleanup(func() { _ = b.Close() })
				r, err := placement.NewRegistry("base", map[string]*placement.Placer{"base": placement.New(b, nil)})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := session.NewHarnessRegistry(h.store, r, echoFactory); err == nil || !strings.Contains(err.Error(), "sql: database is closed") {
					t.Fatalf("startup error lost store cause: %v", err)
				}
			} else {
				ctx := observability.WithRequestID(t.Context(), "registry-store-request")
				_, err := h.reg.RegisterHarness(ctx, &v1.RegisterHarnessRequest{Name: "policy", Spec: remoteSpec("127.0.0.1:9000")})
				if status.Code(err) != codes.Internal || !strings.Contains(err.Error(), "sql: database is closed") {
					t.Fatalf("register error changed: %v", err)
				}
			}
			found := false
			for _, record := range registryDiagnosticRecords(t, output) {
				if record["method"] == method && record["error_kind"] == "internal" && record["error_category"] == "host" {
					found = true
					if cause, _ := record["error"].(string); !strings.Contains(cause, "sql: database is closed") {
						t.Errorf("safe store cause lost: %v", record)
					}
					if stage != "startup" && record["request_id"] != "registry-store-request" {
						t.Errorf("request correlation lost: %v", record)
					}
				}
			}
			if !found {
				t.Fatalf("no classified store diagnostic: %s", output)
			}
		})
	}
}

// The startup summary and the original Describe diagnostic must share a correlation ID. A
// factory's in-process error is no more trusted than a peer's, including wrapped store sentinels.
func TestHarnessRegistryPolicyFailureDiagnostics(t *testing.T) {
	const credential = "https://provider.invalid?api_key=sk-registry-private"
	for _, origin := range []string{"describe", "factory"} {
		for _, startup := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/startup=%t", origin, startup), func(t *testing.T) {
				output, logger := captureRegistryDiagnostics(t)
				var describeID string
				factory := func(_ string, spec *v1.HarnessSpec) (*placement.Placer, func(), error) {
					cause := fmt.Errorf("%s: %w", credential, sqlitelog.ErrHarnessNotFound)
					if origin == "factory" {
						return nil, nil, cause
					}
					b := local.New(registryDiagnosticHarness{err: cause, requestID: &describeID})
					return placement.New(b, nil, placement.WithDescriptorID(spec.GetDescriptorId()), placement.WithLogger(logger),
						placement.WithModelPolicy(api.ModelPolicy{Default: "m", Allowed: []string{"m"}})), func() { _ = b.Close() }, nil
				}
				h := startRegistryHost(t, ":memory:")
				ctx := observability.WithRequestID(t.Context(), "registry-policy-request")
				if startup {
					registerHarness(t, h, "policy", remoteSpec("127.0.0.1:9000"))
					b := local.New(sessionPolicyHarness{})
					t.Cleanup(func() { _ = b.Close() })
					r, err := placement.NewRegistry("base", map[string]*placement.Placer{"base": placement.New(b, nil)})
					if err != nil {
						t.Fatal(err)
					}
					reg, err := session.NewHarnessRegistry(h.store, r, factory)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(reg.Close)
					_, err = r.For("policy")
					if !errors.Is(err, placement.ErrHarnessUnservable) || (origin == "describe" && strings.Contains(err.Error(), credential)) {
						t.Fatalf("startup failed to retain unavailable route safely: %v", err)
					}
				} else {
					// Use a second registry on the same store to exercise the real direct-call service.
					b := local.New(sessionPolicyHarness{})
					t.Cleanup(func() { _ = b.Close() })
					r, err := placement.NewRegistry("base", map[string]*placement.Placer{"base": placement.New(b, nil)})
					if err != nil {
						t.Fatal(err)
					}
					reg, err := session.NewHarnessRegistry(h.store, r, factory)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(reg.Close)
					_, err = reg.RegisterHarness(ctx, &v1.RegisterHarnessRequest{Name: "policy", Spec: remoteSpec("127.0.0.1:9000")})
					want := codes.Unavailable
					if origin == "factory" {
						want = codes.FailedPrecondition
					}
					if status.Code(err) != want || (origin == "describe" && (!errors.Is(err, placement.ErrHarnessUnavailable) || strings.Contains(err.Error(), credential))) {
						t.Fatalf("public registration error changed: %v", err)
					}
					if _, err := h.store.Harness("policy"); !errors.Is(err, sqlitelog.ErrHarnessNotFound) {
						t.Fatalf("failed registration persisted: %v", err)
					}
				}
				if strings.Contains(output.String(), "sk-registry-private") || strings.Contains(output.String(), credential) {
					t.Fatalf("provider/factory credentials logged: %s", output)
				}
				method := v1.HarnessRegistry_RegisterHarness_FullMethodName
				if startup {
					method = "NewHarnessRegistry"
				}
				found := false
				var correlation string
				for _, record := range registryDiagnosticRecords(t, output) {
					if record["error_category"] != "external" || record["error_kind"] == "" || record["error"] != nil {
						t.Errorf("unsafe or unclassified external diagnostic: %v", record)
					}
					id, _ := record["request_id"].(string)
					if correlation != "" && correlation != id {
						t.Errorf("startup/request diagnostics not correlated: %s != %s", correlation, id)
					}
					correlation = id
					if !startup && id != "registry-policy-request" {
						t.Errorf("direct-call request ID changed: %v", record)
					}
					if record["method"] == method {
						found = true
					}
				}
				if !found || (origin == "describe" && describeID != correlation) {
					t.Fatalf("missing registry/Describe correlation: method=%s describeID=%s log=%s", method, describeID, output)
				}
			})
		}
	}
}

type registryDiagnosticHarness struct {
	err       error
	requestID *string
}

func (h registryDiagnosticHarness) Describe(ctx context.Context) (api.Descriptor, error) {
	*h.requestID = observability.RequestID(ctx)
	return api.Descriptor{}, h.err
}
func (registryDiagnosticHarness) Run(context.Context, *api.Start, api.EventSink) error { return nil }

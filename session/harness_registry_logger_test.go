package session_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	pb "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/observability"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Each RPC must use the embedder's logger, retaining safe host causes only in
// correlated diagnostics, never in its enforced response or the process logger.
func TestAuthorizationRegistryLogger(t *testing.T) {
	for _, mode := range []string{"custom", "default", "nil"} {
		for _, method := range []string{
			pb.HarnessRegistry_RegisterHarness_FullMethodName,
			pb.HarnessRegistry_GetHarness_FullMethodName,
			pb.HarnessRegistry_RetireHarness_FullMethodName,
			pb.HarnessRegistry_ListHarnesses_FullMethodName,
		} {
			t.Run(mode+"/"+method, func(t *testing.T) {
				var process, custom bytes.Buffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&process, nil)))
				t.Cleanup(func() { slog.SetDefault(previous) })
				store := openStore(t, ":memory:")
				r, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
				policy := policyFunc(func(context.Context, api.AuthorizationRequest) error { return nil })
				opts := []session.HarnessRegistryOption{session.WithRegistryAuthorization(policy)}
				switch mode {
				case "custom":
					opts = append(opts, session.WithRegistryLogger(slog.New(slog.NewJSONHandler(&custom, nil))))
				case "nil":
					opts = append(opts, session.WithRegistryLogger(nil))
				}
				h, err := session.NewHarnessRegistry(store, r, echoFactory, opts...)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(h.Close)
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				ctx := observability.WithRequestID(authorizationDiagnosticContext(t), "registry-request-190")
				switch method {
				case pb.HarnessRegistry_RegisterHarness_FullMethodName:
					_, err = h.RegisterHarness(ctx, &pb.RegisterHarnessRequest{Name: "registered", Spec: remoteSpec("127.0.0.1:1")})
				case pb.HarnessRegistry_GetHarness_FullMethodName:
					_, err = h.GetHarness(ctx, &pb.GetHarnessRequest{Name: "registered"})
				case pb.HarnessRegistry_RetireHarness_FullMethodName:
					_, err = h.RetireHarness(ctx, &pb.RetireHarnessRequest{Name: "registered"})
				case pb.HarnessRegistry_ListHarnesses_FullMethodName:
					_, err = h.ListHarnesses(ctx, &pb.ListHarnessesRequest{})
				}
				if status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal error" {
					t.Fatalf("RPC error = %v, want generic INTERNAL", err)
				}
				output := &process
				if mode == "custom" {
					output = &custom
					if process.Len() != 0 {
						t.Errorf("diagnostic escaped to process logger: %s", process.String())
					}
				}
				records := authorizationDiagnosticRecords(t, output)
				if len(records) != 1 {
					t.Fatalf("diagnostics = %s, want exactly one", output.String())
				}
				record := records[0]
				cause, _ := record["error"].(string)
				if record["request_id"] != "registry-request-190" || record["method"] != method || record["error_kind"] != "internal" || record["error_category"] != "host" || !strings.Contains(cause, "sql: database is closed") {
					t.Errorf("missing correlated host cause: %v", record)
				}
			})
		}
	}
}

func TestHarnessRegistryLoggerStartupWarning(t *testing.T) {
	store := openStore(t, ":memory:")
	if _, _, err := store.RegisterHarness(sqlitelog.HarnessRecord{Name: "unavailable", UID: "harness-unavailable", Spec: "{}", SpecDigest: "invalid"}); err != nil {
		t.Fatal(err)
	}
	r, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
	var output bytes.Buffer
	h, err := session.NewHarnessRegistry(store, r, func(string, *pb.HarnessSpec) (*placement.Placer, func(), error) {
		t.Fatal("invalid stored spec must not reach the factory")
		return nil, nil, nil
	}, session.WithRegistryLogger(slog.New(slog.NewJSONHandler(&output, nil))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	records := authorizationDiagnosticRecords(t, &output)
	if len(records) != 1 || records[0]["level"] != "WARN" || records[0]["harness"] != "unavailable" || records[0]["msg"] != "stored harness cannot be served on this host" || records[0]["error"] != nil || records[0]["request_id"] == "" || records[0]["method"] != "NewHarnessRegistry" || records[0]["error_category"] != "external" || records[0]["error_kind"] != "harness_unservable" {
		t.Errorf("missing sanitized correlated startup warning on embedder logger: %s", output.String())
	}
}

func TestHarnessRegistryLoggerStartupStoreFailure(t *testing.T) {
	var process, custom bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&process, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	store := openStore(t, ":memory:")
	r, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := session.NewHarnessRegistry(store, r, echoFactory,
		session.WithRegistryLogger(slog.New(slog.NewJSONHandler(&custom, nil))))
	if err == nil || !strings.Contains(err.Error(), "sql: database is closed") {
		t.Fatalf("startup lost store error: %v", err)
	}
	if process.Len() != 0 {
		t.Errorf("startup diagnostic escaped to process logger: %s", process.String())
	}
	records := authorizationDiagnosticRecords(t, &custom)
	if len(records) != 1 {
		t.Fatalf("startup diagnostics = %s, want exactly one", custom.String())
	}
	record := records[0]
	cause, _ := record["error"].(string)
	if record["request_id"] == "" || record["method"] != "NewHarnessRegistry" || record["error_kind"] != "internal" || record["error_category"] != "host" || !strings.Contains(cause, "sql: database is closed") {
		t.Errorf("missing correlated startup host cause: %v", record)
	}
}

func TestHarnessRegistryLoggerAdmission(t *testing.T) {
	const private = "https://provider.invalid?api_key=sk-registry-private"
	for _, enforced := range []bool{false, true} {
		for _, failure := range []struct {
			name  string
			code  codes.Code
			cause error
		}{
			{"describe", codes.Unavailable, placement.ErrHarnessUnavailable},
			{"model", codes.FailedPrecondition, api.ErrModelNotAllowed},
			{"nil placer", codes.Internal, nil},
		} {
			t.Run(fmt.Sprintf("%s/enforced=%t", failure.name, enforced), func(t *testing.T) {
				var process, custom bytes.Buffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&process, nil)))
				t.Cleanup(func() { slog.SetDefault(previous) })
				logger := slog.New(slog.NewJSONHandler(&custom, nil))
				store := openStore(t, ":memory:")
				r, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
				factory := func(_ string, spec *pb.HarnessSpec) (*placement.Placer, func(), error) {
					if failure.name == "nil placer" {
						return nil, nil, nil
					}
					harness := &policyDescriptorHarness{models: []string{"forbidden"}}
					if failure.name == "describe" {
						harness.describeErr = errors.New(private)
					}
					b := local.New(harness)
					return placement.New(b, nil, placement.WithLogger(logger), placement.WithDescriptorID(spec.GetDescriptorId()),
						placement.WithModelPolicy(api.ModelPolicy{Default: "allowed", Allowed: []string{"allowed"}})), func() { _ = b.Close() }, nil
				}
				opts := []session.HarnessRegistryOption{session.WithRegistryLogger(logger)}
				if enforced {
					opts = append(opts, session.WithRegistryAuthorization(policyFunc(func(context.Context, api.AuthorizationRequest) error { return nil })))
				}
				h, err := session.NewHarnessRegistry(store, r, factory, opts...)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(h.Close)
				ctx := observability.WithRequestID(authorizationDiagnosticContext(t), "registry-admission-request")
				spec := remoteSpec("127.0.0.1:9000")
				spec.DescriptorId = "expected"
				_, err = h.RegisterHarness(ctx, &pb.RegisterHarnessRequest{Name: "registered", Spec: spec})
				if status.Code(err) != failure.code || (failure.cause != nil && !errors.Is(err, failure.cause)) {
					t.Fatalf("admission error changed status/cause: %v; want %s / %v", err, failure.code, failure.cause)
				}
				if enforced && failure.code == codes.Internal && status.Convert(err).Message() != "internal error" {
					t.Errorf("enforced response not sanitized: %v", err)
				}
				if process.Len() != 0 {
					t.Errorf("admission diagnostic escaped to process logger: %s", process.String())
				}
				if strings.Contains(custom.String(), private) || strings.Contains(custom.String(), "sk-registry-private") {
					t.Errorf("admission diagnostic leaked backend message: %s", custom.String())
				}
				registryRecords := 0
				for _, record := range registryDiagnosticRecords(t, &custom) {
					if record["error"] != nil || record["error_category"] != "external" || record["request_id"] != "registry-admission-request" {
						t.Errorf("unsafe or uncorrelated admission diagnostic: %v", record)
					}
					if record["method"] == pb.HarnessRegistry_RegisterHarness_FullMethodName {
						registryRecords++
						if record["error_kind"] == "" {
							t.Errorf("unclassified registry diagnostic: %v", record)
						}
					}
				}
				if registryRecords != 1 {
					t.Errorf("registry admission diagnostics = %d, want exactly one: %s", registryRecords, custom.String())
				}
				if _, err := store.Harness("registered"); !errors.Is(err, sqlitelog.ErrHarnessNotFound) {
					t.Errorf("failed admission persisted immutable row: %v", err)
				}
			})
		}
	}
}

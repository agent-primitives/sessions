package session_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	pb "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/observability"
	"github.com/agent-primitives/sessions/placement"
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
	if len(records) != 1 || records[0]["level"] != "WARN" || records[0]["harness"] != "unavailable" || records[0]["msg"] != "stored harness cannot be served on this host" || records[0]["error"] == nil {
		t.Errorf("missing startup warning on embedder logger: %s", output.String())
	}
}

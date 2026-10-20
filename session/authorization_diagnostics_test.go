package session_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	pb "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/model/openai"
	"github.com/agent-primitives/sessions/observability"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Losing either the early owner-lookup diagnostic or the deferred INTERNAL
// diagnostic must not silently turn an operational failure into an opaque one.
func TestAuthorizationInternalDiagnostics(t *testing.T) {
	for _, path := range []string{"owner lookup", "post-policy get", "post-policy create", "post-policy list", "post-policy delete"} {
		for _, id := range []struct{ name, value string }{
			{"provided ID", "authz-request-190"},
			{"missing ID", ""},
			{"unsafe ID", "unsafe\nrequest"},
		} {
			t.Run(path+"/"+id.name, func(t *testing.T) {
				store := openStore(t, ":memory:")
				registry, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
				if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "owned", Harness: "gate"}, authOwner, false); err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				policyCalls := 0
				policy := policyFunc(func(context.Context, api.AuthorizationRequest) error {
					policyCalls++
					// Session-scoped calls have already read the owner; their next store access
					// (or the global operation's first store access) now fails.
					if err := store.Close(); err != nil {
						t.Fatal(err)
					}
					return nil
				})
				svc := session.NewService(store, registry, session.WithAuthorization(policy),
					session.WithLogger(slog.New(slog.NewJSONHandler(&output, nil))))
				ctx := authorizationDiagnosticContext(t)
				if id.value != "" {
					ctx = observability.WithRequestID(ctx, id.value)
				}
				if path == "owner lookup" {
					if err := store.Close(); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				switch path {
				case "post-policy create":
					_, err = svc.CreateSession(ctx, &pb.CreateSessionRequest{})
				case "post-policy list":
					_, err = svc.ListSessions(ctx, &pb.ListSessionsRequest{})
				case "post-policy delete":
					_, err = svc.DeleteSession(ctx, &pb.DeleteSessionRequest{Uid: "owned"})
				default:
					_, err = svc.GetSession(ctx, &pb.GetSessionRequest{Uid: "owned"})
				}
				if status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal error" {
					t.Fatalf("RPC error = %v, want generic INTERNAL", err)
				}
				wantCalls := 1
				if path == "owner lookup" {
					wantCalls = 0
				}
				if policyCalls != wantCalls {
					t.Fatalf("policy calls = %d, want %d", policyCalls, wantCalls)
				}
				records := authorizationDiagnosticRecords(t, &output)
				found := false
				requestID := ""
				for _, record := range records {
					gotID, _ := record["request_id"].(string)
					if gotID == "" || len(gotID) > 128 || strings.Trim(gotID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != "" {
						t.Errorf("missing or unsafe request_id: %v", record)
					}
					if id.name == "provided ID" && gotID != id.value {
						t.Errorf("request_id = %q, want %q", gotID, id.value)
					}
					if requestID != "" && gotID != requestID {
						t.Errorf("diagnostic and operation logs use different request IDs: %q, %q", requestID, gotID)
					}
					requestID = gotID
					diagnostic, _ := record["error"].(string)
					if strings.Contains(diagnostic, "sql: database is closed") {
						found = true
						wantMethod := pb.Sessions_GetSession_FullMethodName
						switch path {
						case "post-policy create":
							wantMethod = pb.Sessions_CreateSession_FullMethodName
						case "post-policy list":
							wantMethod = pb.Sessions_ListSessions_FullMethodName
						case "post-policy delete":
							wantMethod = pb.Sessions_DeleteSession_FullMethodName
						}
						if record["method"] != wantMethod || record["error_kind"] != "internal" || record["error_category"] != "host" {
							t.Errorf("host diagnostic lacks method/kind/category: %v", record)
						}
					}
				}
				if !found {
					t.Errorf("missing original database-closed error attribute in logs: %s", output.String())
				}
			})
		}
	}
}

// Exercise the real provider adapter and harness path, not just a synthetic RPC
// status. Neither credential-shaped nor ordinary provider text belongs in logs.
func TestAuthorizationProviderFailureDoesNotLogMessage(t *testing.T) {
	const secret = "sk-test-secret-190"
	const detail = "provider-private-diagnostic"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"` + detail + ` ` + secret + `"}}`))
	}))
	t.Cleanup(provider.Close)
	model, err := openai.New(openai.WithBaseURL(provider.URL), openai.WithModel("echo"))
	if err != nil {
		t.Fatal(err)
	}
	store := openStore(t, ":memory:")
	backend := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = backend.Close() })
	r, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": placement.New(backend, model.Model)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "owned", Harness: "echo"}, authOwner, false); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	svc := session.NewService(store, r, session.WithAuthorization(api.OwnerAuthorizer{}),
		session.WithLogger(slog.New(slog.NewJSONHandler(&output, nil))))
	ctx := observability.WithRequestID(callerContext(t, authOwner), "provider-request-190")
	err = svc.Exec(&pb.ExecRequest{Session: "owned"}, &rpcStream{ctx: ctx})
	if status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal error" {
		t.Fatalf("provider failure = %v, want generic INTERNAL", err)
	}
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), detail) {
		t.Errorf("provider message reached logs: %s", output.String())
	}
	found := false
	for _, record := range authorizationDiagnosticRecords(t, &output) {
		if record["error_category"] == "execution" {
			found = true
			if record["request_id"] != "provider-request-190" || record["method"] != pb.Sessions_Exec_FullMethodName || record["error_kind"] != "internal" {
				t.Errorf("external diagnostic lacks correlation/category: %v", record)
			}
			if _, ok := record["error"]; ok {
				t.Errorf("external diagnostic contains error text: %v", record)
			}
		}
	}
	if !found {
		t.Errorf("missing categorized provider diagnostic: %s", output.String())
	}
}

// A decode failure can quote caller-supplied metadata even though it was read
// from a store; it is not a trusted host diagnostic.
func TestAuthorizationCorruptMetadataDoesNotLogContent(t *testing.T) {
	const secret = "sk-test-secret-metadata-190"
	store := openStore(t, ":memory:")
	r, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
	if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "owned", Harness: "gate", Origin: `{"` + secret + `":true}`}, authOwner, false); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	svc := session.NewService(store, r, session.WithAuthorization(api.OwnerAuthorizer{}),
		session.WithLogger(slog.New(slog.NewJSONHandler(&output, nil))))
	_, err := svc.GetSession(callerContext(t, authOwner), &pb.GetSessionRequest{Uid: "owned"})
	if status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal error" {
		t.Fatalf("invalid metadata = %v, want generic INTERNAL", err)
	}
	if strings.Contains(output.String(), secret) {
		t.Errorf("stored metadata reached logs: %s", output.String())
	}
}

// The policy, not the service, owns logging its failures. A raw policy error may
// contain credentials and must not be mistaken for a trusted host diagnostic.
func TestAuthorizationPolicyErrorsAreNotDiagnostics(t *testing.T) {
	const private = "raw-policy-detail authorization=Bearer authz-bearer-secret cookie=session=authz-cookie-secret x-api-key=authz-api-key-secret"
	for _, failure := range []struct {
		name string
		err  error
		code codes.Code
	}{
		{"raw error", errors.New(private), codes.Internal},
		{"status error", status.Error(codes.Internal, private), codes.Internal},
		{"wrapped denial", fmt.Errorf("%s: %w", private, api.ErrPermissionDenied), codes.PermissionDenied},
	} {
		for _, method := range []string{"get", "create", "list"} {
			t.Run(failure.name+"/"+method, func(t *testing.T) {
				store := openStore(t, ":memory:")
				registry, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
				if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "owned", Harness: "gate"}, authOwner, false); err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				svc := session.NewService(store, registry,
					session.WithLogger(slog.New(slog.NewJSONHandler(&output, nil))),
					session.WithAuthorization(policyFunc(func(context.Context, api.AuthorizationRequest) error { return failure.err })))
				ctx := observability.WithRequestID(authorizationDiagnosticContext(t), "policy-request-190")
				var err error
				switch method {
				case "create":
					_, err = svc.CreateSession(ctx, &pb.CreateSessionRequest{})
				case "list":
					_, err = svc.ListSessions(ctx, &pb.ListSessionsRequest{})
				default:
					_, err = svc.GetSession(ctx, &pb.GetSessionRequest{Uid: "owned"})
				}
				wantMessage := "internal error"
				if failure.code == codes.PermissionDenied {
					wantMessage = "permission denied"
				}
				if status.Code(err) != failure.code || status.Convert(err).Message() != wantMessage {
					t.Errorf("RPC error = %v, want generic %v", err, failure.code)
				}
				for _, record := range authorizationDiagnosticRecords(t, &output) {
					if diagnostic, ok := record["error"]; ok {
						t.Errorf("service logged a policy failure as a host diagnostic: %v", diagnostic)
					}
				}
			})
		}
	}
}

func authorizationDiagnosticContext(t *testing.T) context.Context {
	t.Helper()
	return metadata.NewIncomingContext(callerContext(t, authOwner), metadata.Pairs(
		"authorization", "Bearer authz-bearer-secret",
		"cookie", "session=authz-cookie-secret",
		"x-api-key", "authz-api-key-secret",
	))
}

func authorizationDiagnosticRecords(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	for _, secret := range []string{"authz-bearer-secret", "authz-cookie-secret", "authz-api-key-secret", "raw-policy-detail"} {
		if strings.Contains(output.String(), secret) {
			t.Errorf("logs contain credential or raw policy error %q: %s", secret, output.String())
		}
	}
	var records []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(output.Bytes()))
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

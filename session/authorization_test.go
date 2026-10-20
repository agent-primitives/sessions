package session_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/agent-primitives/sessions/api"
	pb "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/authn"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

var authOwner = api.CallerIdentity{Issuer: "issuer", Subject: "owner"}

type policyFunc func(context.Context, api.AuthorizationRequest) error

func (f policyFunc) Authorize(ctx context.Context, r api.AuthorizationRequest) error {
	return f(ctx, r)
}

type callerVerifier struct{ caller api.CallerIdentity }

func (v callerVerifier) Verify(context.Context) (authn.VerifiedCaller, error) {
	return authn.VerifiedCaller{Identity: v.caller, ValidUntil: time.Now().Add(time.Hour)}, nil
}
func callerContext(t *testing.T, caller api.CallerIdentity) context.Context {
	t.Helper()
	u, _, err := authn.NewInterceptors(authn.Required, callerVerifier{caller})
	if err != nil {
		t.Fatal(err)
	}
	var ctx context.Context
	_, err = u(t.Context(), nil, &grpc.UnaryServerInfo{}, func(c context.Context, _ any) (any, error) { ctx = c; return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

type rpcStream struct {
	ctx  context.Context
	req  proto.Message
	sent []proto.Message
}

func (s *rpcStream) Context() context.Context   { return s.ctx }
func (*rpcStream) SetHeader(metadata.MD) error  { return nil }
func (*rpcStream) SendHeader(metadata.MD) error { return nil }
func (*rpcStream) SetTrailer(metadata.MD)       {}
func (s *rpcStream) SendMsg(m any) error {
	s.sent = append(s.sent, proto.Clone(m.(proto.Message)))
	return nil
}
func (s *rpcStream) RecvMsg(m any) error {
	if s.req == nil {
		return io.EOF
	}
	proto.Merge(m.(proto.Message), s.req)
	s.req = nil
	return nil
}
func (s *rpcStream) Send(m *pb.ExecUpdate) error { return s.SendMsg(m) }

// Removing any handler check, using request identity, or checking after effects fails this walker.
func TestAuthorizationEveryGeneratedRPC(t *testing.T) {
	store := openStore(t, ":memory:")
	r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
	if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "owned", Harness: "gate"}, authOwner, false); err != nil {
		t.Fatal(err)
	}
	var requests []api.AuthorizationRequest
	var failure error
	var wantActions []api.Action
	policy := policyFunc(func(_ context.Context, q api.AuthorizationRequest) error {
		requests = append(requests, q)
		if len(requests) == len(wantActions) {
			return failure
		}
		return nil
	})
	svc := session.NewService(store, r, session.WithAuthorization(policy))
	registry, err := session.NewHarnessRegistry(store, r, func(string, *pb.HarnessSpec) (*placement.Placer, func(), error) {
		t.Fatal("factory called before authorization")
		return nil, nil, nil
	}, session.WithRegistryAuthorization(policy))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registry.Close)
	ctx := callerContext(t, authOwner)
	canceled, cancel := context.WithCancelCause(ctx)
	cancel(errors.New("private detail"))
	expired, expire := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer expire()
	actions := map[string][]api.Action{
		"CreateSession": {api.ActionCreate}, "GetSession": {api.ActionRead}, "ListSessions": {api.ActionList}, "DeleteSession": {api.ActionDelete},
		"Exec": {api.ActionExec}, "Replay": {api.ActionRead}, "Approve": {api.ActionRead, api.ActionApprove}, "Cancel": {api.ActionCancel},
		"Suspend": {api.ActionSuspend}, "Resume": {api.ActionResume, api.ActionExec}, "Fork": {api.ActionFork, api.ActionCreate},
		"RegisterHarness": {api.ActionRegistryRegister}, "GetHarness": {api.ActionRegistryRead}, "ListHarnesses": {api.ActionRegistryList}, "RetireHarness": {api.ActionRegistryRetire},
	}
	for _, service := range []struct {
		desc *grpc.ServiceDesc
		impl any
	}{{&pb.Sessions_ServiceDesc, svc}, {&pb.HarnessRegistry_ServiceDesc, registry}} {
		descriptor, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(service.desc.ServiceName))
		if err != nil {
			t.Fatal(err)
		}
		run := func(name string, invoke func(context.Context, proto.Message, *rpcStream) error) {
			t.Run(name, func(t *testing.T) {
				want, ok := actions[name]
				if !ok {
					t.Fatalf("new RPC needs action coverage: %s", name)
				}
				req := dynamicpb.NewMessage(descriptor.(protoreflect.ServiceDescriptor).Methods().ByName(protoreflect.Name(name)).Input())
				for _, field := range []protoreflect.Name{"uid", "session"} {
					if f := req.Descriptor().Fields().ByName(field); f != nil && f.Kind() == protoreflect.StringKind {
						req.Set(f, protoreflect.ValueOfString("owned"))
					}
				}
				for _, tc := range []struct {
					name string
					ctx  context.Context
					err  error
					code codes.Code
				}{
					{"no caller", t.Context(), nil, codes.Unauthenticated},
					{"denied", ctx, fmt.Errorf("private detail: %w", api.ErrPermissionDenied), codes.PermissionDenied},
					{"policy failure", ctx, errors.New("private detail"), codes.Internal},
					{"status is not denial", ctx, status.Error(codes.PermissionDenied, "private detail"), codes.Internal},
					{"policy cancel", ctx, context.Canceled, codes.Internal},
					{"policy deadline", ctx, context.DeadlineExceeded, codes.Internal},
					{"wrapped policy cancel", ctx, fmt.Errorf("private detail: %w", context.Canceled), codes.Internal},
					{"wrapped policy deadline", ctx, fmt.Errorf("private detail: %w", context.DeadlineExceeded), codes.Internal},
					{"caller cancel", canceled, nil, codes.Canceled},
					{"caller deadline", expired, nil, codes.DeadlineExceeded},
				} {
					t.Run(tc.name, func(t *testing.T) {
						wantActions = want
						requests = nil
						failure = tc.err
						stream := &rpcStream{ctx: tc.ctx}
						err := invoke(tc.ctx, req, stream)
						if status.Code(err) != tc.code {
							t.Fatalf("code=%v want %v: %v", status.Code(err), tc.code, err)
						}
						if err != nil && strings.Contains(err.Error(), "private detail") {
							t.Fatalf("unsanitized: %v", err)
						}
						if len(stream.sent) != 0 || b.ioCounts() != ([4]int32{}) {
							t.Fatal("denied call sent a frame or touched runtime")
						}
						head, _ := store.Session("owned").Head()
						if head != 0 {
							t.Fatalf("denied call changed head: %d", head)
						}
						if tc.code == codes.Unauthenticated || tc.ctx.Err() != nil {
							if len(requests) != 0 {
								t.Fatal("unauthenticated policy call")
							}
							return
						}
						if len(requests) != len(want) {
							t.Fatalf("actions=%v want %v", requests, want)
						}
						for i, q := range requests {
							assertAuthorizationMethod(t, q, "/"+service.desc.ServiceName+"/"+name)
							if q.Caller != authOwner || q.Action != want[i] {
								t.Fatalf("request=%+v", q)
							}
							global := q.Action == api.ActionCreate || q.Action == api.ActionList || strings.HasPrefix(string(q.Action), "registry.")
							if global {
								if q.Owner.Valid() || q.SessionUID != "" {
									t.Fatalf("global resource: %+v", q)
								}
							} else if q.Owner != authOwner || q.SessionUID != "owned" {
								t.Fatalf("untrusted resource: %+v", q)
							}
						}
					})
				}
				proto.Reset(req)
				stream := &rpcStream{ctx: t.Context()}
				if err := invoke(t.Context(), req, stream); status.Code(err) != codes.Unauthenticated {
					t.Fatalf("zero request bypassed caller check: %v", err)
				}
			})
		}
		for _, m := range service.desc.Methods {
			run(m.MethodName, func(ctx context.Context, req proto.Message, _ *rpcStream) error {
				_, err := m.Handler(service.impl, ctx, func(dest any) error { proto.Merge(dest.(proto.Message), req); return nil }, nil)
				return err
			})
		}
		for _, m := range service.desc.Streams {
			run(m.StreamName, func(ctx context.Context, req proto.Message, s *rpcStream) error {
				s.req = req
				return m.Handler(service.impl, s)
			})
		}
	}
}

func TestAuthorizationCallerEndsDuringPolicy(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		for _, policyFailure := range []bool{false, true} {
			t.Run(fmt.Sprintf("deadline=%t/policy_failure=%t", deadline, policyFailure), func(t *testing.T) {
				store := openStore(t, ":memory:")
				r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
				if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "owned", Harness: "gate"}, authOwner, false); err != nil {
					t.Fatal(err)
				}
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(callerContext(t, authOwner))
					defer cancel()
					want := codes.Canceled
					if deadline {
						var expire context.CancelFunc
						ctx, expire = context.WithTimeout(ctx, time.Hour)
						defer expire()
						want = codes.DeadlineExceeded
					}
					called := false
					svc := session.NewService(store, r, session.WithAuthorization(policyFunc(func(ctx context.Context, _ api.AuthorizationRequest) error {
						called = true
						if deadline {
							<-ctx.Done() // Virtual time advances only after policy admission.
						} else {
							cancel()
						}
						if policyFailure {
							return fmt.Errorf("private detail: %w", ctx.Err())
						}
						return nil
					})))
					_, err := svc.GetSession(ctx, &pb.GetSessionRequest{Uid: "owned"})
					if !called || status.Code(err) != want || strings.Contains(err.Error(), "private detail") {
						t.Fatalf("policy called=%t, GetSession=%v, want %v without private detail", called, err, want)
					}
					head, headErr := store.Session("owned").Head()
					if headErr != nil || head != 0 || b.ioCounts() != ([4]int32{}) {
						t.Fatalf("canceled policy had effects: head=%d err=%v", head, headErr)
					}
				})
			})
		}
	}
}

func TestAuthorizationResourceHidingAndStoreFailure(t *testing.T) {
	store := openStore(t, ":memory:")
	r, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
	if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "foreign", Harness: "gate"}, api.CallerIdentity{Issuer: "other", Subject: "owner"}, false); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSession(sqlitelog.SessionMeta{UID: "unbound", Harness: "gate"}); err != nil {
		t.Fatal(err)
	}
	svc := session.NewService(store, r, session.WithAuthorization(api.OwnerAuthorizer{}))
	ctx := callerContext(t, authOwner)
	for _, uid := range []string{"unknown", "foreign", "unbound"} {
		_, err := svc.GetSession(ctx, &pb.GetSessionRequest{Uid: uid})
		if status.Code(err) != codes.PermissionDenied || status.Convert(err).Message() != "permission denied" {
			t.Fatalf("%s: %v", uid, err)
		}
		stream := &replayAuthStream{rpcStream: rpcStream{ctx: ctx}}
		err = svc.Replay(&pb.ReplayRequest{Session: uid}, stream)
		if status.Code(err) != codes.PermissionDenied || len(stream.sent) != 0 {
			t.Fatalf("replay %s: %v", uid, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetSession(ctx, &pb.GetSessionRequest{Uid: "unknown"}); status.Code(err) != codes.Internal || status.Convert(err).Message() != "internal error" {
		t.Fatalf("store failure=%v", err)
	}
}

type replayAuthStream struct{ rpcStream }

func (s *replayAuthStream) Send(m *pb.LogRecord) error { return s.SendMsg(m) }

// Deletion admission precedes owner lookup only for mutations. A verified foreign
// caller who knows the UID can observe contention, but never gain mutation authority.
func TestAuthorizationDeletionAdmissionDisclosure(t *testing.T) {
	store := openStore(t, ":memory:")
	r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
	if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "owned", Harness: "gate"}, authOwner, false); err != nil {
		t.Fatal(err)
	}
	svc := session.NewService(store, r, session.WithAuthorization(api.OwnerAuthorizer{}))
	foreign := callerContext(t, api.CallerIdentity{Issuer: "issuer", Subject: "foreign"})
	request := &pb.DeleteSessionRequest{Uid: "owned"}
	for _, busy := range []bool{false, true, false} {
		check := func() error {
			want := codes.PermissionDenied
			if busy {
				want = codes.Aborted
			}
			_, err := svc.DeleteSession(foreign, request)
			if status.Code(err) != want {
				t.Errorf("foreign Delete, busy=%v: %v, want %v", busy, err, want)
			}
			if busy && !errors.Is(err, placement.ErrSessionBusy) {
				t.Errorf("deletion contention lost busy cause: %v", err)
			}
			if _, err := svc.DeleteSession(t.Context(), request); status.Code(err) != codes.Unauthenticated {
				t.Errorf("unverified Delete: %v", err)
			}
			if _, err := svc.GetSession(foreign, &pb.GetSessionRequest{Uid: "owned"}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("foreign audit read: %v", err)
			}
			return nil
		}
		if busy {
			if err := r.WithSessionGuard("owned", check); err != nil {
				t.Fatal(err)
			}
		} else {
			_ = check()
		}
	}
	info, err := store.SessionInfo("owned")
	if err != nil || info.DeletionState != api.DeletionActive || info.LastSeq != 0 || b.ioCounts() != ([4]int32{}) {
		t.Fatalf("foreign refusal changed state or performed IO: %+v %v", info, err)
	}
}

func TestAuthorizationOwnedCreationAndPaging(t *testing.T) {
	store := openStore(t, ":memory:")
	r, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
	policy := api.OwnerAllowlistAuthorizer{Allow: map[api.Action][]api.CallerIdentity{api.ActionCreate: {authOwner}, api.ActionList: {authOwner}}}
	svc := session.NewService(store, r, session.WithAuthorization(policy))
	ctx := callerContext(t, authOwner)
	for i := 0; i < 3; i++ {
		if _, err := svc.CreateSession(ctx, &pb.CreateSessionRequest{}); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: fmt.Sprintf("foreign-%d", i)}, api.CallerIdentity{Issuer: "elsewhere", Subject: "owner"}, false); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	token := ""
	for {
		page, err := svc.ListSessions(ctx, &pb.ListSessionsRequest{PageSize: 1, PageToken: token})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Sessions) != 1 {
			t.Fatalf("underfilled page: %v", page)
		}
		uid := page.Sessions[0].GetMetadata().GetUid()
		info, err := store.SessionInfo(uid)
		if err != nil || info.Owner != authOwner || seen[uid] {
			t.Fatalf("wrong/repeated owner: %+v %v", info, err)
		}
		seen[uid] = true
		token = page.GetNextPageToken()
		if token == "" {
			break
		}
	}
	if len(seen) != 3 {
		t.Fatalf("listed %d", len(seen))
	}
	stream := &rpcStream{ctx: ctx}
	if err := svc.Exec(&pb.ExecRequest{}, stream); err != nil {
		t.Fatal(err)
	}
	uid := stream.sent[0].(*pb.ExecUpdate).GetSession().GetMetadata().GetUid()
	info, err := store.SessionInfo(uid)
	if err != nil || info.Owner != authOwner || info.LastSeq == 0 {
		t.Fatalf("auto-created=%+v %v", info, err)
	}
}

// Recovery is an implicit Resume, including the pre-request repair path. Exec alone must not admit it.
func TestAuthorizationApprovalIsIndependentAndActorIsUntrusted(t *testing.T) {
	store := openStore(t, ":memory:")
	r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
	if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "owned", Harness: "gate"}, authOwner, false); err != nil {
		t.Fatal(err)
	}
	seedPublicGate(t, store.Session("owned"), "gate", api.EventApprovalRequest)
	q := publicDecision("owned")
	q.Identity = &pb.IdentityRef{Issuer: "forged", Subject: "approver"}
	ctx := callerContext(t, authOwner)
	before := routingRecords(t, store.Session("owned"))
	ownerOnly := session.NewService(store, r, session.WithAuthorization(api.OwnerAuthorizer{}))
	if _, err := ownerOnly.Approve(ctx, q); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("owner alone approved: %v", err)
	}
	other := api.CallerIdentity{Issuer: "issuer", Subject: "approver"}
	policy := api.OwnerAllowlistAuthorizer{Allow: map[api.Action][]api.CallerIdentity{api.ActionApprove: {authOwner, other}}}
	svc := session.NewService(store, r, session.WithAuthorization(policy))
	if _, err := svc.Approve(callerContext(t, other), q); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("non-owner approved: %v", err)
	}
	if !reflect.DeepEqual(before, routingRecords(t, store.Session("owned"))) {
		t.Fatal("denial mutated approval")
	}
	decision, err := svc.Approve(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if actor := decision.GetDecision().GetEvent().GetActor(); actor.GetIssuer() != "" || actor.GetSubject() != "" || actor.GetPrincipal() != "" {
		t.Fatalf("forged audit actor: %v", actor)
	}
	q.Identity = &pb.IdentityRef{Issuer: "different", Subject: "forged"}
	retry, err := svc.Approve(ctx, q)
	if err != nil || !proto.Equal(retry.GetDecision(), decision.GetDecision()) {
		t.Fatalf("normalized retry: %v %v", retry, err)
	}
	if b.ioCounts() != ([4]int32{}) {
		t.Fatal("approval touched runtime")
	}
}

func TestAuthorizationAutoCreateChecksBothBeforeInsert(t *testing.T) {
	for _, deny := range []api.Action{api.ActionCreate, api.ActionExec} {
		t.Run(string(deny), func(t *testing.T) {
			store := openStore(t, ":memory:")
			r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
			svc := session.NewService(store, r, session.WithAuthorization(policyFunc(func(_ context.Context, q api.AuthorizationRequest) error {
				assertAuthorizationMethod(t, q, pb.Sessions_Exec_FullMethodName)
				if q.Action == deny {
					return api.ErrPermissionDenied
				}
				return nil
			})))
			stream := &rpcStream{ctx: callerContext(t, authOwner)}
			if err := svc.Exec(&pb.ExecRequest{}, stream); status.Code(err) != codes.PermissionDenied {
				t.Fatalf("auto-create: %v", err)
			}
			page, err := store.ListSessions(sqlitelog.ListOptions{})
			if err != nil || len(page.Sessions) != 0 || len(stream.sent) != 0 || b.ioCounts() != ([4]int32{}) {
				t.Fatalf("denied creation left effects: %v %v", page, err)
			}
		})
	}
}

func TestAuthorizationOwnedCreationKeepsHarnessPreconditions(t *testing.T) {
	for _, retired := range []bool{false, true} {
		t.Run(fmt.Sprintf("retired=%v", retired), func(t *testing.T) {
			store := openStore(t, ":memory:")
			r, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
			policy := api.OwnerAllowlistAuthorizer{Allow: map[api.Action][]api.CallerIdentity{api.ActionCreate: {authOwner}, api.ActionRegistryRegister: {authOwner}, api.ActionRegistryRetire: {authOwner}}}
			name := "gate"
			if retired {
				h, err := session.NewHarnessRegistry(store, r, echoFactory, session.WithRegistryAuthorization(policy))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(h.Close)
				name = "registered"
				ctx := callerContext(t, authOwner)
				if _, err := h.RegisterHarness(ctx, &pb.RegisterHarnessRequest{Name: name, Spec: remoteSpec("127.0.0.1:1")}); err != nil {
					t.Fatal(err)
				}
				if _, err := h.RetireHarness(ctx, &pb.RetireHarnessRequest{Name: name}); err != nil {
					t.Fatal(err)
				}
			} else if _, _, err := store.RegisterHarness(sqlitelog.HarnessRecord{Name: name, UID: "collision", Spec: "{}", SpecDigest: "collision-digest"}); err != nil {
				t.Fatal(err)
			}
			svc := session.NewService(store, r, session.WithAuthorization(policy))
			if _, err := svc.CreateSession(callerContext(t, authOwner), &pb.CreateSessionRequest{Session: &pb.Session{Harness: name}}); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("harness precondition: %v", err)
			}
			page, err := store.ListSessions(sqlitelog.ListOptions{})
			if err != nil || len(page.Sessions) != 0 {
				t.Fatalf("failed create left row: %v %v", page, err)
			}
		})
	}
}

func TestAuthorizationRegistryObservationAndForkRefusal(t *testing.T) {
	store := openStore(t, ":memory:")
	r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
	policy := api.OwnerAllowlistAuthorizer{Allow: map[api.Action][]api.CallerIdentity{api.ActionRegistryRead: {authOwner}, api.ActionRegistryList: {authOwner}, api.ActionCreate: {authOwner}}}
	h, err := session.NewHarnessRegistry(store, r, echoFactory, session.WithRegistryAuthorization(policy))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	ctx := callerContext(t, authOwner)
	if _, err := h.GetHarness(callerContext(t, api.CallerIdentity{Issuer: "foreign", Subject: "owner"}), &pb.GetHarnessRequest{Name: "gate", Observe: true}); status.Code(err) != codes.PermissionDenied || b.ioCounts() != ([4]int32{}) {
		t.Fatalf("denied observed read: %v", err)
	}
	if _, err := h.GetHarness(ctx, &pb.GetHarnessRequest{Name: "gate"}); err != nil || b.ioCounts() != ([4]int32{}) {
		t.Fatalf("stored read: %v", err)
	}
	if page, err := h.ListHarnesses(ctx, &pb.ListHarnessesRequest{}); err != nil || len(page.Harnesses) != 1 {
		t.Fatalf("registry list: %v %v", page, err)
	}
	if got, err := h.GetHarness(ctx, &pb.GetHarnessRequest{Name: "gate", Observe: true}); err != nil || got.GetObserved() == nil || b.describes.Load() != 1 {
		t.Fatalf("observed read: %v %v", got, err)
	}
	if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "owned", Harness: "gate"}, authOwner, false); err != nil {
		t.Fatal(err)
	}
	svc := session.NewService(store, r, session.WithAuthorization(policy))
	counts := b.ioCounts()
	if _, err := svc.Fork(ctx, &pb.ForkRequest{Session: "owned"}); status.Code(err) != codes.FailedPrecondition || b.ioCounts() != counts {
		t.Fatalf("owned fork not closed before IO: %v", err)
	}
	if _, err := svc.Cancel(ctx, &pb.CancelRequest{Session: "owned"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("cancel: %v", err)
	}
	_, want := (&pb.UnimplementedSessionsServer{}).Cancel(ctx, &pb.CancelRequest{})
	_, got := session.NewService(store, r).Cancel(ctx, &pb.CancelRequest{})
	if got.Error() != want.Error() {
		t.Fatalf("off Cancel changed: %v", got)
	}
	if _, err := session.NewService(store, r, session.WithAuthorization(nil)).GetSession(ctx, &pb.GetSessionRequest{Uid: "owned"}); status.Code(err) != codes.Internal {
		t.Fatalf("nil policy fell open: %v", err)
	}
}

// Recovery policy evaluation must not hold deletion admission, even before approval repair.
func TestAuthorizationExecRecoveryPolicyReleasesDeletionAdmission(t *testing.T) {
	for _, cut := range []api.EventKind{api.EventToolCall, api.EventApprovalRequest} {
		t.Run(string(cut), func(t *testing.T) {
			store := openStore(t, ":memory:")
			r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
			if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "owned", Harness: "gate"}, authOwner, false); err != nil {
				t.Fatal(err)
			}
			seedPublicGate(t, store.Session("owned"), "gate", cut)
			before := routingRecords(t, store.Session("owned"))
			var svc *session.Service
			var deleteErr error
			var checked bool
			svc = session.NewService(store, r, session.WithAuthorization(policyFunc(func(ctx context.Context, q api.AuthorizationRequest) error {
				if q.Action == api.ActionResume {
					checked = true
					// A separate deletion during policy evaluation must not contend with Exec's
					// metadata read. Re-entry probes the hold without goroutines or timing races.
					_, deleteErr = svc.DeleteSession(ctx, &pb.DeleteSessionRequest{Uid: q.SessionUID})
					return api.ErrPermissionDenied
				}
				return (api.OwnerAuthorizer{}).Authorize(ctx, q)
			})))
			stream := &rpcStream{ctx: callerContext(t, authOwner)}
			if err := svc.Exec(&pb.ExecRequest{Session: "owned"}, stream); status.Code(err) != codes.PermissionDenied {
				t.Fatalf("recovery policy denial = %v", err)
			}
			if !checked || deleteErr != nil {
				t.Fatalf("resume policy checked=%t, Delete during policy = %v", checked, deleteErr)
			}
			if len(stream.sent) != 0 || b.ioCounts() != ([4]int32{}) {
				t.Fatal("denied recovery sent a frame or touched runtime")
			}
			after := routingRecords(t, store.Session("owned"))
			if len(after) != len(before)+1 || !reflect.DeepEqual(before, after[:len(before)]) || after[len(before)].Event.Lifecycle == nil || after[len(before)].Event.Lifecycle.Kind != api.LifecycleDelete {
				t.Fatal("recovery changed the journal beyond the authorized deletion")
			}
		})
	}
}

func TestAuthorizationExecRecoveryRequiresResume(t *testing.T) {
	for _, cut := range []api.EventKind{api.EventToolCall, api.EventApprovalRequest} {
		t.Run(string(cut), func(t *testing.T) {
			store := openStore(t, ":memory:")
			r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
			if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "owned", Harness: "gate"}, authOwner, false); err != nil {
				t.Fatal(err)
			}
			seedPublicGate(t, store.Session("owned"), "gate", cut)
			var actions []api.Action
			svc := session.NewService(store, r, session.WithAuthorization(policyFunc(func(ctx context.Context, q api.AuthorizationRequest) error {
				assertAuthorizationMethod(t, q, pb.Sessions_Exec_FullMethodName)
				actions = append(actions, q.Action)
				if q.Action == api.ActionResume {
					return api.ErrPermissionDenied
				}
				return (api.OwnerAuthorizer{}).Authorize(ctx, q)
			})))
			before := routingRecords(t, store.Session("owned"))
			stream := &rpcStream{ctx: callerContext(t, authOwner)}
			err := svc.Exec(&pb.ExecRequest{Session: "owned"}, stream)
			if status.Code(err) != codes.PermissionDenied || len(stream.sent) != 0 || !reflect.DeepEqual(actions, []api.Action{api.ActionExec, api.ActionResume}) {
				t.Fatalf("recovery=%v frames=%d actions=%v", err, len(stream.sent), actions)
			}
			if b.ioCounts() != ([4]int32{}) || !reflect.DeepEqual(before, routingRecords(t, store.Session("owned"))) {
				t.Fatal("refused recovery performed IO or repair")
			}
			last := before[len(before)-1]
			probe := api.TextMessage("assistant", "fence probe")
			if _, err := store.Session("owned").Append(last.Seq, last.Fence, api.Event{Kind: api.EventOutput, ExecutionID: "execution", Message: probe}); err != nil {
				t.Fatalf("refused recovery fenced existing writer: %v", err)
			}
		})
	}
	store := openStore(t, ":memory:")
	r, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
	if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "ordinary", Harness: "gate"}, authOwner, false); err != nil {
		t.Fatal(err)
	}
	svc := session.NewService(store, r, session.WithAuthorization(policyFunc(func(ctx context.Context, q api.AuthorizationRequest) error {
		if q.Action == api.ActionResume {
			return api.ErrPermissionDenied
		}
		return (api.OwnerAuthorizer{}).Authorize(ctx, q)
	})))
	if err := svc.Exec(&pb.ExecRequest{Session: "ordinary"}, &rpcStream{ctx: callerContext(t, authOwner)}); err != nil {
		t.Fatalf("ordinary Exec needs no resume: %v", err)
	}
}

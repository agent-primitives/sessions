package session_test

import (
	"context"
	"database/sql"
	"net"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// forwardingStore implements SessionStore and HarnessStore by forwarding to a *sqlitelog.Store
// it does not embed, so it has no method beyond the two interfaces. A type assertion to
// *sqlitelog.Store, or to any interface wider than these, fails on it. It counts calls so a test
// can show which paths went through it. With opaqueLogs set, the journals it returns are
// opaqueLog values, so an assertion to *sqlitelog.Log fails too.
type forwardingStore struct {
	s          *sqlitelog.Store
	opaqueLogs bool
	mu         sync.Mutex
	calls      map[string]int
	// deleteAfter, deleteOnInfo and deleteErr serve armDeletion.
	deleteAfter  string
	deleteOnInfo bool
	deleteErr    error
}

// opaqueLog has only the eventlog.SessionJournal methods, CheckWritable forwarded among them: it
// hides *sqlitelog.Log and the optional BeginAllocation, ConfirmAllocation and ReadProto methods,
// as a minimal journal would.
type opaqueLog struct{ eventlog.SessionJournal }

func (f *forwardingStore) log(l eventlog.SessionJournal) eventlog.SessionJournal {
	if f.opaqueLogs {
		return opaqueLog{l}
	}
	return l
}

var (
	_ session.SessionStore = (*forwardingStore)(nil)
	_ session.HarnessStore = (*forwardingStore)(nil)
)

func newForwardingStore(s *sqlitelog.Store) *forwardingStore {
	return &forwardingStore{s: s, calls: map[string]int{}}
}

func (f *forwardingStore) called(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method]++
}

// uncalled returns the methods in want that the service never called.
func (f *forwardingStore) uncalled(want ...string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, m := range want {
		if f.calls[m] == 0 {
			out = append(out, m)
		}
	}
	return out
}

func (f *forwardingStore) SessionLog(uid string) eventlog.SessionJournal {
	f.called("SessionLog")
	return f.log(f.s.SessionLog(uid))
}

func (f *forwardingStore) ForkSessionLog(meta sqlitelog.SessionMeta) eventlog.SessionJournal {
	f.called("ForkSessionLog")
	return f.log(f.s.ForkSessionLog(meta))
}

func (f *forwardingStore) SessionInfo(uid string) (sqlitelog.SessionInfo, error) {
	f.called("SessionInfo")
	info, err := f.s.SessionInfo(uid)
	f.mu.Lock()
	onInfo := f.deleteOnInfo
	f.mu.Unlock()
	if err == nil && onInfo {
		f.deleteArmed()
	}
	return info, err
}

// armDeletion arms f to delete uid through the backing store when the test calls deleteArmed
// or, with onInfo, right after the next SessionInfo returns. The service has then read an ACTIVE
// session and runs the guarded operation on a deleted one, as when a deletion from another host
// sharing the store commits between the two phases (OPS-30, OPS-36). The deletion bypasses the
// Registry exclusion, so only the journal's preflight and write barrier stand between the
// operation and its effects.
func (f *forwardingStore) armDeletion(uid string, onInfo bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteAfter, f.deleteOnInfo, f.deleteErr = uid, onInfo, nil
}

// deleteArmed commits the deletion armDeletion armed, once.
func (f *forwardingStore) deleteArmed() {
	f.mu.Lock()
	uid := f.deleteAfter
	f.deleteAfter, f.deleteOnInfo = "", false
	f.mu.Unlock()
	if uid == "" {
		return
	}
	_, err := f.s.DeleteSession(context.Background(), uid)
	f.mu.Lock()
	f.deleteErr = err
	f.mu.Unlock()
}

// deletedOnLookup fails t unless the armed deletion ran and committed.
func (f *forwardingStore) deletedOnLookup(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	armed, err := f.deleteAfter, f.deleteErr
	f.mu.Unlock()
	if armed != "" {
		t.Fatalf("operation never looked up %q", armed)
	}
	if err != nil {
		t.Fatalf("deletion after lookup: %v", err)
	}
}

func (f *forwardingStore) ListSessions(opts sqlitelog.ListOptions) (sqlitelog.SessionPage, error) {
	if opts.Owner != nil {
		f.called("ListSessions(owner)")
	} else {
		f.called("ListSessions")
	}
	return f.s.ListSessions(opts)
}

func (f *forwardingStore) PutSession(meta sqlitelog.SessionMeta) error {
	f.called("PutSession")
	return f.s.PutSession(meta)
}

func (f *forwardingStore) PutSessionOnActiveHarness(meta sqlitelog.SessionMeta, registered bool) error {
	f.called("PutSessionOnActiveHarness")
	return f.s.PutSessionOnActiveHarness(meta, registered)
}

func (f *forwardingStore) CreateOwnedSession(meta sqlitelog.SessionMeta, owner api.CallerIdentity, registered bool) error {
	f.called("CreateOwnedSession")
	return f.s.CreateOwnedSession(meta, owner, registered)
}

func (f *forwardingStore) DeleteSession(ctx context.Context, uid string) (sqlitelog.SessionInfo, error) {
	f.called("DeleteSession")
	return f.s.DeleteSession(ctx, uid)
}

func (f *forwardingStore) Harness(name string) (sqlitelog.HarnessRecord, error) {
	f.called("Harness")
	return f.s.Harness(name)
}

func (f *forwardingStore) ListHarnesses(opts sqlitelog.HarnessListOptions) ([]sqlitelog.HarnessRecord, error) {
	f.called("ListHarnesses")
	return f.s.ListHarnesses(opts)
}

func (f *forwardingStore) RegisterHarness(rec sqlitelog.HarnessRecord) (sqlitelog.HarnessRecord, sqlitelog.RegisterResult, error) {
	f.called("RegisterHarness")
	return f.s.RegisterHarness(rec)
}

func (f *forwardingStore) CreateHarness(rec sqlitelog.HarnessRecord) (sqlitelog.HarnessRecord, error) {
	f.called("CreateHarness")
	return f.s.CreateHarness(rec)
}

func (f *forwardingStore) RetireHarness(name, reason string) (sqlitelog.HarnessRecord, error) {
	f.called("RetireHarness")
	return f.s.RetireHarness(name, reason)
}

func (f *forwardingStore) ReserveHarnessNames(names []string) error {
	f.called("ReserveHarnessNames")
	return f.s.ReserveHarnessNames(names)
}

func (f *forwardingStore) HarnessNameReserved(name string) (bool, error) {
	f.called("HarnessNameReserved")
	return f.s.HarnessNameReserved(name)
}

// serveHost serves svc and reg in-process on one server, as a host does.
func serveHost(t *testing.T, svc v1.SessionsServer, reg v1.HarnessRegistryServer) (v1.SessionsClient, v1.HarnessRegistryClient) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	v1.RegisterSessionsServer(srv, svc)
	v1.RegisterHarnessRegistryServer(srv, reg)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return v1.NewSessionsClient(conn), v1.NewHarnessRegistryClient(conn)
}

// TestServiceAndRegistryThroughStoreInterface runs a session lifecycle on a registered and a
// static harness with the Service and HarnessRegistry built over forwardingStore, so any path
// that still needs the concrete *sqlitelog.Store fails here. The opaque-logs run also fails any
// path that needs the concrete *sqlitelog.Log or its optional allocation methods.
func TestServiceAndRegistryThroughStoreInterface(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opaqueLogs bool
	}{
		{name: "sqlitelog logs"},
		{name: "opaque logs", opaqueLogs: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing := openStore(t, filepath.Join(t.TempDir(), "journal.db"))
			store := newForwardingStore(backing)
			store.opaqueLogs = tc.opaqueLogs
			runLifecycleThroughInterface(t, backing, store)
		})
	}
}

func runLifecycleThroughInterface(t *testing.T, backing *sqlitelog.Store, store *forwardingStore) {
	t.Helper()
	ctx := context.Background()

	echo := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = echo.Close() })
	registry, err := placement.NewRegistry("echo", map[string]*placement.Placer{
		"echo": placement.New(echo, echoagent.Model),
	}, placement.WithReservedNames("chat"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := session.NewHarnessRegistry(store, registry, echoFactory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	sessions, harnesses := serveHost(t, session.NewService(store, registry), reg)

	if _, err := backing.Harness("chat"); err == nil {
		t.Fatal("reserved name has a registration row")
	}
	if _, err := harnesses.RegisterHarness(ctx, &v1.RegisterHarnessRequest{Name: "chat", Spec: remoteSpec("127.0.0.1:9000")}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("register a reserved static name: want AlreadyExists, got %v", err)
	}
	const name = "registered"
	if _, err := harnesses.RegisterHarness(ctx, &v1.RegisterHarnessRequest{Name: name, Spec: remoteSpec("127.0.0.1:9000")}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := harnesses.CreateHarness(ctx, &v1.CreateHarnessRequest{Name: "created", Spec: remoteSpec("127.0.0.1:9000")}); err != nil {
		t.Fatalf("create-only register: %v", err)
	}
	if _, err := harnesses.CreateHarness(ctx, &v1.CreateHarnessRequest{Name: name, Spec: remoteSpec("127.0.0.1:9000")}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("create-only register of a registered name: want AlreadyExists, got %v", err)
	}
	// A name another host sharing the store reserved, which this host's registry does not hold.
	if err := backing.ReserveHarnessNames([]string{"other-static"}); err != nil {
		t.Fatal(err)
	}
	if _, err := harnesses.CreateHarness(ctx, &v1.CreateHarnessRequest{Name: "other-static", Spec: remoteSpec("127.0.0.1:9000")}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("create-only register of a name reserved elsewhere: want AlreadyExists, got %v", err)
	}

	sess, err := createOn(ctx, sessions, name)
	if err != nil {
		t.Fatalf("create on registered harness: %v", err)
	}
	if outs := execOutputs(t, sessions, sess, "one", 0); len(outs) != 1 || outs[0] != "echo:one" {
		t.Fatalf("turn 1 outputs = %v", outs)
	}
	if got, err := sessions.Suspend(ctx, &v1.SuspendRequest{Session: sess}); err != nil || got.GetComputeState() != v1.ComputeState_COMPUTE_COLD {
		t.Fatalf("suspend = %v, %v; want COLD", got.GetComputeState(), err)
	}
	if got, err := sessions.Resume(ctx, &v1.ResumeRequest{Session: sess}); err != nil || got.GetComputeState() != v1.ComputeState_COMPUTE_LIVE {
		t.Fatalf("resume = %v, %v; want LIVE", got.GetComputeState(), err)
	}
	cur, err := sessions.GetSession(ctx, &v1.GetSessionRequest{Uid: sess})
	if err != nil {
		t.Fatal(err)
	}
	if outs := execOutputs(t, sessions, sess, "two", cur.GetLastSeq()); len(outs) != 1 || outs[0] != "echo:two" {
		t.Fatalf("turn 2 outputs = %v", outs)
	}
	fork, err := sessions.Fork(ctx, &v1.ForkRequest{Session: sess})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	child := fork.GetChildren()[0].GetMetadata().GetUid()
	if err := execOn(ctx, sessions, child, ""); err != nil {
		t.Fatalf("turn on fork child: %v", err)
	}
	static, err := createOn(ctx, sessions, "")
	if err != nil {
		t.Fatalf("create on static harness: %v", err)
	}

	page, err := sessions.ListSessions(ctx, &v1.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got := uids(page.GetSessions())
	slices.Sort(got)
	want := []string{sess, child, static}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}

	if _, err := sessions.DeleteSession(ctx, &v1.DeleteSessionRequest{Uid: static}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := execOn(ctx, sessions, static, ""); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("exec on a deleted session: want FailedPrecondition, got %v", err)
	}

	if _, err := harnesses.RetireHarness(ctx, &v1.RetireHarnessRequest{Name: name, Reason: "done"}); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err := createOn(ctx, sessions, name); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("create on a retired harness: want FailedPrecondition, got %v", err)
	}
	listed, err := harnesses.ListHarnesses(ctx, &v1.ListHarnessesRequest{IncludeRetired: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(listed.GetHarnesses(), func(h *v1.HarnessRegistration) bool {
		return h.GetMetadata().GetName() == name && h.GetState() == v1.HarnessState_HARNESS_STATE_RETIRED
	}) {
		t.Fatalf("listed harnesses %v lack retired %q", listed.GetHarnesses(), name)
	}

	if missing := store.uncalled("SessionLog", "ForkSessionLog", "SessionInfo", "ListSessions", "PutSession",
		"PutSessionOnActiveHarness", "DeleteSession", "Harness", "ListHarnesses", "RegisterHarness",
		"CreateHarness", "RetireHarness", "ReserveHarnessNames", "HarnessNameReserved"); len(missing) != 0 {
		t.Fatalf("lifecycle never called %v through the interface", missing)
	}
}

// TestOwnedSessionsThroughStoreInterface covers the owner-bound create and the owner-filtered
// listing, which only an authorizing Service uses.
func TestOwnedSessionsThroughStoreInterface(t *testing.T) {
	store := newForwardingStore(openStore(t, ":memory:"))
	echo := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = echo.Close() })
	foreignCaller := api.CallerIdentity{Issuer: "issuer", Subject: "foreign"}
	policy := api.OwnerAllowlistAuthorizer{Allow: map[api.Action][]api.CallerIdentity{
		api.ActionCreate: {authOwner},
		api.ActionList:   {authOwner, foreignCaller},
	}}
	svc := session.NewService(store, echoRegistry(t, echo), session.WithAuthorization(policy))

	owner := callerContext(t, authOwner)
	created, err := svc.CreateSession(owner, &v1.CreateSessionRequest{Session: &v1.Session{}})
	if err != nil {
		t.Fatalf("owned create: %v", err)
	}
	page, err := svc.ListSessions(owner, &v1.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := uids(page.GetSessions()); !slices.Equal(got, []string{created.GetMetadata().GetUid()}) {
		t.Fatalf("owner listed %v", got)
	}
	foreign := callerContext(t, foreignCaller)
	if page, err := svc.ListSessions(foreign, &v1.ListSessionsRequest{}); err != nil || len(page.GetSessions()) != 0 {
		t.Fatalf("foreign listed %v, %v; want none", uids(page.GetSessions()), err)
	}
	if missing := store.uncalled("CreateOwnedSession", "ListSessions(owner)"); len(missing) != 0 {
		t.Fatalf("owned path never called %v through the interface", missing)
	}
}

// TestHarnessStoreRefusesNilPointer keeps the refusal NewHarnessRegistry and
// ReserveStaticHarnessNames gave a nil *sqlitelog.Store before they took an interface, where a
// nil pointer is a non-nil interface value.
func TestHarnessStoreRefusesNilPointer(t *testing.T) {
	echo := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = echo.Close() })
	registry := echoRegistry(t, echo)
	var store *sqlitelog.Store
	if _, err := session.NewHarnessRegistry(store, registry, echoFactory); err == nil {
		t.Error("NewHarnessRegistry accepted a nil *sqlitelog.Store")
	}
	if err := session.ReserveStaticHarnessNames(store, registry); err == nil {
		t.Error("ReserveStaticHarnessNames accepted a nil *sqlitelog.Store")
	}
	if _, err := session.NewHarnessRegistry(nil, registry, echoFactory); err == nil {
		t.Error("NewHarnessRegistry accepted a nil store")
	}
}

// TestDeletionAfterLookupThroughStoreInterface commits a deletion between the service's session
// lookup and the guarded operation (OPS-34) with the Service built over forwardingStore. Each
// operation must fail FAILED_PRECONDITION before Describe or any runtime call, and an exact retry
// of a committed approval must not report success. Only the journal's CheckWritable, which
// SessionJournal requires, refuses in time: the write barrier stops the first fence or append,
// after the runtime work, and an exact approval retry has neither.
func TestDeletionAfterLookupThroughStoreInterface(t *testing.T) {
	for _, logs := range []struct {
		name   string
		opaque bool
	}{
		{name: "sqlitelog logs"},
		{name: "opaque logs", opaque: true},
	} {
		for _, op := range []string{"Exec", "Suspend", "Resume", "Fork"} {
			t.Run(logs.name+"/"+op, func(t *testing.T) {
				ctx := t.Context()
				backing := openStore(t, filepath.Join(t.TempDir(), "journal.db"))
				store := newForwardingStore(backing)
				store.opaqueLogs = logs.opaque
				r, b := deletionRegistry(t)
				var svc v1.SessionsServer = session.NewService(store, r)
				if op == "Exec" {
					// Exec's lookup phase ends with its session frame; the guarded runtime work
					// follows it (OPS-34), so the deletion commits there.
					svc = deleteBeforeRuntimeServer{SessionsServer: svc, beforeRuntime: store.deleteArmed}
				}
				client := serveSessions(t, svc)
				uid := mustCreate(t, client)
				if op != "Exec" {
					if err := execOn(ctx, client, uid, ""); err != nil {
						t.Fatal(err)
					}
				}
				if op == "Resume" {
					if _, err := client.Suspend(ctx, &v1.SuspendRequest{Session: uid}); err != nil {
						t.Fatal(err)
					}
				}
				b.reset()
				store.armDeletion(uid, op != "Exec")
				before, err := backing.Session(uid).Head()
				if err != nil {
					t.Fatal(err)
				}
				err = deleteRaceMutation(ctx, client, op, uid, "")
				store.deletedOnLookup(t)
				if status.Code(err) != codes.FailedPrecondition {
					t.Errorf("%s after a deletion that followed its lookup = %v, want FailedPrecondition", op, err)
				}
				if calls := b.effects(); len(calls) != 0 {
					t.Errorf("%s reached the runtime after the deletion: %v", op, calls)
				}
				if head, err := backing.Session(uid).Head(); err != nil || head != before+1 {
					t.Errorf("journal head = %d, %v; want %d (only the deletion)", head, err, before+1)
				}
			})
		}
		t.Run(logs.name+"/exact approval retry", func(t *testing.T) {
			ctx := t.Context()
			backing := openStore(t, filepath.Join(t.TempDir(), "journal.db"))
			store := newForwardingStore(backing)
			store.opaqueLogs = logs.opaque
			r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
			client := serveSessions(t, session.NewService(store, r))
			const uid = "gated"
			if err := backing.PutSession(sqlitelog.SessionMeta{UID: uid, Harness: "gate"}); err != nil {
				t.Fatal(err)
			}
			seedPublicGate(t, backing.Session(uid), "gate", api.EventApprovalRequest)
			if _, err := backing.Session(uid).Append(3, 1, api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleSuspend}}); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Approve(ctx, publicDecision(uid)); err != nil {
				t.Fatalf("first decision: %v", err)
			}
			beforeIO := b.ioCounts()
			store.armDeletion(uid, true)
			retry, err := client.Approve(ctx, publicDecision(uid))
			store.deletedOnLookup(t)
			if status.Code(err) != codes.FailedPrecondition {
				t.Errorf("exact retry after a deletion that followed its lookup = %v, %v; want FailedPrecondition", retry, err)
			}
			if b.ioCounts() != beforeIO {
				t.Error("approval retry contacted compute")
			}
		})
	}
}

// TestReplayFieldPresenceThroughStoreInterface shows that Replay over a SessionStore returns the
// stored protobuf event, with its field presence, when the journal has ReadProto, and the
// converted domain record, with the stored hashes, when it does not.
func TestReplayFieldPresenceThroughStoreInterface(t *testing.T) {
	for _, tc := range []struct {
		name       string
		opaqueLogs bool
		wantActor  bool
	}{
		{name: "sqlitelog logs", wantActor: true},
		{name: "opaque logs", opaqueLogs: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			backing := openStore(t, path)
			const uid = "presence"
			log := backing.SessionLog(uid)
			fence, err := log.NewFence()
			if err != nil {
				t.Fatal(err)
			}
			want, err := log.Append(0, fence, api.Event{Kind: api.EventInput})
			if err != nil {
				t.Fatal(err)
			}
			// Store a present, empty actor, which the domain event cannot represent, leaving the
			// hash as it was.
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			var blob []byte
			if err := db.QueryRow("SELECT event FROM events WHERE session = ? AND seq = 1", uid).Scan(&blob); err != nil {
				t.Fatal(err)
			}
			ev := &v1.Event{}
			if err := proto.Unmarshal(blob, ev); err != nil {
				t.Fatal(err)
			}
			ev.Actor = &v1.IdentityRef{}
			if blob, err = proto.Marshal(ev); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE events SET event = ? WHERE session = ? AND seq = 1", blob, uid); err != nil {
				t.Fatal(err)
			}

			store := newForwardingStore(backing)
			store.opaqueLogs = tc.opaqueLogs
			echo := local.New(echoagent.Harness{})
			t.Cleanup(func() { _ = echo.Close() })
			registry, err := placement.NewRegistry("echo", map[string]*placement.Placer{
				"echo": placement.New(echo, echoagent.Model),
			})
			if err != nil {
				t.Fatal(err)
			}
			reg, err := session.NewHarnessRegistry(store, registry, echoFactory)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(reg.Close)
			sessions, _ := serveHost(t, session.NewService(store, registry), reg)

			stream, err := sessions.Replay(t.Context(), &v1.ReplayRequest{Session: uid})
			if err != nil {
				t.Fatal(err)
			}
			got, err := stream.Recv()
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if got.GetSeq() != want.Seq || got.GetContentHash() != want.Hash || got.GetEvent().GetKind() != v1.EventKind_EVENT_INPUT {
				t.Fatalf("replayed seq %d hash %q kind %v; want seq %d hash %q EVENT_INPUT",
					got.GetSeq(), got.GetContentHash(), got.GetEvent().GetKind(), want.Seq, want.Hash)
			}
			if gotActor := got.GetEvent().Actor != nil; gotActor != tc.wantActor {
				t.Fatalf("replayed actor present = %v, want %v", gotActor, tc.wantActor)
			}
			if missing := store.uncalled("SessionLog"); len(missing) != 0 {
				t.Fatalf("Replay did not read through the store: %v", missing)
			}
		})
	}
}

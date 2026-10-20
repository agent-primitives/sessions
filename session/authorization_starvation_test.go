package session_test

import (
	"context"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/agent-primitives/sessions/api"
	pb "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/authn"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"modernc.org/sqlite"
)

var authorizationBarrierID atomic.Uint64

// Park a real Store write inside SQLite, occupying its only connection. Once the
// foreign lookup is durably blocked on that connection it must hold no admission.
func TestAuthorizationOwnerLookupDoesNotHoldDeletionAdmission(t *testing.T) {
	var entered, release chan struct{}
	function := fmt.Sprintf("authorization_barrier_%d", authorizationBarrierID.Add(1))
	if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
		close(entered)
		<-release
		return int64(0), nil
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "journal.db")
	store := openStore(t, path)
	r, _ := publicGateRegistry(t, &publicGateHarness{}, nil)
	if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "owned", Harness: "gate"}, authOwner, false); err != nil {
		t.Fatal(err)
	}
	db := deletionSQL(t, path)
	if _, err := db.Exec("CREATE TRIGGER authorization_barrier BEFORE INSERT ON sessions BEGIN SELECT " + function + "(); END"); err != nil {
		t.Fatal(err)
	}
	svc := session.NewService(store, r, session.WithAuthorization(api.OwnerAuthorizer{}))
	foreign := callerContext(t, api.CallerIdentity{Issuer: "issuer", Subject: "foreign"})
	synctest.Test(t, func(t *testing.T) {
		entered, release = make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		defer unblock()
		written, denied := make(chan error, 1), make(chan error, 1)
		go func() { written <- store.PutSession(sqlitelog.SessionMeta{UID: "blocker"}) }()
		<-entered
		go func() {
			_, err := svc.Suspend(foreign, &pb.SuspendRequest{Session: "owned"})
			denied <- err
		}()
		synctest.Wait()
		select {
		case err := <-denied:
			t.Fatalf("lookup did not wait on the Store connection: %v", err)
		default:
		}
		// This is the same exclusive admission DeleteSession needs, tested while
		// SQL is still blocked rather than relying on scheduler probabilities.
		if err := r.WithSessionGuard("owned", func() error { return nil }); err != nil {
			t.Errorf("denied caller's blocked owner lookup held Delete admission: %v", err)
		}
		unblock()
		if err := <-written; err != nil {
			t.Fatal(err)
		}
		if err := <-denied; status.Code(err) != codes.PermissionDenied {
			t.Fatalf("foreign Suspend: %v", err)
		}
	})
}

// A single denied client must not monopolize Delete admission while reading its
// target's owner. Keep the reviewer's 500-attempt bufconn comparison reproducible.
func TestAuthorizationDeniedCallerDoesNotStarveDelete(t *testing.T) {
	for _, looping := range []bool{false, true} {
		t.Run(map[bool]string{false: "baseline", true: "denied-suspend-loop"}[looping], func(t *testing.T) {
			store := openStore(t, ":memory:")
			r, b := publicGateRegistry(t, &publicGateHarness{}, nil)
			if err := store.CreateOwnedSession(sqlitelog.SessionMeta{UID: "owned", Harness: "gate"}, authOwner, false); err != nil {
				t.Fatal(err)
			}
			svc := session.NewService(store, r, session.WithAuthorization(api.OwnerAuthorizer{}))
			clientFor := func(caller api.CallerIdentity) pb.SessionsClient {
				u, s, err := authn.NewInterceptors(authn.Required, callerVerifier{caller})
				if err != nil {
					t.Fatal(err)
				}
				return serveSessions(t, svc, grpc.ChainUnaryInterceptor(u), grpc.ChainStreamInterceptor(s))
			}
			owner := clientFor(authOwner)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			var denied atomic.Int64
			if looping {
				foreign := clientFor(api.CallerIdentity{Issuer: "issuer", Subject: "foreign"})
				done := make(chan struct{})
				go func() {
					defer close(done)
					for ctx.Err() == nil {
						_, err := foreign.Suspend(ctx, &pb.SuspendRequest{Session: "owned"})
						if status.Code(err) == codes.PermissionDenied {
							denied.Add(1)
						}
					}
				}()
				defer func() { cancel(); <-done }()
				for denied.Load() == 0 && ctx.Err() == nil {
					runtime.Gosched()
				}
			}
			aborted := 0
			for range 500 {
				_, err := owner.DeleteSession(ctx, &pb.DeleteSessionRequest{Uid: "owned"})
				switch status.Code(err) {
				case codes.Aborted:
					aborted++
				case codes.OK:
				default:
					t.Fatalf("Delete: %v", err)
				}
			}
			t.Logf("Delete ABORTED: %d/500; denied Suspend replies: %d", aborted, denied.Load())
			// A probe can briefly race exclusive admission, but it must not turn a
			// denied caller's SQL wait into sustained starvation (>10% refusals).
			if aborted > 50 || (!looping && aborted != 0) {
				t.Errorf("denied caller starved owner deletion: %d/500 ABORTED", aborted)
			}
			if looping && denied.Load() == 0 {
				t.Error("foreign client never exercised denial")
			}
			if b.ioCounts() != ([4]int32{}) {
				t.Fatal("denied Suspend or Delete touched runtime")
			}
		})
	}
}

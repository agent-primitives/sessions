package main

import (
	"context"
	"database/sql"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/canon"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func TestVerifyStoredProtobufPresence(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		storedActor, hashedActor bool
		wantErr                  bool
	}{
		{"valid empty actor", true, true, false},
		{"added empty actor", true, false, true},
		{"removed empty actor", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			store, err := sqlitelog.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			log := store.Session("audit-presence")
			fence, err := log.NewFence()
			if err != nil {
				t.Fatal(err)
			}
			record, err := log.Append(0, fence, api.Event{Kind: api.EventInput})
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			var blob []byte
			if err := db.QueryRow("SELECT event FROM events WHERE session = ? AND seq = 1", "audit-presence").Scan(&blob); err != nil {
				t.Fatal(err)
			}
			event := &v1.Event{}
			if err := proto.Unmarshal(blob, event); err != nil {
				t.Fatal(err)
			}
			if tc.hashedActor {
				event.Actor = &v1.IdentityRef{}
			}
			hash, err := canon.HashRecordProto(record.PrevHash, record.Seq, event)
			if err != nil {
				t.Fatal(err)
			}
			event.Actor = nil
			if tc.storedActor {
				event.Actor = &v1.IdentityRef{}
			}
			blob, err = proto.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("UPDATE events SET event = ?, hash = ? WHERE session = ? AND seq = 1", blob, hash, "audit-presence"); err != nil {
				t.Fatal(err)
			}
			err = cmdVerify([]string{"--journal", path, "--session", "audit-presence"})
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "content_hash mismatch at seq 1") {
					t.Fatalf("verify = %v, want presence tampering refusal", err)
				}
			} else if err != nil {
				t.Fatalf("valid stored presence rejected: %v", err)
			}
		})
	}
}

func TestVerifyEmptyCapturedHeadDoesNotReplay(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	event := &v1.Event{Kind: v1.EventKind_EVENT_INPUT}
	hash, err := canon.HashRecordProto("", 1, event)
	if err != nil {
		t.Fatal(err)
	}
	host := &verifyConcurrentAppend{
		appended: &v1.LogRecord{Seq: 1, ContentHash: hash, Event: event},
	}
	server := grpc.NewServer()
	v1.RegisterSessionsServer(server, host)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	if err := cmdVerify([]string{"--server", lis.Addr().String(), "--session", "empty-prefix"}); err != nil {
		t.Fatalf("empty captured prefix rejected: %v", err)
	}
	if n := host.replayCalls.Load(); n != 0 {
		t.Fatalf("Replay called %d times for an empty captured prefix", n)
	}
}

type verifyConcurrentAppend struct {
	v1.UnimplementedSessionsServer
	appended    *v1.LogRecord
	replayCalls atomic.Int32
}

func (*verifyConcurrentAppend) GetSession(context.Context, *v1.GetSessionRequest) (*v1.Session, error) {
	return &v1.Session{LastSeq: 0}, nil
}

func (s *verifyConcurrentAppend) Replay(_ *v1.ReplayRequest, stream v1.Sessions_ReplayServer) error {
	s.replayCalls.Add(1)
	return stream.Send(s.appended)
}

func TestVerifyRecords(t *testing.T) {
	var records []*v1.LogRecord
	prev := ""
	for i := int64(1); i <= 2; i++ {
		event := &v1.Event{Kind: v1.EventKind_EVENT_INPUT, Actor: &v1.IdentityRef{}}
		hash, err := canon.HashRecordProto(prev, i, event)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, &v1.LogRecord{Seq: i, PrevHash: prev, ContentHash: hash, Event: event})
		prev = hash
	}
	if err := verifyRecords(records, 2); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func([]*v1.LogRecord) []*v1.LogRecord
	}{
		{"presence tampering", func(r []*v1.LogRecord) []*v1.LogRecord { r[0].Event.Actor = nil; return r }},
		{"content tampering", func(r []*v1.LogRecord) []*v1.LogRecord { r[0].ContentHash = "changed"; return r }},
		{"broken link", func(r []*v1.LogRecord) []*v1.LogRecord { r[1].PrevHash = "changed"; return r }},
		{"sequence gap", func(r []*v1.LogRecord) []*v1.LogRecord { r[1].Seq++; return r }},
		{"truncated", func(r []*v1.LogRecord) []*v1.LogRecord { return r[:1] }},
		{"missing event", func(r []*v1.LogRecord) []*v1.LogRecord { r[0].Event = nil; return r }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloned := []*v1.LogRecord{proto.Clone(records[0]).(*v1.LogRecord), proto.Clone(records[1]).(*v1.LogRecord)}
			if err := verifyRecords(tc.mutate(cloned), 2); err == nil {
				t.Fatal("invalid chain accepted")
			}
		})
	}
	if err := verifyRecords(nil, 0); err != nil {
		t.Fatal(err)
	}
}

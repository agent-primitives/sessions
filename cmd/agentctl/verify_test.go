package main

import (
	"testing"

	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/canon"
	"google.golang.org/protobuf/proto"
)

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

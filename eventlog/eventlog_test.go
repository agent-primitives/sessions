package eventlog

import (
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/api"
)

func out(text string) api.Event {
	return api.Event{Kind: api.EventOutput, Message: api.TextMessage("assistant", text)}
}

// §5: single-writer enforcement — a second writer with a stale expected_last_seq is
// rejected; exactly one write at each head wins.
func TestSingleWriterCAS(t *testing.T) {
	l := New()
	f := l.NewFence()

	if _, err := l.Append(0, f, out("a")); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// head is now 1; a writer that still thinks head==0 must be rejected.
	if _, err := l.Append(0, f, out("b")); err != ErrConflict {
		t.Fatalf("stale expected_last_seq: want ErrConflict, got %v", err)
	}
	r, err := l.Append(1, f, out("b"))
	if err != nil || r.Seq != 2 {
		t.Fatalf("second append: seq=%d err=%v", r.Seq, err)
	}
}

// §5: incarnation fencing — a superseded incarnation (older token) cannot write.
func TestFencing(t *testing.T) {
	l := New()
	f1 := l.NewFence()
	if _, err := l.Append(0, f1, out("a")); err != nil {
		t.Fatalf("append with f1: %v", err)
	}
	f2 := l.NewFence() // a new incarnation takes over
	if _, err := l.Append(1, f1, out("zombie")); err != ErrFenced {
		t.Fatalf("stale fence: want ErrFenced, got %v", err)
	}
	if _, err := l.Append(1, f2, out("b")); err != nil {
		t.Fatalf("append with current fence f2: %v", err)
	}
}

// §7: the hash-chain links every record and is tamper-evident.
func TestHashChain(t *testing.T) {
	l := New()
	f := l.NewFence()
	last := int64(0)
	for _, s := range []string{"a", "b", "c"} {
		r, err := l.Append(last, f, out(s))
		if err != nil {
			t.Fatalf("append %q: %v", s, err)
		}
		last = r.Seq
	}
	if err := l.Verify(); err != nil {
		t.Fatalf("verify clean chain: %v", err)
	}
	// Tamper with a committed record's content; the chain must no longer verify.
	l.records[1].Event.Message.Parts[0].Text.Text = "tampered"
	if err := l.Verify(); !errors.Is(err, ErrTampered) {
		t.Fatalf("verify tampered chain: want ErrTampered, got %v", err)
	}
}

// VerifyChain checks links and content from any verified point, so a reader can verify only the
// records appended since it last checked.
func TestVerifyChain(t *testing.T) {
	l := New()
	f := l.NewFence()
	for i, s := range []string{"a", "b", "c", "d"} {
		if _, err := l.Append(int64(i), f, out(s)); err != nil {
			t.Fatal(err)
		}
	}
	recs := l.Read(1)
	if err := VerifyChain("", recs); err != nil {
		t.Fatalf("whole chain: %v", err)
	}
	if err := VerifyChain(recs[1].Hash, recs[2:]); err != nil {
		t.Fatalf("continued chain: %v", err)
	}
	if err := VerifyChain("", nil); err != nil {
		t.Fatalf("empty chain: %v", err)
	}
	for _, tc := range []struct {
		name string
		prev string
		recs func() []Record
		want string
	}{
		{"wrong start", recs[0].Hash, func() []Record { return recs[2:] }, "prev_hash mismatch at seq 3"},
		{"missing record", "", func() []Record { return append([]Record{recs[0]}, recs[2:]...) }, "prev_hash mismatch at seq 3"},
		{"changed event", "", func() []Record {
			cp := append([]Record(nil), recs...)
			cp[2].Event = out("changed")
			return cp
		}, "content_hash mismatch at seq 3"},
		{"changed hash", "", func() []Record {
			cp := append([]Record(nil), recs...)
			cp[3].Hash = "rewritten"
			return cp
		}, "content_hash mismatch at seq 4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyChain(tc.prev, tc.recs())
			if !errors.Is(err, ErrTampered) || err.Error() != ErrTampered.Error()+": "+tc.want {
				t.Fatalf("err = %v, want ErrTampered: %s", err, tc.want)
			}
		})
	}
}

// A replay-fork seeds a child from the parent prefix; the child's chain continues from
// the parent's hash at the fork point (hash-tree).
func TestForkContinuesChain(t *testing.T) {
	parent := New()
	f := parent.NewFence()
	last := int64(0)
	for _, s := range []string{"a", "b"} {
		r, _ := parent.Append(last, f, out(s))
		last = r.Seq
	}
	forkAt := parent.Read(1) // prefix [1..head]
	child := NewFrom(forkAt)
	cf := child.NewFence()
	r, err := child.Append(child.Head(), cf, out("c"))
	if err != nil {
		t.Fatalf("child append: %v", err)
	}
	if r.PrevHash != forkAt[len(forkAt)-1].Hash {
		t.Fatalf("child.first.prev_hash != parent@R.hash (hash-tree link broken)")
	}
	if err := child.Verify(); err != nil {
		t.Fatalf("child chain verify: %v", err)
	}
}

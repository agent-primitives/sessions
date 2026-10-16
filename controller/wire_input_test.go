package controller_test

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/wire"
)

// inputsHarness sends the turn's inputs to the model unchanged, as a harness forwarding the
// caller's message does.
type inputsHarness struct{}

func (inputsHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{ID: "inputs", Capabilities: api.Capabilities{Resumability: api.ResumabilityStatelessReplay}}, nil
}

func (inputsHarness) Run(ctx context.Context, s *api.Start, sink api.EventSink) error {
	_, err := sink.Model(ctx, api.ModelRequest{Model: "echo", Messages: s.Inputs})
	return err
}

// An Exec input decoded from the wire with a bytes source set to empty bytes (an empty file) must
// replay. The live turn hashes the decoded input (I0) and replay hashes the input read back from
// the journal, which stores the encoded form without the empty source; the two used to differ (a
// non-nil empty slice against nil), so every replay of the turn failed as divergence.
func TestReplayInputWithEmptyBytesSource(t *testing.T) {
	data, err := proto.Marshal(&v1.Message{Role: "user", Parts: []*v1.Part{
		{Part: &v1.Part_File{File: &v1.FilePart{Name: "empty.txt", Source: &v1.FilePart_Bytes{Bytes: []byte{}}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pb := &v1.Message{}
	if err := proto.Unmarshal(data, pb); err != nil {
		t.Fatal(err)
	}
	in := wire.MessageFromProto(pb)

	log := memStore(t)
	live, err := controller.New(log, echoModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Exec(t.Context(), inputsHarness{}, []api.Message{*in}, 0); err != nil {
		t.Fatalf("exec: %v", err)
	}
	replay, err := controller.New(log, echoModel)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replay.Replay(t.Context(), inputsHarness{}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if n := replay.ModelInvocations(); n != 0 {
		t.Fatalf("replay invoked the model %d times", n)
	}
}

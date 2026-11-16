package main

import (
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/agent-primitives/sessions/runtime/substrate"

	ateadapter "github.com/agent-primitives/sessions/integrations/substrate"
)

// This binary registers the substrate flags: a substrate harness passes this build's checks and
// reaches the server's own, instead of failing as an unknown flag, as it does in the neutral build.
func TestSubstrateFlagsAreRegistered(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "journal.db")
	err := run([]string{"-journal", journal, "-substrate-harness", "w=t/w", "-substrate-atespace", "s"}, newControl)
	if err == nil || !strings.Contains(err.Error(), "harness w requires -model") {
		t.Fatalf("Run error = %v, want the server's model check", err)
	}
}

func TestNewControlIsTheAdapter(t *testing.T) {
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctl := newControl(conn)
	if _, ok := ctl.(*ateadapter.Adapter); !ok {
		t.Fatal("newControl must return the ate-api adapter")
	}
	// -substrate-egress-policy-file needs it, or every substrate backend refuses to start.
	if _, ok := ctl.(substrate.EgressPolicyClient); !ok {
		t.Fatal("the ate-api adapter must implement substrate.EgressPolicyClient")
	}
}

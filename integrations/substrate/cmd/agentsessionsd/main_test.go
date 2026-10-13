package main

import (
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	ateadapter "github.com/aramase/agentsessions/integrations/substrate"
	"github.com/aramase/agentsessions/internal/agentsessionsd"
)

// This binary registers the substrate flags: a substrate harness reaches the startup checks instead
// of failing as an unknown flag, as it does in the neutral build.
func TestSubstrateFlagsAreRegistered(t *testing.T) {
	journal := filepath.Join(t.TempDir(), "journal.db")
	err := agentsessionsd.Run([]string{"-journal", journal, "-substrate-harness", "w=t/w"}, agentsessionsd.WithSubstrate(newControl))
	if err == nil || !strings.Contains(err.Error(), "-substrate-harness requires -model") {
		t.Fatalf("Run error = %v, want the substrate model check", err)
	}
}

func TestNewControlIsTheAdapter(t *testing.T) {
	conn, err := grpc.NewClient("127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, ok := newControl(conn).(*ateadapter.Adapter); !ok {
		t.Fatal("newControl must return the ate-api adapter")
	}
}

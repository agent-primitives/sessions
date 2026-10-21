package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/agent-primitives/sessions/api/genpb"
)

func cmdDelete(args []string) error {
	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	cfg := commonFlags(fs)
	var uid string
	fs.StringVar(&uid, "session", "", "session UID to permanently tombstone (no data erasure or compute cleanup)")
	_ = fs.Parse(args)
	c, cleanup, err := dial(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	sess, err := c.DeleteSession(cmdCtx, uid)
	if err != nil {
		return err
	}
	fmt.Printf("session %s %s\n", sess.GetMetadata().GetUid(), deletionFields(sess))
	if sess.GetDeletionState() == v1.DeletionState_DELETION_PENDING {
		fmt.Println("PENDING: durable intent only; compute and fork pins retained, not reclaimed.")
	}
	return nil
}

func deletionFields(s *v1.Session) string {
	return fmt.Sprintf("deletion=%s deletion_requested=%s",
		strings.TrimPrefix(s.GetDeletionState().String(), "DELETION_"),
		deletionTime(s.GetDeletionRequestedTime()))
}

func deletionTime(t *timestamppb.Timestamp) string {
	if t == nil {
		return "unset"
	}
	return t.AsTime().UTC().Format(time.RFC3339Nano)
}

package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/agent-primitives/sessions/api/genpb"
)

func cmdDelete(args []string) error {
	fs := flag.NewFlagSet("delete", flag.ExitOnError)
	cfg := sessionFlags(fs)
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
	if sess.GetDeletionState() == v1.DeletionState_DELETION_PENDING {
		if _, err := fmt.Fprintln(os.Stderr, "PENDING: durable intent only; compute and fork pins retained, not reclaimed."); err != nil {
			return fmt.Errorf("deletion intent committed for session %s but writing warning to stderr failed: %w", displayToken(sess.GetMetadata().GetUid()), err)
		}
	}
	if cfg.json {
		return writeJSON(sess)
	}
	return writeOutput(fmt.Sprintf("session %s %s\n", displayToken(sess.GetMetadata().GetUid()), deletionFields(sess)))
}

func deletionTime(t *timestamppb.Timestamp) string {
	if t == nil {
		return "unset"
	}
	return t.AsTime().UTC().Format(time.RFC3339Nano)
}

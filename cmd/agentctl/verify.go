package main

import (
	"errors"
	"flag"
	"fmt"

	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/canon"
)

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	cfg := commonFlags(fs)
	uid := fs.String("session", "", "session UID")
	_ = fs.Parse(args)
	if *uid == "" {
		return errors.New("verify requires -session")
	}
	c, cleanup, err := dial(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx := cmdCtx
	sess, err := c.GetSession(ctx, *uid)
	if err != nil {
		return err
	}
	var records []*v1.LogRecord
	if sess.GetLastSeq() > 0 {
		records, err = c.Replay(ctx, *uid, 1, sess.GetLastSeq())
		if err != nil {
			return err
		}
	}
	if err := verifyRecords(records, sess.GetLastSeq()); err != nil {
		return err
	}
	fmt.Printf("session %s chain verified (%d records)\n", *uid, len(records))
	return nil
}

func verifyRecords(records []*v1.LogRecord, expectedLastSeq int64) error {
	prev := ""
	for i, record := range records {
		seq := int64(i + 1)
		if record == nil || record.Event == nil {
			return fmt.Errorf("journal event missing at seq %d", seq)
		}
		if record.GetSeq() != seq {
			return fmt.Errorf("journal sequence gap: got %d, want %d", record.GetSeq(), seq)
		}
		if record.GetPrevHash() != prev {
			return fmt.Errorf("journal prev_hash mismatch at seq %d", seq)
		}
		want, err := canon.HashRecordProto(prev, seq, record.Event)
		if err != nil {
			return fmt.Errorf("canonicalize seq %d: %w", seq, err)
		}
		if record.GetContentHash() != want {
			return fmt.Errorf("journal content_hash mismatch at seq %d", seq)
		}
		prev = want
	}
	if got := int64(len(records)); got != expectedLastSeq {
		return fmt.Errorf("journal ended at seq %d, session metadata reports %d", got, expectedLastSeq)
	}
	return nil
}

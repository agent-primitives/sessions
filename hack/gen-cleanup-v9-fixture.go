//go:build ignore

// gen-cleanup-v9-fixture is copied into a v0.4.0 worktree to create and probe a genuine v9
// allocation journal. It intentionally uses only public v0.4.0 APIs.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/sqlitelog"
	_ "modernc.org/sqlite"
)

func main() {
	mode := flag.String("mode", "generate", "generate, open, or hold")
	path := flag.String("out", "", "database path (or -path for non-generate modes)")
	flag.StringVar(path, "path", *path, "database path")
	flag.Parse()
	if *path == "" {
		fatal(fmt.Errorf("database path is required"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch *mode {
	case "generate":
		if err := generate(ctx, *path); err != nil {
			fatal(err)
		}
	case "open":
		s, err := sqlitelog.Open(*path)
		if err == nil {
			_ = s.Close()
			fatal(fmt.Errorf("v0.4.0 unexpectedly opened the newer database"))
		}
		fmt.Println("REFUSED")
	case "hold":
		if err := hold(ctx, *path); err != nil {
			fatal(err)
		}
	default:
		fatal(fmt.Errorf("unknown mode %q", *mode))
	}
}

func generate(ctx context.Context, path string) error {
	_ = os.Remove(path)
	s, err := sqlitelog.Open(path)
	if err != nil {
		return err
	}
	defer s.Close()
	attempt, err := s.Session("v9-session").BeginAllocation(ctx, "v9-route", "create")
	if err != nil {
		return err
	}
	if err := s.Session("v9-session").ConfirmAllocation(ctx, attempt, api.Incarnation{ID: "v9-handle"}); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version != 9 {
		return fmt.Errorf("generated version=%d, want 9", version)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM allocations`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("generated fixture has no allocation")
	}
	return nil
}

func hold(ctx context.Context, path string) error {
	s, err := sqlitelog.Open(path)
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Println("READY")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return err
	}
	if line != "WRITE\n" {
		return fmt.Errorf("unexpected command %q", line)
	}
	attempt, err := s.Session("v9-open-writer").BeginAllocation(ctx, "legacy-route", "restore")
	if err != nil {
		return err
	}
	if err := s.Session("v9-open-writer").ConfirmAllocation(ctx, attempt, api.Incarnation{ID: "legacy-handle"}); err != nil {
		return err
	}
	fmt.Println("WROTE")
	return nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

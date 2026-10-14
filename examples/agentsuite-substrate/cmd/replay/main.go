// Command replay re-executes a session's agent against its journal and proves the model was not
// called.
//
// It reads the session's records through the Sessions API, verifies the hash chain locally, then runs
// the controller's replay against the session's own harness, reached through the atenet-router the
// way the host reaches it. Every model call the harness makes is served from the journal; the
// controller fails with ErrReplayInvokedModel if one is not, and the fake model's call counter,
// read before and after, shows the same thing from outside.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/client"
	"github.com/aramase/agentsessions/controller"
	"github.com/aramase/agentsessions/eventlog"
	"github.com/aramase/agentsessions/model/openai"
	"github.com/aramase/agentsessions/placement"
	"github.com/aramase/agentsessions/runtime/substrate"
)

func main() {
	server := flag.String("server", "127.0.0.1:8080", "Sessions API address")
	session := flag.String("session", "", "session UID")
	router := flag.String("router", "127.0.0.1:8081", "atenet-router address")
	atespace := flag.String("atespace", "agentsuite-demo", "atespace of the session actors")
	modelURL := flag.String("model-base-url", "http://127.0.0.1:8082/v1", "model endpoint the replay would call if it called one")
	statsURL := flag.String("stats-url", "http://127.0.0.1:8082/stats", "fake model call counter; empty relies on the controller's own count")
	flag.Parse()
	if err := run(*server, *session, *router, *atespace, *modelURL, *statsURL); err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		os.Exit(1)
	}
}

func run(server, uid, router, atespace, modelURL, statsURL string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := client.Dial(server)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	protos, err := c.Replay(ctx, uid, 1, 0)
	if err != nil {
		return fmt.Errorf("read journal: %w", err)
	}
	log, recs, harness, err := loadJournal(uid, protos)
	if err != nil {
		return err
	}
	head := recs[len(recs)-1]
	fmt.Printf("journal: %d records, hash chain verifies locally (head seq=%d hash=%.16s)\n", len(recs), head.Seq, head.Hash)

	before, err := modelCalls(ctx, statsURL)
	if err != nil {
		return err
	}
	model, err := openai.New(openai.WithModel("fake-model"), openai.WithBaseURL(modelURL))
	if err != nil {
		return err
	}
	opts := []controller.Option{controller.WithSessionUID(uid)}
	if harness != "" {
		opts = append(opts, controller.WithHarness(harness))
	}
	ctl, err := controller.New(eventlog.AsStore(log), model.Model, opts...)
	if err != nil {
		return err
	}
	h, closeHarness, err := placement.DefaultDial(api.Incarnation{
		ID:           uid,
		Address:      router,
		CallMetadata: map[string]string{substrate.TargetActorHeader: atespace + "/" + uid},
	})
	if err != nil {
		return fmt.Errorf("dial harness through the router: %w", err)
	}
	defer func() { _ = closeHarness() }()
	outputs, err := ctl.Replay(ctx, h)
	if err != nil {
		return fmt.Errorf("replay: %w", err)
	}
	after, err := modelCalls(ctx, statsURL)
	if err != nil {
		return err
	}
	for i, o := range outputs {
		fmt.Printf("replayed output %d: %s\n", i+1, o)
	}
	fmt.Printf("re-executed %d completed turns\n", completedTurns(recs))
	if statsURL != "" {
		fmt.Printf("model calls: controller=%d, fake provider %d -> %d (delta %d)\n", ctl.ModelInvocations(), before, after, after-before)
	} else {
		fmt.Printf("model calls: controller=%d\n", ctl.ModelInvocations())
	}
	if ctl.ModelInvocations() != 0 || after != before {
		return fmt.Errorf("replay called the model")
	}
	if err := log.Verify(); err != nil {
		return fmt.Errorf("journal does not verify after replay: %w", err)
	}
	fmt.Println("REPLAY OK: the agent re-ran every completed turn in its substrate actor with zero model calls")
	return nil
}

// modelCalls reads the fake provider's call counter; with no URL (a real provider) it reports 0 and
// the controller's own count is the proof. ctx bounds the request and the body read.
func modelCalls(ctx context.Context, statsURL string) (int64, error) {
	if statsURL == "" {
		return 0, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statsURL, nil)
	if err != nil {
		return 0, fmt.Errorf("read fake model stats: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("read fake model stats: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("read fake model stats: %s", resp.Status)
	}
	var s struct {
		Calls int64 `json:"calls"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return 0, fmt.Errorf("decode fake model stats: %w", err)
	}
	return s.Calls, nil
}

// loadJournal turns the session's records into a verified log and returns the harness its last turn
// ran on (empty for a journal written before EXECUTION_START named it). A session with no records,
// or with no completed turn, has nothing to replay and is refused: the controller re-executes only
// completed turns, so reporting such a replay as OK would claim a re-execution that never ran.
func loadJournal(uid string, protos []*v1.LogRecord) (*eventlog.Log, []eventlog.Record, string, error) {
	if len(protos) == 0 {
		return nil, nil, "", fmt.Errorf("session %s has no journal records: nothing to replay", uid)
	}
	recs := make([]eventlog.Record, 0, len(protos))
	harness := ""
	for _, p := range protos {
		r := eventlog.RecordFromProto(p)
		if r.Event.Kind == api.EventExecutionStart && r.Event.ExecutionStart != nil && r.Event.ExecutionStart.Harness != "" {
			harness = r.Event.ExecutionStart.Harness
		}
		recs = append(recs, r)
	}
	log := eventlog.NewFrom(recs)
	if err := log.Verify(); err != nil {
		return nil, nil, "", fmt.Errorf("journal does not verify: %w", err)
	}
	if completedTurns(recs) == 0 {
		return nil, nil, "", fmt.Errorf("session %s has no completed turn (no END record in %d records): nothing to re-execute", uid, len(recs))
	}
	return log, recs, harness, nil
}

// completedTurns counts the turns the controller's Replay re-executes: each one that reached an END
// record, whatever its state.
func completedTurns(recs []eventlog.Record) int {
	n := 0
	for _, r := range recs {
		if r.Event.Kind == api.EventEnd {
			n++
		}
	}
	return n
}

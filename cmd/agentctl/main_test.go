package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/model/openai"
	"github.com/agent-primitives/sessions/sqlitelog"
	"github.com/agent-primitives/sessions/wire"
)

func TestExecHarnessFlag(t *testing.T) {
	for _, tt := range []struct {
		name    string
		session string
		harness string
	}{
		{name: "host default"},
		{name: "create chat", harness: "chat"},
		{name: "session default", session: "existing"},
		{name: "turn override", session: "existing", harness: "chat"},
		{name: "unknown harness", harness: "unknown"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requests := make(chan *v1.ExecRequest, 1)
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := grpc.NewServer()
			v1.RegisterSessionsServer(srv, &execRecorder{requests: requests})
			go func() { _ = srv.Serve(lis) }()
			t.Cleanup(srv.Stop)

			args := []string{"--server", lis.Addr().String(), "--input", "hello"}
			if tt.session != "" {
				args = append(args, "--session", tt.session)
			}
			if tt.harness != "" {
				args = append(args, "--harness", tt.harness)
			}
			err = cmdExec(args)
			if tt.harness == "unknown" {
				if status.Code(err) != codes.InvalidArgument {
					t.Fatalf("unknown harness error = %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			select {
			case req := <-requests:
				if req.GetHarness() != tt.harness || req.GetSession() != tt.session {
					t.Fatalf("exec request = %v", req)
				}
				if len(req.GetInputs()) != 1 || wire.MessageFromProto(req.Inputs[0]).Text() != "hello" {
					t.Fatalf("inputs = %v", req.GetInputs())
				}
				if req.ExpectedLastSeq != nil {
					t.Fatal("default exec unexpectedly enabled CAS")
				}
			default:
				t.Fatal("exec sent no request")
			}
		})
	}
}

type execRecorder struct {
	v1.UnimplementedSessionsServer
	requests chan *v1.ExecRequest
}

func (s *execRecorder) Exec(req *v1.ExecRequest, stream v1.Sessions_ExecServer) error {
	s.requests <- req
	if req.GetHarness() == "unknown" {
		return status.Error(codes.InvalidArgument, "unknown harness")
	}
	return stream.Send(&v1.ExecUpdate{Update: &v1.ExecUpdate_Session{Session: &v1.Session{
		Metadata: &v1.ResourceMetadata{Uid: "session-uid"},
		Harness:  req.GetHarness(),
	}}})
}

// Embedded mode refuses a journal in which another host registered the harness name it serves, and
// opens one whose registrations use other names.
func TestEmbeddedRefusesRegistrationCollision(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wantErr bool
	}{
		{name: "echo", wantErr: true},
		{name: "suite-agent"},
	} {
		journal := filepath.Join(t.TempDir(), "journal.db")
		store, err := sqlitelog.Open(journal)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.RegisterHarness(sqlitelog.HarnessRecord{Name: tc.name, UID: "u", Spec: "{}", SpecDigest: "d"}); err != nil {
			t.Fatal(err)
		}
		_ = store.Close()
		err = cmdList([]string{"--journal", journal})
		if tc.wantErr {
			if err == nil || !strings.Contains(err.Error(), "collides with a static harness name") {
				t.Errorf("registration %q: got %v, want a collision error", tc.name, err)
			}
		} else if err != nil {
			t.Errorf("registration %q: %v", tc.name, err)
		}
	}
}

// replay -v prints each record's chain links and the model call and usage fields, so the journal's
// digests and token counts can be read without a client of the API.
func TestFormatRecordVerbose(t *testing.T) {
	for _, tt := range []struct {
		r    *v1.LogRecord
		want string
	}{{
		r: &v1.LogRecord{Seq: 1, ContentHash: "sha256:0123456789abcdef0123", Event: &v1.Event{
			Kind: v1.EventKind_EVENT_INPUT, ExecutionId: "e1",
			Body: &v1.Event_Message{Message: &v1.Message{Role: "user", Parts: []*v1.Part{{Part: &v1.Part_Text{Text: &v1.TextPart{Text: "hi"}}}}}},
		}},
		want: "seq=1   EVENT_INPUT          hash=0123456789abcdef prev=- exec=e1  user: hi",
	}, {
		r: &v1.LogRecord{Seq: 2, ContentHash: "abc", PrevHash: "sha256:0123456789abcdef0123", Event: &v1.Event{
			Kind: v1.EventKind_EVENT_MODEL_CALL, Body: &v1.Event_Model{Model: &v1.ModelCall{Model: "gpt", InputHash: "sha256:fedcba98765432100000"}},
		}},
		want: "seq=2   EVENT_MODEL_CALL     hash=abc prev=0123456789abcdef  model=gpt input_hash=fedcba9876543210",
	}, {
		r: &v1.LogRecord{Seq: 3, ContentHash: "def", PrevHash: "abc", Event: &v1.Event{
			Kind: v1.EventKind_EVENT_USAGE, Body: &v1.Event_Usage{Usage: &v1.Usage{Model: "gpt", InputTokens: 12, OutputTokens: 5, ReasoningTokens: 1}},
		}},
		want: "seq=3   EVENT_USAGE          hash=def prev=abc  model=gpt input_tokens=12 output_tokens=5 reasoning_tokens=1",
	}} {
		if got := formatRecordVerbose(tt.r); got != tt.want {
			t.Errorf("got  %q\nwant %q", got, tt.want)
		}
	}
}

// A tool call's record shows the call an approval would authorize, without its idempotency key.
func TestFormatRecordShowsToolCall(t *testing.T) {
	call := wire.ToolCallToProto(&api.ToolCall{ID: "call_1", Tool: "publish", Args: map[string]any{"text": "hello", "a": []any{1.0}}, Mediation: api.MediationRequiresApproval, IdempotencyKey: "secret-key"})
	record := &v1.LogRecord{Seq: 5, ContentHash: "0123456789abcdef0123", Event: &v1.Event{Kind: v1.EventKind_EVENT_TOOL_CALL, Body: &v1.Event_Tool{Tool: call}}}
	want := `seq=5   EVENT_TOOL_CALL  tool=publish id=call_1 mediation=REQUIRES_APPROVAL args={"a":[1],"text":"hello"}`
	if got := formatRecord(record); got != want {
		t.Fatalf("formatRecord =\n%s\nwant\n%s", got, want)
	}
	if got := formatRecordVerbose(record); !strings.HasSuffix(got, `  tool=publish id=call_1 mediation=REQUIRES_APPROVAL args={"a":[1],"text":"hello"}`) || strings.Contains(got, "secret-key") {
		t.Fatalf("formatRecordVerbose = %s", got)
	}
	call.Args = nil
	if got := formatRecord(record); !strings.HasSuffix(got, "args=null") {
		t.Fatalf("formatRecord without arguments = %s", got)
	}
}

// Arguments print as the tool receives them: <, > and & are not HTML-escaped, so an approver reads
// the literal text.
func TestFormatRecordToolCallArgsNotHTMLEscaped(t *testing.T) {
	call := wire.ToolCallToProto(&api.ToolCall{ID: "call_1", Tool: "publish", Args: map[string]any{"text": `<a href="x">A & B</a>`}, Mediation: api.MediationRequiresApproval})
	record := &v1.LogRecord{Seq: 5, Event: &v1.Event{Kind: v1.EventKind_EVENT_TOOL_CALL, Body: &v1.Event_Tool{Tool: call}}}
	want := `args={"text":"<a href=\"x\">A & B</a>"}`
	if got := formatRecord(record); !strings.HasSuffix(got, want) {
		t.Fatalf("formatRecord = %s, want suffix %s", got, want)
	}
}

// Format characters such as the bidi override U+202E print escaped, so model-chosen arguments cannot
// visually reorder what an approver reads; the escaped text still decodes to the original value.
func TestFormatRecordToolCallArgsEscapeFormatRunes(t *testing.T) {
	text := "pay \u202eevil\u202c ok\u200b \U000e0041 a\u0085b\u009b8mc\x7f"
	call := wire.ToolCallToProto(&api.ToolCall{ID: "call_1", Tool: "publish", Args: map[string]any{"text": text}, Mediation: api.MediationRequiresApproval})
	record := &v1.LogRecord{Seq: 5, Event: &v1.Event{Kind: v1.EventKind_EVENT_TOOL_CALL, Body: &v1.Event_Tool{Tool: call}}}
	got := formatRecord(record)
	want := `args={"text":"pay \u202eevil\u202c ok\u200b \udb40\udc41 a\u0085b\u009b8mc\u007f"}`
	if !strings.HasSuffix(got, want) {
		t.Fatalf("formatRecord = %s, want suffix %s", got, want)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(strings.TrimPrefix(want, "args=")), &decoded); err != nil || decoded["text"] != text {
		t.Fatalf("escaped args decode to %q (err %v), want %q", decoded["text"], err, text)
	}
	assertOneSafeLine(t, got)
}

// Message text is untrusted model output. It prints on its own record's line, with every rune a
// terminal could act on or not draw written as an escape.
func TestFormatRecordMessageTextEscaped(t *testing.T) {
	for _, tt := range []struct {
		name, role, text, want string
	}{
		{name: "plain", role: "assistant", text: `Published: {"text":"<a & b>"} \n`, want: `assistant: Published: {"text":"<a & b>"} \n`},
		{name: "newline and ESC", role: "assistant", text: "one\ntwo\x1b[8m", want: `assistant: one\ntwo\x1b[8m`},
		{name: "tab and CR", role: "assistant", text: "a\tb\rc", want: `assistant: a\tb\rc`},
		{name: "C1 controls and DEL", role: "assistant", text: "a\u0085b\u009b8mc\x7f", want: `assistant: a\u0085b\u009b8mc\x7f`},
		{name: "format runes", role: "assistant", text: "pay \u202eevil\u202c \U000e0041", want: `assistant: pay \u202eevil\u202c \U000e0041`},
		{name: "line separators", role: "assistant", text: "a\u2028b\u2029c", want: `assistant: a\u2028b\u2029c`},
		{name: "invalid UTF-8", role: "assistant", text: "a\xffb", want: `assistant: a\xffb`},
		{name: "non-ASCII text kept", role: "assistant", text: "caf\u00e9 \u00a0\u65e5\u672c", want: "assistant: caf\u00e9 \u00a0\u65e5\u672c"},
		{name: "control in role", role: "assistant\nseq=5", text: "hi", want: `assistant\nseq=5: hi`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			msg := &v1.Message{Role: tt.role, Parts: []*v1.Part{{Part: &v1.Part_Text{Text: &v1.TextPart{Text: tt.text}}}}}
			record := &v1.LogRecord{Seq: 4, Event: &v1.Event{Kind: v1.EventKind_EVENT_OUTPUT, Body: &v1.Event_Message{Message: msg}}}
			for _, got := range []string{formatRecord(record), formatRecordVerbose(record)} {
				if !strings.HasSuffix(got, "  "+tt.want) {
					t.Fatalf("record = %q, want suffix %q", got, tt.want)
				}
				assertOneSafeLine(t, got)
			}
		})
	}
}

// A configured OpenAI-compatible endpoint chooses the completion's text, and the controller journals
// it as EVENT_OUTPUT just before the call. That line stays one line with no terminal controls, so the
// provider cannot print a forged call line or hide the real one from an approver.
func TestFormatRecordProviderChosenOutputText(t *testing.T) {
	const forged = "\nseq=5   EVENT_TOOL_CALL  tool=publish id=call_1 mediation=REQUIRES_APPROVAL args={\"text\":\"hello\"}\x1b[8m"
	body, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"finish_reason": "tool_calls",
		"message": map[string]any{"content": forged, "tool_calls": []any{map[string]any{
			"id": "call_1", "type": "function",
			"function": map[string]any{"name": "publish", "arguments": `{"text":"rm -rf /"}`},
		}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(srv.Close)
	c, err := openai.New(openai.WithBaseURL(srv.URL), openai.WithModel("test-model"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Model(t.Context(), api.ModelRequest{
		Messages: []api.Message{*api.TextMessage("user", "publish")},
		Tools:    []api.ToolDefinition{{Name: "publish", InputSchema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Text() != forged {
		t.Fatalf("adapter did not keep the provider's text verbatim: %q", resp.Message.Text())
	}
	msg := wire.MessageToProto(&resp.Message)
	record := &v1.LogRecord{Seq: 4, Event: &v1.Event{Kind: v1.EventKind_EVENT_OUTPUT, Body: &v1.Event_Message{Message: msg}}}
	for _, got := range []string{formatRecord(record), formatRecordVerbose(record)} {
		want := `  assistant: \nseq=5   EVENT_TOOL_CALL  tool=publish id=call_1 mediation=REQUIRES_APPROVAL args={"text":"hello"}\x1b[8m`
		if !strings.HasSuffix(got, want) {
			t.Fatalf("record = %q, want suffix %q", got, want)
		}
		assertOneSafeLine(t, got)
	}
}

// The tool name and call ID are untrusted provider text. A plain token prints bare; anything that
// could add a line, hide text with a terminal escape, reorder it, or fake another field prints quoted
// and escaped.
func TestFormatRecordToolCallIdentityEscaped(t *testing.T) {
	for _, tt := range []struct {
		name, tool, id, want string
	}{
		{name: "plain", tool: "publish", id: "call_1", want: `tool=publish id=call_1 `},
		{name: "newline and ESC in ID", tool: "publish", id: "call_1\n\x1b[8m", want: `tool=publish id="call_1\n\x1b[8m" `},
		{name: "space fakes a field", tool: "publish", id: "call_1 mediation=AUTO", want: `tool=publish id="call_1 mediation=AUTO" `},
		{name: "format rune in ID", tool: "publish", id: "call_\u202e1", want: `tool=publish id="call_\u202e1" `},
		{name: "non-ASCII space in ID", tool: "publish", id: "call\u00a01", want: `tool=publish id="call\u00a01" `},
		{name: "quote and backslash", tool: "publish", id: `a"b\c`, want: `tool=publish id="a\"b\\c" `},
		{name: "invalid UTF-8", tool: "publish", id: "call_\xff", want: `tool=publish id="call_\xff" `},
		{name: "empty", tool: "", id: "", want: `tool="" id="" `},
		{name: "control in tool", tool: "pub\rlish", id: "call_1", want: `tool="pub\rlish" id=call_1 `},
	} {
		t.Run(tt.name, func(t *testing.T) {
			call := &v1.ToolCall{Id: tt.id, Tool: tt.tool, Mediation: v1.Mediation_MEDIATION_REQUIRES_APPROVAL}
			record := &v1.LogRecord{Seq: 5, Event: &v1.Event{Kind: v1.EventKind_EVENT_TOOL_CALL, Body: &v1.Event_Tool{Tool: call}}}
			got := formatRecord(record)
			if !strings.Contains(got, "  "+tt.want+"mediation=REQUIRES_APPROVAL args=null") {
				t.Fatalf("formatRecord = %q, want it to contain %q", got, tt.want)
			}
			assertOneSafeLine(t, got)
		})
	}
}

// A configured OpenAI-compatible endpoint chooses the call ID, and the adapter accepts any non-empty
// unique ID. The record agentctl renders for that accepted call stays one line with no terminal
// controls, so a forged ID cannot hide the real arguments from an approver.
func TestFormatRecordProviderChosenToolCallID(t *testing.T) {
	const forged = "call_1 mediation=AUTO args={\"text\":\"safe\"}\n\x1b[8m"
	body, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"finish_reason": "tool_calls",
		"message": map[string]any{"tool_calls": []any{map[string]any{
			"id": forged, "type": "function",
			"function": map[string]any{"name": "publish", "arguments": `{"text":"rm -rf /"}`},
		}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	t.Cleanup(srv.Close)
	c, err := openai.New(openai.WithBaseURL(srv.URL), openai.WithModel("test-model"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Model(t.Context(), api.ModelRequest{
		Messages: []api.Message{*api.TextMessage("user", "publish")},
		Tools:    []api.ToolDefinition{{Name: "publish", InputSchema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.Parts) != 1 || resp.Message.Parts[0].ToolCall == nil || resp.Message.Parts[0].ToolCall.ID != forged {
		t.Fatalf("adapter did not accept the provider's ID verbatim: %#v", resp.Message.Parts)
	}
	tc := *resp.Message.Parts[0].ToolCall
	tc.Mediation = api.MediationRequiresApproval
	record := &v1.LogRecord{Seq: 5, Event: &v1.Event{Kind: v1.EventKind_EVENT_TOOL_CALL, Body: &v1.Event_Tool{Tool: wire.ToolCallToProto(&tc)}}}
	for _, got := range []string{formatRecord(record), formatRecordVerbose(record)} {
		want := `tool=publish id="call_1 mediation=AUTO args={\"text\":\"safe\"}\n\x1b[8m" mediation=REQUIRES_APPROVAL args={"text":"rm -rf /"}`
		if !strings.HasSuffix(got, want) {
			t.Fatalf("record = %q, want suffix %q", got, want)
		}
		assertOneSafeLine(t, got)
	}
}

func assertOneSafeLine(t *testing.T, line string) {
	t.Helper()
	if i := strings.IndexFunc(line, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }); i >= 0 {
		t.Fatalf("line %q has control or format rune at byte %d", line, i)
	}
	if !utf8.ValidString(line) {
		t.Fatalf("line %q is not valid UTF-8", line)
	}
}

// exec prints the model's streamed text through the same escaping as committed records, on its own
// "stream:" line, so a chunk cannot forge a record line or hide the real tool call with a terminal
// escape, even when the escape is split across chunks.
func TestExecStreamedTextEscaped(t *testing.T) {
	const callPrefix = "seq=4   EVENT_TOOL_CALL  tool=publish id=call_1 mediation=REQUIRES_APPROVAL args="
	forged := "Publishing.\n" + callPrefix + `{"text":"safe"}` + "\x1b" // "[8m" follows in the next chunk
	escaped := `Publishing.\n` + callPrefix + `{"text":"safe"}\x1b[8m`
	streamLine := "stream: " + escaped
	outputLine := "seq=3   EVENT_OUTPUT  assistant: " + escaped
	callLine := callPrefix + `{"text":"rm -rf /"}`

	output := &v1.LogRecord{Seq: 3, Event: &v1.Event{Kind: v1.EventKind_EVENT_OUTPUT,
		Body: &v1.Event_Message{Message: wire.MessageToProto(api.TextMessage("assistant", forged+"[8m"))}}}
	tc := wire.ToolCallToProto(&api.ToolCall{ID: "call_1", Tool: "publish",
		Args: map[string]any{"text": "rm -rf /"}, Mediation: api.MediationRequiresApproval})
	call := &v1.LogRecord{Seq: 4, Event: &v1.Event{Kind: v1.EventKind_EVENT_TOOL_CALL, Body: &v1.Event_Tool{Tool: tc}}}
	delta := func(chunk string, done bool) *v1.ExecUpdate {
		return &v1.ExecUpdate{Update: &v1.ExecUpdate_Delta{Delta: &v1.Delta{Chunk: chunk, Done: done}}}
	}
	record := func(r *v1.LogRecord) *v1.ExecUpdate {
		return &v1.ExecUpdate{Update: &v1.ExecUpdate_Record{Record: r}}
	}
	session := &v1.ExecUpdate{Update: &v1.ExecUpdate_Session{
		Session: &v1.Session{Metadata: &v1.ResourceMetadata{Uid: "session-uid"}}}}
	streamed := []*v1.ExecUpdate{session, delta(forged, false), delta("[8m", false)}
	for _, tt := range []struct {
		name    string
		updates []*v1.ExecUpdate
		want    []string
	}{{
		name:    "escape split across chunks",
		updates: append(streamed[:3:3], delta("", true), record(output), record(call)),
		want:    []string{"session session-uid", streamLine, outputLine, callLine},
	}, {
		name:    "no done frame before a record",
		updates: append(streamed[:3:3], record(call)),
		want:    []string{"session session-uid", streamLine, callLine},
	}, {
		name:    "no done frame before the turn ends",
		updates: streamed,
		want:    []string{"session session-uid", streamLine},
	}} {
		t.Run(tt.name, func(t *testing.T) {
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := grpc.NewServer()
			v1.RegisterSessionsServer(srv, &execStreamer{updates: tt.updates})
			go func() { _ = srv.Serve(lis) }()
			t.Cleanup(srv.Stop)

			var out bytes.Buffer
			if err := runExec([]string{"--server", lis.Addr().String(), "--input", "publish"}, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(out.String(), "\n") {
				t.Fatalf("output %q does not end its last line", out.String())
			}
			got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("exec output\n got %q\nwant %q", got, tt.want)
			}
			for _, line := range got {
				assertOneSafeLine(t, line)
			}
		})
	}
}

// execStreamer answers Exec with fixed updates, as the daemon streams them.
type execStreamer struct {
	v1.UnimplementedSessionsServer
	updates []*v1.ExecUpdate
}

func (s *execStreamer) Exec(_ *v1.ExecRequest, stream v1.Sessions_ExecServer) error {
	for _, u := range s.updates {
		if err := stream.Send(u); err != nil {
			return err
		}
	}
	return nil
}

// Command agentctl is the client-facing CLI for agentsessions — the top seam a user or GitHub
// drives. By default it runs an EMBEDDED Sessions gRPC server over a local unix socket (backed by a
// sqlite journal on disk) and connects to it as a real gRPC client — not an in-process wrapper — so
// the same code path serves remote. Pass --server <addr> to drive a remote controller instead.
//
//	agentctl create  --name "triage" --project acme
//	agentctl list    --project acme           # enumerate sessions, newest first
//	agentctl exec    --input "hi"            # create + run a turn (prints the session UID)
//	agentctl exec    --session <uid> --input "..."
//	agentctl replay  --session <uid>          # re-deliver the committed log
//	agentctl fork    --session <uid> --at N    # branch into a child session
//	agentctl suspend --session <uid>
//	agentctl resume  --session <uid>
//	agentctl delete  --session <uid>          # permanent intent only; compute/pins retained
//	agentctl approve <uid>                  # commit approval, then Resume
//	agentctl deny    <uid> --reason "no"    # commit denial, then Resume
//	agentctl harness register --server 127.0.0.1:8081 --name mine --spec spec.json
//	agentctl harness get|list|retire --server 127.0.0.1:8081 [flags]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"google.golang.org/grpc"

	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/client"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/internal/version"
	"github.com/agent-primitives/sessions/observability"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"create":  cmdCreate,
		"list":    cmdList,
		"get":     cmdGet,
		"delete":  cmdDelete,
		"exec":    cmdExec,
		"replay":  cmdReplay,
		"fork":    cmdFork,
		"suspend": cmdSuspend,
		"resume":  cmdResume,
		"approve": cmdApprove,
		"deny":    cmdDeny,
		"version": cmdVersion,
		"harness": cmdHarness,
	}
	run, ok := cmds[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	if err := run(os.Args[2:]); err != nil {
		printError(os.Stderr, err)
		os.Exit(1)
	}
}

// printError writes the line agentctl prints for a failed command.
func printError(w io.Writer, err error) {
	_, _ = fmt.Fprintln(w, "agentctl:", err)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: agentctl <create|list|get|delete|exec|replay|fork|suspend|resume|approve|deny|harness|version> [flags]")
}

type config struct {
	server  string
	journal string
	project string
}

func commonFlags(fs *flag.FlagSet) *config {
	c := &config{}
	fs.StringVar(&c.server, "server", "", "remote Sessions gRPC address; empty = embedded local server")
	fs.StringVar(&c.journal, "journal", "agentsessions.db", "sqlite journal path (embedded mode)")
	fs.StringVar(&c.project, "project", sqlitelog.DefaultProject, "project (tenant) to create and list sessions in")
	return c
}

// dial returns a Sessions client. With --server it dials the remote; otherwise it starts an
// embedded Sessions server over a unix socket, backed by the local sqlite journal, and dials that.
func dial(cfg *config) (*client.Client, func(), error) {
	if cfg.server != "" {
		c, err := client.Dial(cfg.server, client.WithProject(cfg.project))
		if err != nil {
			return nil, nil, err
		}
		return c, func() { _ = c.Close() }, nil
	}

	store, err := sqlitelog.Open(cfg.journal, sqlitelog.WithDefaultProject(cfg.project))
	if err != nil {
		return nil, nil, err
	}
	logger := slog.Default()
	backend := local.New(echoagent.Harness{}, local.WithLogger(logger))
	registry, err := placement.NewRegistry("echo", map[string]*placement.Placer{
		"echo": placement.New(backend, echoagent.Model, placement.WithLogger(logger)),
	})
	if err != nil {
		_ = backend.Close()
		_ = store.Close()
		return nil, nil, err
	}
	// The journal may be shared with a host that registers harnesses. Reserving the names this
	// embedded server runs refuses a journal where one is already registered, and stops one being
	// registered later.
	if err := session.ReserveStaticHarnessNames(store, registry); err != nil {
		_ = backend.Close()
		_ = store.Close()
		return nil, nil, fmt.Errorf("journal %s: %w", cfg.journal, err)
	}
	svc := session.NewService(store, registry,
		session.WithLogger(logger), session.WithDefaultProject(cfg.project))
	sock := fmt.Sprintf("%s/agentctl-%d.sock", os.TempDir(), os.Getpid())
	_ = os.Remove(sock)
	lis, err := net.Listen("unix", sock)
	if err != nil {
		_ = backend.Close()
		_ = store.Close()
		return nil, nil, err
	}
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(observability.UnaryServerInterceptor(logger)),
		grpc.ChainStreamInterceptor(observability.StreamServerInterceptor(logger)),
	)
	v1.RegisterSessionsServer(srv, svc)
	// A failed Serve leaves the embedded server dead and every later call failing to connect, which
	// reads as a client bug rather than a server that never started. Log it; the command is already
	// committed to this socket by the time it could be reported.
	go func() {
		if err := srv.Serve(lis); err != nil {
			logger.Error("embedded sessions server stopped", "error", err)
		}
	}()

	c, err := client.Dial("passthrough:///embedded",
		client.WithProject(cfg.project),
		client.WithDialOptions(grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		})),
	)
	if err != nil {
		srv.Stop()
		os.Remove(sock)
		_ = backend.Close()
		_ = store.Close()
		return nil, nil, err
	}
	cleanup := func() {
		_ = c.Close()
		srv.Stop()
		os.Remove(sock)
		_ = backend.Close()
		_ = store.Close()
	}
	return c, cleanup, nil
}

func cmdVersion([]string) error {
	fmt.Println("agentctl", version.Get())
	return nil
}

func cmdCreate(args []string) error {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	cfg := commonFlags(fs)
	var name, harness, model string
	fs.StringVar(&name, "name", "", "human-readable session name")
	fs.StringVar(&harness, "harness", "", "harness to run the session on (default: the host's)")
	fs.StringVar(&model, "model", "", "model id")
	_ = fs.Parse(args)
	c, cleanup, err := dial(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	sess, err := c.CreateSession(context.Background(), &v1.Session{
		Metadata: &v1.ResourceMetadata{Project: cfg.project, Name: name},
		Harness:  harness,
		Model:    model,
	})
	if err != nil {
		return err
	}
	fmt.Println(sess.GetMetadata().GetUid())
	return nil
}

// cmdList walks every page rather than printing the first one. A CLI that stopped at the default
// page size would quietly under-report, which is worse than being slow.
func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	cfg := commonFlags(fs)
	var pageSize int
	fs.IntVar(&pageSize, "page-size", 0, "sessions per request; 0 uses the server default")
	_ = fs.Parse(args)
	c, cleanup, err := dial(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	sessions, err := c.ListSessions(context.Background(), cfg.project)
	if err != nil {
		return err
	}
	for _, s := range sessions {
		fmt.Printf("%s\tlast_seq=%d\tcompute=%s\tharness=%s\tname=%s\n",
			s.GetMetadata().GetUid(),
			s.GetLastSeq(),
			strings.TrimPrefix(s.GetComputeState().String(), "COMPUTE_"),
			s.GetHarness(),
			s.GetMetadata().GetName(),
		)
	}
	return nil
}

func cmdGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	cfg := commonFlags(fs)
	var uid string
	fs.StringVar(&uid, "session", "", "session UID")
	_ = fs.Parse(args)
	c, cleanup, err := dial(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	sess, err := c.GetSession(context.Background(), uid)
	if err != nil {
		return err
	}
	fmt.Printf("session %s project=%s name=%s harness=%s last_seq=%d compute=%s %s\n",
		sess.GetMetadata().GetUid(),
		sess.GetMetadata().GetProject(),
		sess.GetMetadata().GetName(),
		sess.GetHarness(),
		sess.GetLastSeq(),
		strings.TrimPrefix(sess.GetComputeState().String(), "COMPUTE_"),
		deletionFields(sess),
	)
	return nil
}

func cmdExec(args []string) error { return runExec(args, os.Stdout) }

// runExec runs one turn and writes it to w as it happens.
func runExec(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("exec", flag.ExitOnError)
	cfg := commonFlags(fs)
	var sess, input, harness string
	fs.StringVar(&sess, "session", "", "session UID (created if empty)")
	fs.StringVar(&input, "input", "", "user input for this turn")
	fs.StringVar(&harness, "harness", "", "harness for a new session or override for this turn (default: the session's or host's)")
	_ = fs.Parse(args)
	c, cleanup, err := dial(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	// One call: an empty session is created by the server, and leaving ExpectedLastSeq nil appends
	// at the head. Records print as they commit rather than after the turn.
	out := &execOutput{w: w}
	defer out.endStream()
	_, err = c.Exec(context.Background(), client.ExecOptions{
		Session: sess,
		Inputs:  []string{input},
		Harness: harness,
		OnSession: func(s *v1.Session) {
			if sess == "" {
				out.line(fmt.Sprintf("session %s", s.GetMetadata().GetUid()))
			}
		},
		OnRecord: func(r *v1.LogRecord) { out.line(formatRecord(r)) },
		OnDelta:  out.delta,
	})
	return err
}

// execOutput writes an exec turn as it arrives: whole lines (the session UID, committed records)
// and the model's streamed text. Streamed text is as untrusted as the committed message it
// previews, so it prints through the same escapeText and on its own "stream:" line, closed before
// the next whole line: a chunk cannot end the line, act on the terminal (hiding the tool call
// record that follows), or pass for a record. Each rune is escaped on its own, so a terminal
// sequence split across chunks (ESC in one, "[8m" in the next) is still inert.
type execOutput struct {
	w         io.Writer
	streaming bool // a "stream:" line is open
}

func (o *execOutput) line(s string) {
	o.endStream()
	_, _ = fmt.Fprintln(o.w, s)
}

// delta prints a chunk on the open stream line, opening one if needed, and closes the line when the
// model's response is done. The done frame is not relied on to close it: a failed model call sends
// none, so the next whole line or the end of the turn closes it instead.
func (o *execOutput) delta(d *v1.Delta) {
	if chunk := d.GetChunk(); chunk != "" {
		if !o.streaming {
			_, _ = io.WriteString(o.w, "stream: ")
			o.streaming = true
		}
		_, _ = io.WriteString(o.w, escapeText(chunk))
	}
	if d.GetDone() {
		o.endStream()
	}
}

func (o *execOutput) endStream() {
	if o.streaming {
		_, _ = fmt.Fprintln(o.w)
		o.streaming = false
	}
}

func cmdReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	cfg := commonFlags(fs)
	var sess string
	var from int64
	var verbose bool
	fs.StringVar(&sess, "session", "", "session UID")
	fs.Int64Var(&from, "from", 1, "replay from this seq")
	fs.BoolVar(&verbose, "v", false, "also print each record's hash and previous hash, a model call's model and input hash, and token usage")
	_ = fs.Parse(args)
	c, cleanup, err := dial(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	records, err := c.Replay(context.Background(), sess, from, 0)
	if err != nil {
		return err
	}
	for _, r := range records {
		if verbose {
			fmt.Println(formatRecordVerbose(r))
		} else {
			printRecord(r)
		}
	}
	return nil
}

func cmdFork(args []string) error {
	fs := flag.NewFlagSet("fork", flag.ExitOnError)
	cfg := commonFlags(fs)
	var sess string
	var at int64
	var count int
	var names string
	fs.StringVar(&sess, "session", "", "parent session UID")
	fs.Int64Var(&at, "at", 0, "fork at this seq (0 = head)")
	fs.IntVar(&count, "count", 1, "number of children")
	fs.StringVar(&names, "names", "", "comma-separated display names for the children, in order; must match -count")
	_ = fs.Parse(args)
	c, cleanup, err := dial(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	var childNames []string
	if names != "" {
		childNames = strings.Split(names, ",")
	}
	children, err := c.Fork(context.Background(), sess, client.ForkOptions{
		AtSeq: at, Count: int32(count), Names: childNames,
	})
	if err != nil {
		return err
	}
	for _, ch := range children {
		fmt.Printf("child %s parent=%s fork_seq=%d name=%s\n", ch.GetMetadata().GetUid(), ch.GetParentUid(), ch.GetForkSeq(), ch.GetMetadata().GetName())
	}
	return nil
}

func cmdSuspend(args []string) error { return lifecycle(args, "suspend") }
func cmdResume(args []string) error  { return lifecycle(args, "resume") }

func lifecycle(args []string, which string) error {
	fs := flag.NewFlagSet(which, flag.ExitOnError)
	cfg := commonFlags(fs)
	var sess string
	fs.StringVar(&sess, "session", "", "session UID")
	_ = fs.Parse(args)
	c, cleanup, err := dial(cfg)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx := context.Background()
	var out *v1.Session
	if which == "suspend" {
		out, err = c.Suspend(ctx, sess)
	} else {
		out, err = c.Resume(ctx, sess, false)
	}
	if err != nil {
		return err
	}
	fmt.Printf("session %s %s compute=%s last_seq=%d\n", out.GetMetadata().GetUid(), which, out.GetComputeState(), out.GetLastSeq())
	return nil
}

func printRecord(r *v1.LogRecord) { fmt.Println(formatRecord(r)) }

// formatRecord is one record's line: its seq and kind, a message's text, and a tool call's tool,
// ID, mediation and arguments, so an operator can read the call an approval would authorize.
func formatRecord(r *v1.LogRecord) string {
	ev := r.GetEvent()
	line := fmt.Sprintf("seq=%-3d %s", r.GetSeq(), ev.GetKind())
	if msg := ev.GetMessage(); msg != nil {
		line += "  " + partsText(msg)
	}
	if tc := ev.GetTool(); tc != nil {
		line += "  " + toolCallText(tc)
	}
	return line
}

// formatRecordVerbose is printRecord's line plus the record's chain links (content_hash and
// prev_hash, shortened to 16 hex) and the body fields replay checks: a model call's model and input
// hash, and a usage record's tokens.
func formatRecordVerbose(r *v1.LogRecord) string {
	ev := r.GetEvent()
	line := fmt.Sprintf("seq=%-3d %-20s hash=%s prev=%s", r.GetSeq(), ev.GetKind(), short(r.GetContentHash()), short(r.GetPrevHash()))
	if exec := ev.GetExecutionId(); exec != "" {
		line += " exec=" + exec
	}
	if m := ev.GetModel(); m != nil {
		line += fmt.Sprintf("  model=%s input_hash=%s", m.GetModel(), short(m.GetInputHash()))
	}
	if u := ev.GetUsage(); u != nil {
		line += fmt.Sprintf("  model=%s input_tokens=%d output_tokens=%d reasoning_tokens=%d",
			u.GetModel(), u.GetInputTokens(), u.GetOutputTokens(), u.GetReasoningTokens())
	}
	if msg := ev.GetMessage(); msg != nil {
		line += "  " + partsText(msg)
	}
	if tc := ev.GetTool(); tc != nil {
		line += "  " + toolCallText(tc)
	}
	return line
}

// short keeps a hash readable in a line: its first 16 characters after any "sha256:" prefix, or "-"
// when it is empty (the first record has no previous hash).
func short(h string) string {
	h = strings.TrimPrefix(h, "sha256:")
	if h == "" {
		return "-"
	}
	if len(h) > 16 {
		return h[:16]
	}
	return h
}

// toolCallText shows a tool call without its idempotency key. The tool name and call ID print as
// displayToken, so a provider-chosen ID cannot add a line, hide text with terminal escapes, or fake
// another field. Arguments print as JSON with sorted keys, so the same call prints the same way every
// time, and without HTML escaping, so an approver reads <, > and & as the tool will receive them.
// Control, format and other non-graphic runes in the arguments (the C1 control U+009B, the bidi
// override U+202E) print as \uXXXX escapes, so model-chosen text cannot act on the terminal or
// visually reorder the line.
func toolCallText(tc *v1.ToolCall) string {
	args := "null"
	if tc.GetArgs() != nil {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(tc.GetArgs().AsMap()); err == nil {
			args = escapeFormatRunes(strings.TrimSuffix(buf.String(), "\n"))
		} else {
			args = "(not JSON)"
		}
	}
	return fmt.Sprintf("tool=%s id=%s mediation=%s args=%s", displayToken(tc.GetTool()), displayToken(tc.GetId()),
		strings.TrimPrefix(tc.GetMediation().String(), "MEDIATION_"), args)
}

// displayToken prints an untrusted identifier bare when it is valid UTF-8 made only of printable,
// non-space runes other than '"' and '\\'; otherwise, or when empty, it prints as a Go quoted
// string, which escapes control characters (newline, ESC), format characters (Cf) and invalid
// bytes. A bare token therefore never contains a space, so it cannot be read as two fields.
func displayToken(s string) string {
	if s != "" && utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool {
		return !unicode.IsPrint(r) || r == ' ' || r == '"' || r == '\\'
	}) {
		return s
	}
	return strconv.Quote(s)
}

// escapeFormatRunes replaces each rune in encoded JSON that is not graphic (control characters
// such as DEL and the C1 controls U+0080-U+009F, format characters such as the bidi override
// U+202E, private-use and unassigned code points) with its \uXXXX escape, as a UTF-16 surrogate pair
// above U+FFFF. encoding/json already escapes C0 controls and invalid UTF-8, so such runes occur only
// inside JSON strings, and the result is still valid JSON that decodes to the same value.
func escapeFormatRunes(s string) string {
	if !strings.ContainsFunc(s, needsEscape) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case !needsEscape(r):
			b.WriteRune(r)
		case r > 0xFFFF:
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String()
}

// needsEscape reports a rune a terminal may act on or not draw: anything outside the graphic
// categories (letters, marks, numbers, punctuation, symbols and space separators), so control (Cc),
// format (Cf), line and paragraph separator (Zl, Zp), private-use and unassigned runes.
func needsEscape(r rune) bool { return !unicode.IsGraphic(r) }

// escapeText prints untrusted message text on one line: each rune for which needsEscape holds prints
// as its Go escape (\n, \x1b, \u202e), and each invalid UTF-8 byte as \xNN. Other runes, backslash
// included, print as they are, so the output is for reading, not for decoding back.
func escapeText(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, needsEscape) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case needsEscape(r):
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// partsText is a message's role and text parts on one line. The role and text are untrusted (a model
// or provider writes an assistant message, and it is journaled just before the tool call it
// accompanies), so they print through escapeText: they cannot add a line that looks like another
// record or hide the following lines with terminal escapes.
func partsText(m *v1.Message) string {
	s := escapeText(m.GetRole()) + ":"
	for _, p := range m.GetParts() {
		if t := p.GetText(); t != nil {
			s += " " + escapeText(t.GetText())
		}
	}
	return s
}

package agentsessionsd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/client"
	"github.com/agent-primitives/sessions/controller"
)

const validToolConfig = `{"version":1,"executors":[{"name":"e","type":"unix","socket":"/run/tool.sock"}],"tools":[{"harness":"h","tool":"publish","executor":"e","durable_dedup":true}]}`

func writeToolConfig(t *testing.T, data string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tools.json")
	if err := os.WriteFile(p, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Removing validation must let a malformed start create its journal and fail this test.
func TestRunRejectsToolConfigBeforeSideEffects(t *testing.T) {
	cases := map[string]string{
		"syntax": `{`, "empty": ``, "null": `null`,
		"missing version": strings.Replace(validToolConfig, `"version":1,`, "", 1),
		"version":         strings.Replace(validToolConfig, `"version":1`, `"version":2`, 1),
		"null version":    strings.Replace(validToolConfig, `"version":1`, `"version":null`, 1),
		"unknown":         strings.Replace(validToolConfig, `"version":1`, `"version":1,"secret":"no"`, 1),
		"nested unknown":  strings.Replace(validToolConfig, `"type":"unix"`, `"type":"unix","command":"no"`, 1),
		"route unknown":   strings.Replace(validToolConfig, `"tool":"publish"`, `"tool":"publish","wildcard":true`, 1),
		"case":            strings.Replace(validToolConfig, `"version"`, `"Version"`, 1),
		"nested case":     strings.Replace(validToolConfig, `"socket"`, `"Socket"`, 1),
		"duplicate field": strings.Replace(validToolConfig, `"durable_dedup":true`, `"durable_dedup":false,"durable_dedup":true`, 1),
		"trailing":        validToolConfig + ` {}`, "invalid utf8": strings.Replace(validToolConfig, "publish", string([]byte{0xff}), 1),
		"timeout null":       strings.Replace(validToolConfig, `"type":"unix"`, `"type":"unix","timeout_ms":null`, 1),
		"timeout zero":       strings.Replace(validToolConfig, `"type":"unix"`, `"type":"unix","timeout_ms":0`, 1),
		"timeout big":        strings.Replace(validToolConfig, `"type":"unix"`, `"type":"unix","timeout_ms":300001`, 1),
		"wrong boolean":      strings.Replace(validToolConfig, `"durable_dedup":true`, `"durable_dedup":"true"`, 1),
		"unknown executor":   strings.Replace(validToolConfig, `"executor":"e"`, `"executor":"other"`, 1),
		"unknown harness":    strings.Replace(validToolConfig, `"harness":"h"`, `"harness":"other"`, 1),
		"builtin":            strings.Replace(validToolConfig, `"harness":"h"`, `"harness":"echo"`, 1),
		"type":               strings.Replace(validToolConfig, `"type":"unix"`, `"type":"tcp"`, 1),
		"relative socket":    strings.Replace(validToolConfig, `/run/tool.sock`, `tool.sock`, 1),
		"nul socket":         strings.Replace(validToolConfig, `/run/tool.sock`, `/run/\u0000.sock`, 1),
		"long socket":        strings.Replace(validToolConfig, `/run/tool.sock`, "/"+strings.Repeat("x", 103), 1),
		"empty name":         strings.Replace(validToolConfig, `"name":"e"`, `"name":""`, 1),
		"empty tool":         strings.Replace(validToolConfig, `"tool":"publish"`, `"tool":""`, 1),
		"duplicate executor": `{"version":1,"executors":[{"name":"e","type":"unix","socket":"/x"},{"name":"e","type":"unix","socket":"/y"}]}`,
		"duplicate pair":     strings.Replace(validToolConfig, `"tools":[`, `"tools":[{"harness":"h","tool":"publish","executor":"e"},`, 1),
		"oversize":           validToolConfig + strings.Repeat(" ", 1<<20),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			journal := filepath.Join(t.TempDir(), "journal.db")
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			listened := false
			err := Run([]string{"-tool-executors", writeToolConfig(t, data), "-journal", journal, "-addr", "127.0.0.1:0", "-model", "m", "-harness", "h=127.0.0.1:9000"}, withContext(ctx), withListening(func(string) { listened = true }))
			if err == nil || errors.Is(err, ErrUsage) {
				t.Fatalf("config must fail with a startup error, got %v", err)
			}
			if listened {
				t.Fatal("invalid config served")
			}
			if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid config created journal: %v", err)
			}
		})
	}
}

func TestToolConfigSocketPathLength(t *testing.T) {
	for _, socket := range []string{
		"/" + strings.Repeat("x", 102),
		"/" + strings.Repeat("x", 103),
		"/" + strings.Repeat("é", 51),
		"/" + strings.Repeat("é", 52),
	} {
		t.Run(socket, func(t *testing.T) {
			data := strings.Replace(validToolConfig, "/run/tool.sock", socket, 1)
			_, err := loadToolExecutorConfig(writeToolConfig(t, data), remoteHarnesses{"h": "127.0.0.1:9000"})
			if len(socket) <= 103 {
				if err != nil {
					t.Fatalf("valid %d-byte path rejected: %v", len(socket), err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "Unix socket path must be at most 103 bytes") {
				t.Fatalf("oversized %d-byte path: %v", len(socket), err)
			}
		})
	}
}

func TestRunValidatesToolConfigWithoutHarnesses(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, data := range []string{`{"version":2}`, validToolConfig} {
		err := Run([]string{"-tool-executors", writeToolConfig(t, data), "-journal", filepath.Join(t.TempDir(), "j.db")}, withContext(ctx))
		if err == nil || errors.Is(err, ErrUsage) {
			t.Fatalf("unvalidated config: %v", err)
		}
	}
}

func TestRunToolConfigConflictsWithProgrammaticExecutors(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	journal := filepath.Join(t.TempDir(), "j.db")
	err := Run([]string{"-tool-executors", writeToolConfig(t, `{"version":1}`), "-journal", journal}, WithToolExecutors(func(string, *client.Client) (controller.ToolFunc, error) {
		t.Fatal("must not call conflicting factory")
		return nil, nil
	}), withContext(ctx))
	if err == nil || errors.Is(err, ErrUsage) {
		t.Fatalf("want config conflict, got %v", err)
	}
	if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("conflict created journal: %v", err)
	}
}

func TestRunAcceptsToolConfigWithoutDialingAtStartup(t *testing.T) {
	for _, declaration := range []string{"true", "false", "null", "missing"} {
		t.Run(declaration, func(t *testing.T) {
			data := strings.Replace(validToolConfig, `"durable_dedup":true`, `"durable_dedup":`+declaration, 1)
			if declaration == "missing" {
				data = strings.Replace(validToolConfig, `,"durable_dedup":true`, "", 1)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			served := false
			err := Run([]string{"-tool-executors", writeToolConfig(t, data), "-journal", filepath.Join(t.TempDir(), "j.db"), "-addr", "127.0.0.1:0", "-model", "m", "-harness", "h=127.0.0.1:9000"}, withContext(ctx), withListening(func(string) { served = true; cancel() }))
			if err != nil || !served {
				t.Fatalf("valid config must start without dialing: %v, served=%v", err, served)
			}
		})
	}
}

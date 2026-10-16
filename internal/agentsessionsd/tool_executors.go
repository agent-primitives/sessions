package agentsessionsd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/client"
	"github.com/agent-primitives/sessions/controller"
)

const toolMessageLimit = 1 << 20

type toolExecutorConfig struct {
	Version   int                    `json:"version"`
	Executors []socketExecutorConfig `json:"executors"`
	Tools     []toolRoute            `json:"tools"`
}

type socketExecutorConfig struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Socket    string `json:"socket"`
	TimeoutMS *int   `json:"timeout_ms"`
}

type toolRoute struct {
	Harness      string `json:"harness"`
	Tool         string `json:"tool"`
	Executor     string `json:"executor"`
	DurableDedup *bool  `json:"durable_dedup"`
}

func loadToolExecutorConfig(path string, remotes remoteHarnesses) (*toolExecutorConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open tool executor config: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, toolMessageLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read tool executor config: %w", err)
	}
	if len(data) > toolMessageLimit {
		return nil, errors.New("tool executor config exceeds 1 MiB")
	}
	var c toolExecutorConfig
	if err := strictJSON(data, &c); err != nil {
		return nil, fmt.Errorf("tool executor config: %w", err)
	}
	if c.Version != 1 {
		return nil, errors.New("tool executor config requires version 1")
	}
	names := map[string]bool{}
	for _, e := range c.Executors {
		if e.Name == "" || names[e.Name] {
			return nil, errors.New("executor names must be nonempty and unique")
		}
		names[e.Name] = true
		if e.Type != "unix" {
			return nil, fmt.Errorf("executor %q requires type unix", e.Name)
		}
		if !filepath.IsAbs(e.Socket) || strings.ContainsRune(e.Socket, 0) {
			return nil, fmt.Errorf("executor %q requires an absolute Unix socket path without NUL", e.Name)
		}
		if e.TimeoutMS != nil && (*e.TimeoutMS < 1 || *e.TimeoutMS > 300000) {
			return nil, fmt.Errorf("executor %q timeout_ms must be 1..300000", e.Name)
		}
	}
	pairs := map[[2]string]bool{}
	for _, r := range c.Tools {
		if r.Harness == "" || r.Tool == "" || r.Executor == "" {
			return nil, errors.New("tool routes require nonempty harness, tool and executor")
		}
		if _, ok := remotes[r.Harness]; !ok {
			return nil, fmt.Errorf("tool route harness %q is not a CLI -harness", r.Harness)
		}
		if !names[r.Executor] {
			return nil, fmt.Errorf("tool route executor %q is not configured", r.Executor)
		}
		key := [2]string{r.Harness, r.Tool}
		if pairs[key] {
			return nil, errors.New("duplicate harness/tool route")
		}
		pairs[key] = true
	}
	return &c, nil
}

// factory freezes the exact allowlist at startup. Declarations are operator assertions;
// only true is dispatchable while the controller cannot distinguish first use from recovery.
func (c *toolExecutorConfig) factory(logger *slog.Logger) ToolExecutorFunc {
	executors := map[string]socketExecutorConfig{}
	for _, e := range c.Executors {
		executors[e.Name] = e
	}
	routes := map[string]map[string]toolRoute{}
	for _, r := range c.Tools {
		if routes[r.Harness] == nil {
			routes[r.Harness] = map[string]toolRoute{}
		}
		routes[r.Harness][r.Tool] = r
		declaration := "unknown"
		if r.DurableDedup != nil {
			declaration = fmt.Sprint(*r.DurableDedup)
		}
		logger.Info("configured tool executor", "harness", r.Harness, "tool", r.Tool, "executor", r.Executor, "durable_dedup", declaration)
	}
	return func(harness string, _ *client.Client) (controller.ToolFunc, error) {
		tools := routes[harness]
		if len(tools) == 0 {
			return nil, nil
		}
		return func(ctx context.Context, scope controller.ToolCallContext, call api.ToolCall) (api.ToolResult, error) {
			r, ok := tools[call.Tool]
			if !ok {
				return api.ToolResult{}, fmt.Errorf("tool executor: harness %q tool %q is not configured", harness, call.Tool)
			}
			if r.DurableDedup == nil || !*r.DurableDedup {
				return api.ToolResult{}, fmt.Errorf("tool executor: harness %q tool %q requires durable_dedup true until #139", harness, call.Tool)
			}
			return executors[r.Executor].execute(ctx, harness, scope, call)
		}, nil
	}
}

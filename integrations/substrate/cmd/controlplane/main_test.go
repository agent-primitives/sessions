package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	atepb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestParseConfigRequiresDeploymentInputs(t *testing.T) {
	if _, err := parseConfig(nil); err == nil || !strings.Contains(err.Error(), "-model is required") {
		t.Fatalf("error = %v, want missing model", err)
	}
	cfg, err := parseConfig([]string{
		"-model", "test-model",
		"-harness-image", "example/harness@sha256:" + strings.Repeat("a", 64),
		"-ateapi-insecure-skip-verify",
		"-atespace", "test-space",
		"-actor-template", "test-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.model != "test-model" || cfg.atespace != "test-space" || cfg.templateName != "test-chat" {
		t.Fatalf("config = %+v", cfg)
	}
	if !cfg.ateapiInsecureSkipVerify {
		t.Fatal("kind TLS override was not parsed")
	}
	if _, err := parseConfig([]string{"-model", "test", "-log-level", "verbose"}); err == nil {
		t.Fatal("invalid log level was accepted")
	}
}

func TestFileTokenCredentialsRereadsToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	creds := &fileTokenCredentials{path: path}
	for _, token := range []string{"first", "second"} {
		if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := creds.GetRequestMetadata(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if got["authorization"] != "Bearer "+token {
			t.Fatalf("authorization = %q", got["authorization"])
		}
	}
	if !creds.RequireTransportSecurity() {
		t.Fatal("token credentials must require TLS")
	}
}

func TestFileTokenCredentialsRejectsEmptyToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&fileTokenCredentials{path: path}).GetRequestMetadata(t.Context()); err == nil {
		t.Fatal("empty token was accepted")
	}
}

func TestEnsureAtespaceIsIdempotent(t *testing.T) {
	for _, code := range []codes.Code{codes.OK, codes.AlreadyExists} {
		t.Run(code.String(), func(t *testing.T) {
			client := &controlClient{code: code}
			if err := ensureAtespace(t.Context(), client, "agentsessions"); err != nil {
				t.Fatal(err)
			}
			if client.name != "agentsessions" {
				t.Fatalf("atespace = %q", client.name)
			}
		})
	}
	client := &controlClient{code: codes.PermissionDenied}
	if err := ensureAtespace(t.Context(), client, "agentsessions"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("error = %v, want PermissionDenied", err)
	}
}

func TestEnsureChatTemplate(t *testing.T) {
	cfg := config{templateAtespace: "templates", templateName: "chat", model: "test",
		harnessImage: "example/harness@sha256:" + strings.Repeat("a", 64)}
	for _, tc := range []struct {
		name   string
		code   codes.Code
		mutate func(*atepb.ActorTemplate)
		want   string
	}{
		{name: "create"},
		{name: "existing", code: codes.AlreadyExists},
		{name: "create denied", code: codes.PermissionDenied, want: "create chat template"},
		{name: "stale image", mutate: func(t *atepb.ActorTemplate) { t.Containers[0].Image = "stale" }, want: "immutable"},
		{name: "stale model", mutate: func(t *atepb.ActorTemplate) { t.Containers[0].Env[1].Value = "stale" }, want: "immutable"},
		{name: "wrong pool", mutate: func(t *atepb.ActorTemplate) { t.WorkerSelector.MatchLabels["workload"] = "other" }, want: "immutable"},
		{name: "wrong sandbox", mutate: func(t *atepb.ActorTemplate) { t.SandboxConfig.ConfigName = "other" }, want: "immutable"},
		{name: "build failure", mutate: func(t *atepb.ActorTemplate) { t.Status.GoldenSnapshotStatus.ErrorMessage = "build failed" }, want: "build failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := chatTemplate(cfg)
			got.Status = &atepb.ActorTemplateStatus{GoldenSnapshotStatus: &atepb.GoldenSnapshotStatus{
				GoldenTag: &atepb.ObjectRef{Atespace: "templates", Name: "golden"},
			}}
			if tc.mutate != nil {
				tc.mutate(got)
			}
			ctl := &templateClient{code: tc.code, got: got}
			err := ensureChatTemplate(t.Context(), ctl, cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if !proto.Equal(ctl.created, chatTemplate(cfg)) {
				t.Fatal("template provisioning spec changed")
			}
		})
	}
	t.Run("bounded golden wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := ensureChatTemplate(ctx, &templateClient{got: chatTemplate(cfg)}, cfg)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want canceled golden wait", err)
		}
	})
}

type templateClient struct {
	atepb.ControlClient
	code    codes.Code
	got     *atepb.ActorTemplate
	created *atepb.ActorTemplate
}

func (c *templateClient) CreateActorTemplate(_ context.Context, req *atepb.CreateActorTemplateRequest, _ ...grpc.CallOption) (*atepb.ActorTemplate, error) {
	c.created = req.GetActorTemplate()
	if c.code != codes.OK {
		return nil, status.Error(c.code, c.code.String())
	}
	return c.created, nil
}

func (c *templateClient) GetActorTemplate(_ context.Context, req *atepb.GetActorTemplateRequest, _ ...grpc.CallOption) (*atepb.ActorTemplate, error) {
	if req.GetActorTemplate().GetAtespace() != "templates" || req.GetActorTemplate().GetName() != "chat" {
		return nil, status.Error(codes.NotFound, "wrong template reference")
	}
	return c.got, nil
}

type controlClient struct {
	atepb.ControlClient
	code codes.Code
	name string
}

func (c *controlClient) CreateAtespace(_ context.Context, req *atepb.CreateAtespaceRequest, _ ...grpc.CallOption) (*atepb.Atespace, error) {
	c.name = req.GetAtespace().GetMetadata().GetName()
	if c.code != codes.OK {
		return nil, status.Error(c.code, c.code.String())
	}
	return req.GetAtespace(), nil
}

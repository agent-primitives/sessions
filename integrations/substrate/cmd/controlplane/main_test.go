package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	atepb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestSandboxAndSnapshotConfig(t *testing.T) {
	for _, tc := range []struct {
		class, custom, want string
		enum                atepb.SandboxClass
	}{
		{"microvm", "", "microvm", atepb.SandboxClass_SANDBOX_CLASS_MICROVM},
		{"gvisor", "", "gvisor-default", atepb.SandboxClass_SANDBOX_CLASS_GVISOR},
		{"microvm", "custom-vm", "custom-vm", atepb.SandboxClass_SANDBOX_CLASS_MICROVM},
		{"gvisor", "custom-gvisor", "custom-gvisor", atepb.SandboxClass_SANDBOX_CLASS_GVISOR},
	} {
		t.Run(tc.class+"/"+tc.want, func(t *testing.T) {
			args := []string{"-model", "test", "-harness-image", "example/harness@sha256:" + strings.Repeat("a", 64),
				"-sandbox-class", tc.class, "-snapshot-location", "gs://custom/snapshots"}
			if tc.custom != "" {
				args = append(args, "-sandbox-config", tc.custom)
			}
			cfg, err := parseConfig(args)
			if err != nil {
				t.Fatal(err)
			}
			spec := chatTemplate(cfg)
			if spec.GetSandboxConfig().GetSandboxClass() != tc.enum ||
				spec.GetSandboxConfig().GetConfigName() != tc.want ||
				spec.GetSnapshotConfig().GetPreferredFidelity() != atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY ||
				spec.GetWorkerSelector().GetMatchLabels()["workload"] != "agentsessions-chat-"+tc.class ||
				spec.GetSnapshotConfig().GetStorageLocation() != "gs://custom/snapshots/chat-harness/" {
				t.Fatalf("unexpected template: %v", spec)
			}
		})
	}
	for _, flag := range []string{"-sandbox-class=typo", "-sandbox-config= ", "-snapshot-location= ", "-ateapi-insecure-skip-verify"} {
		if _, err := parseConfig([]string{"-model=test", "-harness-image=example/harness@sha256:" + strings.Repeat("a", 64), flag}); err == nil {
			t.Fatalf("accepted invalid or removed flag %s", flag)
		}
	}
}

func TestATEAPITrustsOnlySuppliedBundle(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	path := filepath.Join(t.TempDir(), "ca.pem")
	cert := server.Certificate()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ateapiTLSConfig(config{ateapiCAFile: path, ateapiServerName: "api.ate-system.svc"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InsecureSkipVerify || cfg.ServerName != "api.ate-system.svc" {
		t.Fatalf("unsafe TLS configuration: %+v", cfg)
	}
	wantRoots := x509.NewCertPool()
	wantRoots.AddCert(cert)
	if !cfg.RootCAs.Equal(wantRoots) {
		t.Fatal("roots differ from supplied bundle")
	}
	if err := os.WriteFile(path, []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ateapiTLSConfig(config{ateapiCAFile: path}); err == nil {
		t.Fatal("invalid bundle accepted")
	}
	if ateapiConnectParams.Backoff.MaxDelay > 5*time.Second ||
		ateapiConnectParams.MinConnectTimeout > 5*time.Second {
		t.Fatal("ate-api reconnection budget exceeds five seconds")
	}
}

func TestParseConfigRequiresDeploymentInputs(t *testing.T) {
	if _, err := parseConfig(nil); err == nil || !strings.Contains(err.Error(), "-model is required") {
		t.Fatalf("error = %v, want missing model", err)
	}
	cfg, err := parseConfig([]string{
		"-model", "test-model",
		"-harness-image", "example/harness@sha256:" + strings.Repeat("a", 64),
		"-atespace", "test-space",
		"-actor-template", "test-chat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.model != "test-model" || cfg.atespace != "test-space" || cfg.templateName != "test-chat" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.sandboxClass != "microvm" || cfg.sandboxConfig != "microvm" {
		t.Fatalf("sandbox defaults = %s/%s", cfg.sandboxClass, cfg.sandboxConfig)
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
		harnessImage: "example/harness@sha256:" + strings.Repeat("a", 64),
		sandboxClass: "microvm", sandboxConfig: "microvm", snapshotLocation: "gs://ate-snapshots/agentsessions/"}
	for _, tc := range []struct {
		name   string
		code   codes.Code
		mutate func(*atepb.ActorTemplate)
		want   string
	}{
		{name: "create"},
		{name: "existing", code: codes.AlreadyExists},
		{name: "server-defaulted golden snapshots", mutate: func(t *atepb.ActorTemplate) {
			t.SnapshotConfig.GoldenSnapshotConfig = &atepb.GoldenSnapshotConfig{
				Mode: atepb.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED,
			}
		}},
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

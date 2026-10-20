package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	atepb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestStartupLogsModelWithoutEndpointDetails(t *testing.T) {
	cfg, err := parseConfig([]string{"-model=test-model", "-harness-image=example/harness@sha256:" + strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	transport := serveControlAPI(t, &testControlServer{cfg: cfg})
	cfg.ateapiAddr, cfg.ateapiCAFile = transport.ateapiAddr, transport.ateapiCAFile
	cfg.ateapiServerName, cfg.ateapiTokenFile = transport.ateapiServerName, transport.ateapiTokenFile
	cfg.addr = "127.0.0.1:0"
	cfg.journal = filepath.Join(t.TempDir(), "journal.db")
	cfg.modelBaseURL = "https://secret-user:secret-password@private.example/v1"
	cfg.modelPath = "/chat/completions?api-key=private-query"
	var logs bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	logger := slog.New(startupLogHandler{Handler: slog.NewTextHandler(&logs, nil), cancel: cancel})
	if err := run(ctx, cfg, logger); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "model=test-model") {
		t.Fatalf("missing startup model ID: %s", &logs)
	}
	for _, private := range []string{"secret-user", "secret-password", "private.example", "private-query"} {
		if strings.Contains(logs.String(), private) {
			t.Fatalf("startup logs contain private endpoint detail %q", private)
		}
	}
}

type startupLogHandler struct {
	slog.Handler
	cancel context.CancelFunc
}

func (h startupLogHandler) Handle(ctx context.Context, record slog.Record) error {
	err := h.Handler.Handle(ctx, record)
	if record.Message == "Sessions server listening" {
		h.cancel()
	}
	return err
}

func TestATEAPIBoundsEachCall(t *testing.T) {
	old := ateapiCallTimeout
	ateapiCallTimeout = time.Second
	t.Cleanup(func() { ateapiCallTimeout = old })
	deadlines := make(chan time.Duration, 2)
	cfg := serveControlAPI(t, &testControlServer{deadlines: deadlines})
	conn, err := dialATEAPI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for _, tc := range []struct {
		name  string
		limit time.Duration
	}{
		{"no caller deadline", 0},
		{"earlier caller deadline", 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			limit := ateapiCallTimeout
			if tc.limit != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.limit)
				defer cancel()
				limit = tc.limit
			}
			done := make(chan error, 1)
			go func() {
				_, err := atepb.NewControlClient(conn).CreateAtespace(ctx, &atepb.CreateAtespaceRequest{})
				done <- err
			}()
			select {
			case err := <-done:
				if status.Code(err) != codes.DeadlineExceeded {
					t.Fatalf("call = %v, want DeadlineExceeded", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("ate-api call did not honor its deadline")
			}
			select {
			case duration := <-deadlines:
				if duration <= 0 || duration > limit {
					t.Fatalf("server deadline in %v, want at most %v", duration, limit)
				}
			case <-time.After(time.Second):
				t.Fatal("call did not reach the ate-api server")
			}
		})
	}
}

func serveControlAPI(t *testing.T, control atepb.ControlServer) config {
	t.Helper()
	certificateServer := httptest.NewTLSServer(nil)
	defer certificateServer.Close()
	dir := t.TempDir()
	caFile, tokenFile := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: certificateServer.Certificate().Raw,
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS12, Certificates: certificateServer.TLS.Certificates,
	})))
	atepb.RegisterControlServer(srv, control)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return config{
		ateapiAddr: lis.Addr().String(), ateapiCAFile: caFile, ateapiTokenFile: tokenFile,
		ateapiServerName: certificateServer.Certificate().DNSNames[0],
	}
}

type testControlServer struct {
	atepb.UnimplementedControlServer
	cfg       config
	deadlines chan time.Duration
}

func (s *testControlServer) CreateAtespace(ctx context.Context, req *atepb.CreateAtespaceRequest) (*atepb.Atespace, error) {
	if s.deadlines != nil {
		duration := time.Duration(-1)
		if deadline, ok := ctx.Deadline(); ok {
			duration = time.Until(deadline)
		}
		s.deadlines <- duration
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return req.GetAtespace(), nil
}

func (s *testControlServer) CreateActorTemplate(_ context.Context, req *atepb.CreateActorTemplateRequest) (*atepb.ActorTemplate, error) {
	return req.GetActorTemplate(), nil
}

func (s *testControlServer) GetActorTemplate(context.Context, *atepb.GetActorTemplateRequest) (*atepb.ActorTemplate, error) {
	template := chatTemplate(s.cfg)
	template.Status = &atepb.ActorTemplateStatus{GoldenSnapshotStatus: &atepb.GoldenSnapshotStatus{
		GoldenTag: &atepb.ObjectRef{Atespace: s.cfg.templateAtespace, Name: "golden"},
	}}
	return template, nil
}

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

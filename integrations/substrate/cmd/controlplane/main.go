// Command controlplane composes the Sessions server with the real agent-substrate runtime.
//
// The generic server lifecycle lives in the root module's internal/sessionserver package. This
// command exists in the integration module because it is the dependency boundary that may import
// agent-substrate's generated ate-api client.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	atepb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/harness/chatagent"
	ateadapter "github.com/agent-primitives/sessions/integrations/substrate"
	"github.com/agent-primitives/sessions/internal/modelconfig"
	"github.com/agent-primitives/sessions/internal/sessionserver"
	"github.com/agent-primitives/sessions/internal/version"
	"github.com/agent-primitives/sessions/model/openai"
	"github.com/agent-primitives/sessions/placement"
	runtimesubstrate "github.com/agent-primitives/sessions/runtime/substrate"
	"github.com/agent-primitives/sessions/sqlitelog"
)

type config struct {
	addr             string
	journal          string
	project          string
	model            string
	modelBaseURL     string
	modelPath        string
	modelAuthHeader  string
	logLevel         string
	ateapiAddr       string
	ateapiServerName string
	ateapiTokenFile  string
	ateapiCAFile     string
	atespace         string
	templateAtespace string
	templateName     string
	harnessImage     string
	sandboxClass     string
	sandboxConfig    string
	snapshotLocation string
	showVersion      bool
}

func main() {
	cfg, err := parseConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "controlplane:", err)
		os.Exit(2)
	}
	if cfg.showVersion {
		fmt.Println("controlplane", version.Get())
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	level, err := parseLogLevel(cfg.logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "controlplane:", err)
		os.Exit(2)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if err := run(ctx, cfg, logger); err != nil {
		fmt.Fprintln(os.Stderr, "controlplane:", err)
		os.Exit(1)
	}
}

func parseConfig(args []string) (config, error) {
	var cfg config
	fs := flag.NewFlagSet("controlplane", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cfg.addr, "addr", ":8080", "address to serve the Sessions API on")
	fs.StringVar(&cfg.journal, "journal", "/data/agentsessions.db", "sqlite journal path")
	fs.StringVar(&cfg.project, "project", sqlitelog.DefaultProject, "default project (tenant)")
	fs.StringVar(&cfg.model, "model", "", "model id for the chat harness (required)")
	fs.StringVar(&cfg.modelBaseURL, "model-base-url", openai.DefaultBaseURL, "base URL of the OpenAI-compatible endpoint")
	fs.StringVar(&cfg.modelPath, "model-path", openai.DefaultPath, "completions path under the base URL; may carry a query string")
	fs.StringVar(&cfg.modelAuthHeader, "model-auth-header", "Authorization", "header carrying the credential from MODEL_API_KEY")
	fs.StringVar(&cfg.logLevel, "log-level", "info", "log level: debug, info, warn, or error")
	fs.StringVar(&cfg.ateapiAddr, "ateapi-addr", "api.ate-system.svc:443", "agent-substrate ate-api gRPC address")
	fs.StringVar(&cfg.ateapiServerName, "ateapi-server-name", "api.ate-system.svc", "TLS server name for ate-api")
	fs.StringVar(&cfg.ateapiTokenFile, "ateapi-token-file", "/var/run/secrets/tokens/ateapi-token", "projected service-account token for ate-api")
	fs.StringVar(&cfg.ateapiCAFile, "ateapi-ca-file", "", "PEM CA bundle for ate-api; empty uses system roots")
	fs.StringVar(&cfg.atespace, "atespace", "agentsessions", "atespace that owns session actors")
	fs.StringVar(&cfg.templateAtespace, "actor-template-atespace", "agentsessions", "atespace containing the chat ActorTemplate")
	fs.StringVar(&cfg.templateName, "actor-template", "chat-harness", "chat ActorTemplate name")
	fs.StringVar(&cfg.harnessImage, "harness-image", "", "digest-pinned harnessnode image for the chat ActorTemplate (required)")
	fs.StringVar(&cfg.sandboxClass, "sandbox-class", "microvm", "actor sandbox class: microvm or gvisor")
	fs.StringVar(&cfg.sandboxConfig, "sandbox-config", "", "SandboxConfig name; defaults to microvm or gvisor-default for the chosen class")
	fs.StringVar(&cfg.snapshotLocation, "snapshot-location", "gs://ate-snapshots/agentsessions/", "snapshot storage prefix; the template name is appended")
	fs.BoolVar(&cfg.showVersion, "version", false, "print the build version and exit")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if cfg.showVersion {
		return cfg, nil
	}
	if _, err := parseLogLevel(cfg.logLevel); err != nil {
		return config{}, err
	}
	required := []struct {
		name  string
		value string
	}{
		{"model", cfg.model},
		{"ateapi-addr", cfg.ateapiAddr},
		{"ateapi-server-name", cfg.ateapiServerName},
		{"ateapi-token-file", cfg.ateapiTokenFile},
		{"atespace", cfg.atespace},
		{"actor-template-atespace", cfg.templateAtespace},
		{"actor-template", cfg.templateName},
		{"harness-image", cfg.harnessImage},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return config{}, fmt.Errorf("-%s is required", field.name)
		}
	}
	if !strings.Contains(cfg.harnessImage, "@sha256:") {
		return config{}, fmt.Errorf("-harness-image must be pinned by digest")
	}
	switch cfg.sandboxClass {
	case "microvm":
		if cfg.sandboxConfig == "" {
			cfg.sandboxConfig = "microvm"
		}
	case "gvisor":
		if cfg.sandboxConfig == "" {
			cfg.sandboxConfig = "gvisor-default"
		}
	default:
		return config{}, fmt.Errorf("-sandbox-class %q: want microvm or gvisor", cfg.sandboxClass)
	}
	if strings.TrimSpace(cfg.sandboxConfig) == "" || strings.TrimSpace(cfg.snapshotLocation) == "" {
		return config{}, fmt.Errorf("-sandbox-config and -snapshot-location must not be blank")
	}
	return cfg, nil
}

func parseLogLevel(value string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(value)); err != nil {
		return 0, fmt.Errorf("invalid -log-level %q: %w", value, err)
	}
	return level, nil
}

func run(ctx context.Context, cfg config, logger *slog.Logger) error {
	modelClient, err := modelconfig.New(modelconfig.Config{
		Model:      cfg.model,
		BaseURL:    cfg.modelBaseURL,
		Path:       cfg.modelPath,
		AuthHeader: cfg.modelAuthHeader,
		APIKey:     modelAPIKey(),
	})
	if err != nil {
		return fmt.Errorf("configure model: %w", err)
	}

	conn, err := dialATEAPI(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := ensureAtespace(initCtx, atepb.NewControlClient(conn), cfg.atespace); err != nil {
		return err
	}
	if err := ensureAtespace(initCtx, atepb.NewControlClient(conn), cfg.templateAtespace); err != nil {
		return err
	}
	if err := ensureChatTemplate(initCtx, atepb.NewControlClient(conn), cfg); err != nil {
		return err
	}

	descriptor, err := (chatagent.Harness{Model: cfg.model}).Describe(ctx)
	if err != nil {
		return fmt.Errorf("describe chat harness: %w", err)
	}
	backend := runtimesubstrate.New(
		ateadapter.New(conn),
		cfg.atespace,
		runtimesubstrate.ObjectRef{Atespace: cfg.templateAtespace, Name: cfg.templateName},
		descriptor,
		runtimesubstrate.WithLogger(logger),
	)
	registry, err := chatRegistry(backend, modelClient.Model, modelClient.StreamModel, logger)
	if err != nil {
		return err
	}

	return sessionserver.ListenAndServe(ctx, cfg.addr, sessionserver.Config{
		Journal:          cfg.journal,
		Project:          cfg.project,
		ModelDescription: cfg.model + " @ " + cfg.modelBaseURL + cfg.modelPath,
	}, registry, logger)
}

func chatTemplate(cfg config) *atepb.ActorTemplate {
	class := atepb.SandboxClass_SANDBOX_CLASS_MICROVM
	if cfg.sandboxClass == "gvisor" {
		class = atepb.SandboxClass_SANDBOX_CLASS_GVISOR
	}
	return &atepb.ActorTemplate{
		Metadata: &atepb.ResourceMetadata{Atespace: cfg.templateAtespace, Name: cfg.templateName},
		WorkerSelector: &atepb.Selector{MatchLabels: map[string]string{
			"workload": "agentsessions-chat-" + cfg.sandboxClass,
		}},
		Containers: []*atepb.Container{{
			Name: "harness", Image: cfg.harnessImage, Command: []string{"/ko-app/harnessnode"},
			Env: []*atepb.EnvVar{
				{Name: "HARNESS_KIND", Value: "chat"},
				{Name: "HARNESS_MODEL", Value: cfg.model},
			},
			WakeupProbe: &atepb.ContainerWakeupProbe{
				HttpGet: &atepb.HTTPGetAction{Path: "/readyz", Port: 8081}, TimeoutSeconds: 120,
			},
			Resources: &atepb.Resources{Limits: []*atepb.Limits{
				{Name: "cpu", Quantity: "1"}, {Name: "memory", Quantity: "512Mi"},
			}},
		}},
		SnapshotConfig: &atepb.SnapshotConfig{
			PreferredFidelity: atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
			StorageLocation:   strings.TrimRight(cfg.snapshotLocation, "/") + "/" + cfg.templateName + "/",
			GoldenSnapshotConfig: &atepb.GoldenSnapshotConfig{
				Mode: atepb.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED,
			},
		},
		SandboxConfig: &atepb.SandboxConfig{
			SandboxClass: class, ConfigName: cfg.sandboxConfig,
		},
	}
}

func ensureChatTemplate(ctx context.Context, ctl atepb.ControlClient, cfg config) error {
	want := chatTemplate(cfg)
	ref := &atepb.ObjectRef{Atespace: cfg.templateAtespace, Name: cfg.templateName}
	_, err := ctl.CreateActorTemplate(ctx, &atepb.CreateActorTemplateRequest{ActorTemplate: want})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return fmt.Errorf("create chat template: %w", err)
	}
	for {
		got, err := ctl.GetActorTemplate(ctx, &atepb.GetActorTemplateRequest{ActorTemplate: ref})
		if err != nil {
			return fmt.Errorf("get chat template: %w", err)
		}
		if !proto.Equal(got.GetWorkerSelector(), want.GetWorkerSelector()) ||
			!proto.Equal(got.GetSandboxConfig(), want.GetSandboxConfig()) ||
			!proto.Equal(got.GetSnapshotConfig(), want.GetSnapshotConfig()) ||
			len(got.GetContainers()) != 1 || !proto.Equal(got.GetContainers()[0], want.GetContainers()[0]) {
			return fmt.Errorf("chat template %s/%s differs from requested image, model, or sandbox; templates are immutable, use a new -actor-template name", ref.GetAtespace(), ref.GetName())
		}
		golden := got.GetStatus().GetGoldenSnapshotStatus()
		if msg := golden.GetErrorMessage(); msg != "" {
			return fmt.Errorf("chat template golden snapshot failed: %s", msg)
		}
		if golden.GetGoldenTag().GetName() != "" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for chat template golden snapshot: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

func chatRegistry(runtime *runtimesubstrate.Backend, modelFn controller.ModelFunc, streamFn controller.StreamFunc, logger *slog.Logger) (*placement.Registry, error) {
	registry, err := placement.NewRegistry("chat", map[string]*placement.Placer{
		"chat": placement.New(runtime, modelFn,
			placement.WithLogger(logger),
			placement.WithStreamingModel(streamFn),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("build harness registry: %w", err)
	}
	return registry, nil
}

func ateapiTLSConfig(cfg config) (*tls.Config, error) {
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: cfg.ateapiServerName,
	}
	if cfg.ateapiCAFile != "" {
		pem, err := os.ReadFile(cfg.ateapiCAFile)
		if err != nil {
			return nil, fmt.Errorf("read ate-api CA file %q: %w", cfg.ateapiCAFile, err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ate-api CA file %q contains no certificates", cfg.ateapiCAFile)
		}
		tlsConfig.RootCAs = roots
	}
	return tlsConfig, nil
}

var ateapiConnectParams = grpc.ConnectParams{
	Backoff: backoff.Config{
		BaseDelay: time.Second, Multiplier: 1.6, Jitter: 0.2, MaxDelay: 5 * time.Second,
	},
	MinConnectTimeout: 5 * time.Second,
}

func dialATEAPI(cfg config) (*grpc.ClientConn, error) {
	tlsConfig, err := ateapiTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(cfg.ateapiAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
		grpc.WithPerRPCCredentials(&fileTokenCredentials{path: cfg.ateapiTokenFile}),
		grpc.WithConnectParams(ateapiConnectParams),
	)
	if err != nil {
		return nil, fmt.Errorf("dial ate-api %s: %w", cfg.ateapiAddr, err)
	}
	return conn, nil
}

func ensureAtespace(ctx context.Context, ctl atepb.ControlClient, name string) error {
	_, err := ctl.CreateAtespace(ctx, &atepb.CreateAtespaceRequest{
		Atespace: &atepb.Atespace{Metadata: &atepb.ResourceMetadata{Name: name}},
	})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return fmt.Errorf("ensure atespace %q: %w", name, err)
	}
	return nil
}

type fileTokenCredentials struct{ path string }

func (c *fileTokenCredentials) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	token, err := os.ReadFile(c.path)
	if err != nil {
		return nil, fmt.Errorf("read ate-api token file %q: %w", c.path, err)
	}
	value := strings.TrimSpace(string(token))
	if value == "" {
		return nil, fmt.Errorf("ate-api token file %q is empty", c.path)
	}
	return map[string]string{"authorization": "Bearer " + value}, nil
}

func (*fileTokenCredentials) RequireTransportSecurity() bool { return true }

func modelAPIKey() string {
	if key := os.Getenv("MODEL_API_KEY"); key != "" {
		return key
	}
	return os.Getenv("OPENAI_API_KEY")
}

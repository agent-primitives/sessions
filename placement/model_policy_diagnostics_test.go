package placement_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/observability"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/local"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// Removing the diagnostic loses the only operator evidence before the public error is sanitized.
// Logging the backend error instead leaks credentials, even for an in-process harness that wraps
// a storage sentinel: errors.Is is not evidence of a trusted storage boundary.
func TestModelPolicyDescribeDiagnosticIsCorrelatedAndPayloadFree(t *testing.T) {
	const credential = "https://provider.invalid?api_key=sk-provider-private"
	for _, suppliedID := range []string{"policy-request", ""} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("id=%s/canceled=%t", suppliedID, canceled), func(t *testing.T) {
				var output bytes.Buffer
				logger := slog.New(slog.NewJSONHandler(&output, nil))
				h := &policyHarness{err: fmt.Errorf("%s: %w", credential, sqlitelog.ErrHarnessNotFound)}
				b := local.New(h)
				t.Cleanup(func() { _ = b.Close() })
				p := placement.New(b, nil, placement.WithLogger(logger),
					placement.WithModelPolicy(api.ModelPolicy{Default: "m", Allowed: []string{"m"}}))
				ctx := context.Context(t.Context())
				if suppliedID != "" {
					ctx = observability.WithRequestID(ctx, suppliedID)
				}
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				if canceled {
					cancel()
				}
				err := p.ValidateModelPolicy(ctx)
				want, kind := placement.ErrHarnessUnavailable, "harness_unavailable"
				if canceled {
					want, kind = placement.ErrAdmissionInterrupted, "canceled"
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("caller cancellation lost: %v", err)
					}
				}
				if !errors.Is(err, want) || strings.Contains(err.Error(), credential) {
					t.Fatalf("public error = %v, want sanitized %v", err, want)
				}
				if strings.Contains(output.String(), credential) || strings.Contains(output.String(), "sk-provider-private") {
					t.Fatalf("provider credentials logged: %s", &output)
				}
				var record map[string]any
				if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
					t.Fatalf("missing single operator diagnostic: %q (%v)", &output, err)
				}
				id, _ := record["request_id"].(string)
				if id == "" || (suppliedID != "" && id != suppliedID) {
					t.Fatalf("uncorrelated diagnostic: %v", record)
				}
				if record["method"] != "/agentsessions.v1.Harness/Describe" || record["error_kind"] != kind || record["error_category"] != "external" {
					t.Fatalf("diagnostic classification: %v", record)
				}
				if _, exists := record["error"]; exists {
					t.Fatalf("untrusted error text logged: %v", record)
				}
				before := output.String()
				if again := p.ValidateModelPolicy(t.Context()); again != err || h.describes.Load() != 1 || output.String() != before {
					t.Fatalf("cached failure was changed/retried/relogged: %v, describes=%d, log=%s", again, h.describes.Load(), &output)
				}
			})
		}
	}
}

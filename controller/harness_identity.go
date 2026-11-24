package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/agent-primitives/sessions/api"
)

const describeTimeout = 10 * time.Second

// Bound remote Describe as placement does, without extending an earlier caller deadline.
func describeHarness(ctx context.Context, har api.Harness) (api.Descriptor, error) {
	ctx, cancel := context.WithTimeout(ctx, describeTimeout)
	defer cancel()
	desc, err := har.Describe(ctx)
	if err != nil {
		return api.Descriptor{}, fmt.Errorf("%w: %w", ErrHarnessDescribeFailed, err)
	}
	return desc, nil
}

func (c *Controller) resolvedHarnessName(desc api.Descriptor) string {
	if c.harnessName != "" {
		return c.harnessName
	}
	return desc.ID
}

// checkHarness preflights the selected invocations against the single supplied harness. Recovery
// of legacy invocations without identity evidence does not introduce a Describe dependency;
// reconstruction always describes a nonempty selection so it can enforce the harness class.
// Version matching is opt-in per invocation; recorded nonempty versions require an exact match,
// even if served empty.
func (c *Controller) checkHarness(ctx context.Context, har api.Harness, executions []recordedExecution, reconstruction bool) (*api.Descriptor, error) {
	needsDescription := reconstruction && len(executions) > 0
	for _, execution := range executions {
		if execution.harness != "" || execution.harnessVersion != "" || execution.hasApproval() {
			needsDescription = true
			break
		}
	}
	if !needsDescription {
		return nil, nil
	}
	desc, err := describeHarness(ctx, har)
	if err != nil {
		return nil, err
	}
	name := c.resolvedHarnessName(desc)
	for _, execution := range executions {
		if execution.harness != "" && execution.harness != name {
			return nil, fmt.Errorf("%w: execution %q recorded %q, supplied %q", ErrHarnessMismatch, execution.id, execution.harness, name)
		}
		if execution.harnessVersion != "" && execution.harnessVersion != desc.Version {
			return nil, fmt.Errorf("%w: execution %q recorded %q, supplied %q", ErrHarnessVersionMismatch, execution.id, execution.harnessVersion, desc.Version)
		}
	}
	if err := api.ValidateDescriptor(desc); err != nil {
		return nil, err
	}
	for _, execution := range executions {
		if execution.hasApproval() && !approvalTier(desc.Capabilities.Resumability) {
			return nil, ErrApprovalUnavailable
		}
	}
	if reconstruction {
		switch desc.HarnessClass {
		case "", api.HarnessClassMediated:
			// An omitted class is the compatibility form emitted by older mediated harnesses.
		case api.HarnessClassUnmediated:
			return nil, fmt.Errorf("%w: harness %q declares %s", ErrReconstructionUnsupported, name, desc.HarnessClass)
		default:
			return nil, fmt.Errorf("%w: harness %q declares unknown class %q", ErrReconstructionUnsupported, name, desc.HarnessClass)
		}
	}
	return &desc, nil
}

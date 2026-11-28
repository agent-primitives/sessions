package controller

import (
	"errors"
	"fmt"

	"github.com/agent-primitives/sessions/api"
)

// recordedModelResult returns the provider-result OUTPUT immediately after call. found is false
// without consuming anything when the next effect is direct output or another effect. Explicitly
// contradictory correlation evidence is an error and must be latched by the caller.
func recordedModelResult(stream []api.Event, i int, call *api.ModelCall) (out api.Event, found bool, err error) {
	if call == nil {
		return api.Event{}, false, errors.New("recorded model call has no payload")
	}
	switch call.ResultCorrelation {
	case api.ModelResultCorrelationUnspecified, api.ModelResultCorrelationByID:
	default:
		return api.Event{}, false, fmt.Errorf("unknown model result correlation mode %d", call.ResultCorrelation)
	}
	if call.ResultCorrelation == api.ModelResultCorrelationByID && call.ID == "" {
		return api.Event{}, false, errors.New("ID-correlated model call has no ID")
	}
	if i >= len(stream) || stream[i].Kind != api.EventOutput {
		return api.Event{}, false, nil
	}
	out = stream[i]
	if call.ResultCorrelation == api.ModelResultCorrelationUnspecified && out.ModelCallID == nil {
		return out, true, nil // legacy positional completion
	}
	if out.ModelCallID == nil {
		return api.Event{}, false, nil // direct output after a failed ID-correlated call
	}
	if *out.ModelCallID == "" {
		return api.Event{}, false, errors.New("model result has an empty call ID")
	}
	if call.ID == "" || *out.ModelCallID != call.ID {
		return api.Event{}, false, fmt.Errorf("model result call ID %q does not match %q", *out.ModelCallID, call.ID)
	}
	return out, true, nil
}

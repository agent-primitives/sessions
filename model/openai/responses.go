package openai

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/agent-primitives/sessions/api"
)

// ResponsesClient calls the stateless Responses protocol. It shares transport options
// with Client, but does not change the Chat Completions client or infer a protocol.
type ResponsesClient struct{ client *Client }

// NewResponses builds a Responses client. The /responses default is applied before
// caller options, so WithPath still overrides it. A default model is required.
func NewResponses(opts ...Option) (*ResponsesClient, error) {
	c, err := New(append([]Option{WithPath("/responses")}, opts...)...)
	if err != nil {
		return nil, err
	}
	return &ResponsesClient{client: c}, nil
}

// Endpoint returns the configured Responses endpoint. Redact secrets before logging.
func (c *ResponsesClient) Endpoint() string { return c.client.Endpoint() }

// Model performs one stateless Responses call. It satisfies controller.ModelFunc.
func (c *ResponsesClient) Model(ctx context.Context, req api.ModelRequest) (api.ModelResponse, error) {
	model, body, err := c.buildResponsesRequest(req, false)
	if err != nil {
		return api.ModelResponse{}, err
	}
	resp, err := c.client.post(ctx, body)
	if err != nil {
		return api.ModelResponse{}, responsesIOError("request", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return api.ModelResponse{}, responsesHTTPError(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
	if err != nil {
		return api.ModelResponse{}, responsesIOError("read response", err)
	}
	if len(raw) > 32<<20 {
		return api.ModelResponse{}, fmt.Errorf("openai responses: response exceeds 32 MiB")
	}
	return decodeResponses(model, raw)
}

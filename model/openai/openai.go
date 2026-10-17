// Package openai is a model provider for OpenAI-compatible Chat Completions endpoints.
//
// It speaks the HTTP wire protocol directly and depends on no vendor SDK, which is what keeps it
// in the core module: the protocol is an interoperable format that many endpoints implement, not a
// vendor. Point it at a different base URL and it drives a different service, including a
// self-hosted one or a gateway that fronts a provider with a non-compatible native API.
//
// Endpoints vary in two ways that are not about the protocol at all: where the completions path
// sits, and how the request authenticates. WithPath and WithHeader cover both, so an endpoint that
// scopes the model into its path, pins an API version with a query string, or authenticates with
// something other than a bearer token is reached by configuration rather than by code.
//
// The limit is the request body. An endpoint that accepts the chat-completions body works here; one
// with its own body or signing scheme (a native messages API, or a cloud that signs requests) is a
// different protocol, and the seam for those is controller.ModelFunc, which this type satisfies.
//
// Client.Model satisfies controller.ModelFunc, so it plugs straight into placement.New.
//
// It is used only on the LIVE path. Replay serves recorded completions from the journal and never
// reaches a provider, which is what makes a replayed session free and byte-identical; see
// controller.Controller.Replay.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/agent-primitives/sessions/api"
)

// DefaultBaseURL is the public OpenAI endpoint. Any compatible service works by overriding it.
const DefaultBaseURL = "https://api.openai.com/v1"

// DefaultPath is where the completions endpoint sits under the base URL. Endpoints that scope the
// model into the path, or that pin an API version with a query string, override it.
const DefaultPath = "/chat/completions"

// defaultTimeout bounds a request that the caller did not bound itself. A model call that hangs
// forever holds the turn's single writer slot open, so an unbounded default would turn one wedged
// request into a session that can never be advanced.
const defaultTimeout = 2 * time.Minute

// ErrUnsupportedPart reports unmapped request semantics or malformed/unsupported tool content.
// Refusing rather than dropping it keeps the neutral request and recorded completion faithful
// to the supported Chat Completions mapping.
var ErrUnsupportedPart = errors.New("openai: unsupported content part")

// Client calls an OpenAI-compatible Chat Completions endpoint.
type Client struct {
	baseURL  string
	path     string
	endpoint string // baseURL joined with path by New
	headers  map[string]string
	model    string
	http     *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL points the client at a compatible endpoint other than the default. It must be an
// absolute URL. Its query string is kept and sent with every request, ahead of any query the path
// carries; a fragment is dropped, as HTTP never sends one.
func WithBaseURL(u string) Option {
	return func(c *Client) {
		if u != "" {
			c.baseURL = strings.TrimSuffix(u, "/")
		}
	}
}

// WithAPIKey sets a bearer token. It is sugar for the Authorization header most compatible
// endpoints expect; an endpoint that authenticates differently uses WithHeader instead. Empty
// sends no header at all, which is what a local endpoint or a gateway that injects its own
// credential wants.
func WithAPIKey(k string) Option {
	return func(c *Client) {
		if k != "" {
			c.headers["Authorization"] = "Bearer " + k
		}
	}
}

// WithHeader sets a request header, replacing any previous value for the same name. It is how an
// endpoint that authenticates outside the bearer convention is reached (an api-key or x-api-key
// header), and how a gateway that needs routing or tenant headers is satisfied. Headers are sent
// only to the base URL's origin (scheme, host and port): a redirect to any other origin is refused
// rather than followed, so a credential set here never reaches a host the caller did not configure.
//
// This exists because those endpoints differ from OpenAI only in the envelope: the request body
// they accept is identical, so requiring a separate provider implementation for them would be a
// recompile in exchange for nothing.
func WithHeader(name, value string) Option {
	return func(c *Client) {
		if name != "" {
			c.headers[name] = value
		}
	}
}

// WithPath overrides the path appended to the base URL. It may carry a query string, which is what
// an endpoint that pins an API version needs; New refuses a query parameter that the base URL's
// query also sets.
func WithPath(p string) Option {
	return func(c *Client) {
		if p != "" {
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			c.path = p
		}
	}
}

// WithModel sets the model used when a request does not name one.
func WithModel(m string) Option {
	return func(c *Client) {
		if m != "" {
			c.model = m
		}
	}
}

// WithHTTPClient supplies the HTTP client, for custom transports, proxies, or tests. The client is
// copied, not modified: the copy refuses redirects that leave the base URL's origin and then
// applies h.CheckRedirect, if any, to the redirects it allows.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// New builds a Client. A default model is required to identify fallback provider calls and usage.
// The journal still hashes the original neutral request: callers needing an explicit MODEL_CALL
// identity should set Model rather than rely on fallback. A non-absolute base URL, invalid path,
// or query parameter set in both the base URL and path is an error.
func New(opts ...Option) (*Client, error) {
	c := &Client{
		baseURL: DefaultBaseURL,
		path:    DefaultPath,
		headers: map[string]string{},
		http:    &http.Client{Timeout: defaultTimeout},
	}
	for _, o := range opts {
		o(c)
	}
	if c.model == "" {
		return nil, errors.New("openai: a default model is required (WithModel)")
	}
	endpoint, err := joinEndpoint(c.baseURL, c.path)
	if err != nil {
		return nil, err
	}
	c.endpoint = endpoint
	c.http = sameOriginClient(c.http)
	return c, nil
}

// Endpoint returns the URL every completions request is sent to: the base URL's path joined with
// the path, the base URL's query ahead of the path's, and no fragment. It carries whatever userinfo
// and query the base URL has, so redact it (url.URL.Redacted) before logging.
func (c *Client) Endpoint() string { return c.endpoint }

// joinEndpoint appends path to base's path and keeps both query strings, base's first. Joining the
// strings instead would put the path after a query or fragment in base, sending
// https://h/v1?api-version=2#x/chat/completions to /v1 with the path lost in the fragment.
//
// A parameter both queries set is refused rather than sent twice: which copy an endpoint reads
// differs between servers, so an api-version in both could select a version neither one names.
// Only names are compared, and the error quotes the name alone, since a value may be a secret.
func joinEndpoint(base, path string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("openai: base URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("openai: base URL %q is not absolute (want scheme://host[/path])", base)
	}
	p, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("openai: path: %w", err)
	}
	if key := sharedQueryKey(u.RawQuery, p.RawQuery); key != "" {
		return "", fmt.Errorf("openai: query parameter %q is set in both the base URL and the path; set it in one of them", key)
	}
	joined := *u
	joined.Path = strings.TrimSuffix(u.Path, "/") + p.Path
	joined.RawPath = strings.TrimSuffix(u.EscapedPath(), "/") + p.EscapedPath()
	switch {
	case u.RawQuery == "":
		joined.RawQuery = p.RawQuery
	case p.RawQuery != "":
		joined.RawQuery = u.RawQuery + "&" + p.RawQuery
	}
	joined.ForceQuery = false
	joined.Fragment, joined.RawFragment = "", ""
	return joined.String(), nil
}

// sharedQueryKey returns the first parameter name, in a's order, that both raw queries set, or "".
// Names are compared after decoding, so api-version and api%2Dversion are the same parameter. A
// pair that does not decode is compared as far as it parses; the query is still sent as given.
func sharedQueryKey(a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	bq, _ := url.ParseQuery(b)
	for _, pair := range strings.Split(a, "&") {
		name, _, _ := strings.Cut(pair, "=")
		if name, err := url.QueryUnescape(name); err == nil && name != "" {
			if _, ok := bq[name]; ok {
				return name
			}
		}
	}
	return ""
}

// maxRedirects is net/http's default redirect limit, which setting CheckRedirect replaces.
const maxRedirects = 10

// sameOriginClient returns a shallow copy of h whose redirect policy refuses any redirect that
// leaves the origin of the original request.
//
// net/http copies a request's headers onto every redirect it follows and strips only
// Authorization, WWW-Authenticate and Cookie, and only when the target is neither the same host nor
// a subdomain of it (another port, or a subdomain, keeps even those), so a custom credential header
// (api-key, x-api-key) would otherwise reach whatever host a provider or proxy redirects to. A 307
// or 308 also resends the body. Refusing the hop is the only policy that keeps every configured
// header on the configured origin.
//
// h is copied rather than modified because the caller may share it with other code. A policy h
// already has runs after this one allows the redirect, and replaces the default limit, as it would
// on h itself.
func sameOriginClient(h *http.Client) *http.Client {
	cp := *h
	next := h.CheckRedirect
	cp.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		from, to := via[0].URL, req.URL
		if origin(from) != origin(to) {
			// Name the origins, never a header value: the headers carry the credential.
			return fmt.Errorf("refusing redirect from %s://%s to %s://%s: configured headers are sent only to the base URL's origin",
				from.Scheme, from.Host, to.Scheme, to.Host)
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return nil
	}
	return &cp
}

// origin returns u's scheme, host and port in a comparable form. Scheme and host compare
// case-insensitively, and an explicit default port matches an omitted one, so
// https://Host:443 and https://host are the same origin.
func origin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

// Model performs one live completion. It satisfies controller.ModelFunc.
//
// ctx carries the turn's cancellation and deadline through to the HTTP request, so a cancelled
// execution abandons an in-flight completion rather than leaving it to finish unobserved.
func (c *Client) Model(ctx context.Context, req api.ModelRequest) (api.ModelResponse, error) {
	model, body, err := c.buildRequest(req, false)
	if err != nil {
		return api.ModelResponse{}, err
	}
	resp, err := c.post(ctx, body)
	if err != nil {
		return api.ModelResponse{}, fmt.Errorf("openai: %s: %w", model, err)
	}
	defer resp.Body.Close()

	// Bound the read: a compatible-looking endpoint that streams unbounded bytes would otherwise
	// grow the process until it dies, and a completion is small.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return api.ModelResponse{}, fmt.Errorf("openai: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return api.ModelResponse{}, statusError(model, resp.StatusCode, raw)
	}

	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return api.ModelResponse{}, fmt.Errorf("openai: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return api.ModelResponse{}, fmt.Errorf("openai: %s returned no choices", model)
	}
	choice := out.Choices[0]
	if err := checkToolFinish(choice.FinishReason, choice.Message.ToolCalls, req.ToolChoice); err != nil {
		return api.ModelResponse{}, err
	}
	return toModelResponse(model, choice.Message, out.Usage.toAPI(model))
}

// StreamModel performs one live completion, reporting text as it arrives. It satisfies
// controller.StreamFunc.
//
// onChunk sees partial output; the returned response is the complete message, which is what the
// host records. That split is deliberate: the chunks are a view of the call in progress, and the
// journal must not depend on whether anyone was watching.
func (c *Client) StreamModel(ctx context.Context, req api.ModelRequest, onChunk func(string)) (api.ModelResponse, error) {
	model, body, err := c.buildRequest(req, true)
	if err != nil {
		return api.ModelResponse{}, err
	}
	resp, err := c.post(ctx, body)
	if err != nil {
		return api.ModelResponse{}, fmt.Errorf("openai: %s: %w", model, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return api.ModelResponse{}, statusError(model, resp.StatusCode, raw)
	}

	var text, reasoning strings.Builder
	calls := map[int]*toolDeltaBuffer{}
	var finishReason string
	var finalized api.ModelResponse
	usage := api.Usage{Model: model}
	finalize := func(reason string) (api.ModelResponse, error) {
		message := chatCompletionMessage{Content: text.String(), ReasoningContent: reasoning.String()}
		// Never allocate a slice from a provider-controlled maximum index.
		for i := 0; i < len(calls); i++ {
			call := calls[i]
			if call == nil {
				return api.ModelResponse{}, fmt.Errorf("%w: noncontiguous tool call indexes", ErrUnsupportedPart)
			}
			wire := chatToolCall{ID: call.id, Type: call.kind}
			wire.Function.Name, wire.Function.Arguments = call.name, call.args.String()
			message.ToolCalls = append(message.ToolCalls, wire)
		}
		if err := checkToolFinish(reason, message.ToolCalls, req.ToolChoice); err != nil {
			return api.ModelResponse{}, err
		}
		return toModelResponse(model, message, usage)
	}

	scanner := bufio.NewScanner(resp.Body)
	// A single event can exceed the default 64KB line budget on a long completion, which would
	// otherwise fail the turn partway with a misleading "token too long".
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // comments, blank separators, and any event: lines
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			break
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return api.ModelResponse{}, fmt.Errorf("openai: decode stream chunk: %w", err)
		}
		if u := chunk.Usage; u != nil {
			usage = u.toAPI(model)
		}
		if len(chunk.Choices) == 0 {
			continue // usage-only frames carry no choice
		}
		choice := chunk.Choices[0]
		d := choice.Delta
		if finishReason != "" {
			// A terminal choice freezes semantic output, but not trailing usage.
			// Refuse before appending fragments or exposing text to the callback.
			if d.Content != "" || d.ReasoningContent != "" || len(d.ToolCalls) != 0 || legacyFunctionCall(d.FunctionCall) {
				return api.ModelResponse{}, fmt.Errorf("%w: semantic delta after completion finish", ErrUnsupportedPart)
			}
			if choice.FinishReason != "" && choice.FinishReason != finishReason {
				return api.ModelResponse{}, fmt.Errorf("%w: contradictory completion finish reason", ErrUnsupportedPart)
			}
			continue
		}
		if legacyFunctionCall(d.FunctionCall) {
			return api.ModelResponse{}, fmt.Errorf("%w: legacy function_call is not supported", ErrUnsupportedPart)
		}
		if err := appendToolDeltas(calls, d.ToolCalls); err != nil {
			return api.ModelResponse{}, err
		}
		if d.ReasoningContent != "" {
			reasoning.WriteString(d.ReasoningContent)
		}
		if d.Content != "" {
			text.WriteString(d.Content)
			if onChunk != nil {
				onChunk(d.Content)
			}
		}
		if choice.FinishReason != "" {
			// The finish-bearing frame can carry the final argument suffix itself.
			finalized, err = finalize(choice.FinishReason)
			if err != nil {
				return api.ModelResponse{}, err
			}
			finishReason = choice.FinishReason
		}
	}
	if err := scanner.Err(); err != nil {
		return api.ModelResponse{}, fmt.Errorf("openai: read stream: %w", err)
	}

	if finishReason == "" {
		// Preserve text-only streams without a finish reason; tool calls still require one.
		finalized, err = finalize("")
		if err != nil {
			return api.ModelResponse{}, err
		}
	}
	finalized.Usage = usage
	return finalized, nil
}

// buildRequest resolves the model and encodes the body. Both call paths share it so a streamed turn
// and a non-streamed one cannot disagree about what was sent.
func (c *Client) buildRequest(req api.ModelRequest, stream bool) (string, []byte, error) {
	model := req.Model
	if model == "" || model == "echo" {
		// "echo" is the reference harness's placeholder id. Treating it as "unset" lets the sample
		// harnesses run against a real endpoint without every one of them hard-coding a model.
		model = c.model
	}
	if len(req.Params) != 0 {
		return model, nil, fmt.Errorf("%w: Params are not mapped to Chat Completions", ErrUnsupportedPart)
	}
	tools, err := toChatTools(req.Tools)
	if err != nil {
		return model, nil, err
	}
	choice, err := toChatToolChoice(req.ToolChoice)
	if err != nil {
		return model, nil, err
	}
	msgs, err := toChatMessages(req.Messages)
	if err != nil {
		return model, nil, err
	}
	body := chatRequest{Model: model, Messages: msgs, Tools: tools, ToolChoice: choice}
	if stream {
		body.Stream = true
		body.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return model, nil, fmt.Errorf("%w: encode request: %w", ErrUnsupportedPart, err)
	}
	return model, raw, nil
}

// post sends the encoded body with the configured path and headers.
func (c *Client) post(ctx context.Context, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for name, value := range c.headers {
		httpReq.Header.Set(name, value)
	}
	return c.http.Do(httpReq)
}

// statusError turns a non-200 into an error carrying the provider's own message, which is usually
// far more actionable than the status code alone (a wrong model id and a missing key are both 4xx).
func statusError(model string, code int, raw []byte) error {
	var e errorResponse
	if err := json.Unmarshal(raw, &e); err == nil && e.Error.Message != "" {
		return fmt.Errorf("openai: %s: %s (http %d)", model, e.Error.Message, code)
	}
	detail := strings.TrimSpace(string(raw))
	if len(detail) > 200 {
		detail = detail[:200] + "..."
	}
	if detail == "" {
		return fmt.Errorf("openai: %s: http %d", model, code)
	}
	return fmt.Errorf("openai: %s: http %d: %s", model, code, detail)
}

// toModelResponse is shared by streaming and nonstreaming so the journal receives the same
// finalized parts. Reasoning is recorded only when supplied; tool arguments enter the existing
// JSON-native map domain (float64 numbers), not a lexical JSON/whitespace representation.
func toModelResponse(model string, message chatCompletionMessage, usage api.Usage) (api.ModelResponse, error) {
	if legacyFunctionCall(message.FunctionCall) {
		return api.ModelResponse{}, fmt.Errorf("%w: legacy function_call is not supported", ErrUnsupportedPart)
	}
	calls, err := toolCallParts(message.ToolCalls)
	if err != nil {
		return api.ModelResponse{}, err
	}
	parts := make([]api.Part, 0, 2+len(calls))
	if r := message.ReasoningContent; r != "" {
		parts = append(parts, api.Part{Reasoning: &api.ReasoningPart{Provider: "openai", ModelID: model, Opaque: []byte(r)}})
	}
	if t := message.Content; t != "" {
		parts = append(parts, api.Part{Text: &api.TextPart{Text: t}})
	}
	parts = append(parts, calls...)
	return api.ModelResponse{Message: api.Message{Role: "assistant", Parts: parts}, Usage: usage}, nil
}

// ---- wire types ----

type chatRequest struct {
	Model      string               `json:"model"`
	Messages   []chatMessage        `json:"messages"`
	Tools      []chatToolDefinition `json:"tools,omitempty"`
	ToolChoice any                  `json:"tool_choice,omitempty"`
	Stream     bool                 `json:"stream,omitempty"`
	// Include usage so streaming does not silently lose accounting.
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content          string          `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			ToolCalls        []chatToolDelta `json:"tool_calls"`
			FunctionCall     json.RawMessage `json:"function_call"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatCompletionMessage struct {
	Content string `json:"content"`
	// Compatible endpoints may supply reasoning text; never synthesize it when absent.
	ReasoningContent string          `json:"reasoning_content"`
	ToolCalls        []chatToolCall  `json:"tool_calls"`
	FunctionCall     json.RawMessage `json:"function_call"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatCompletionMessage `json:"message"`
		FinishReason string                `json:"finish_reason"`
	} `json:"choices"`
	Usage chatUsage `json:"usage"`
}

type chatUsage struct {
	PromptTokens            int64 `json:"prompt_tokens"`
	CompletionTokens        int64 `json:"completion_tokens"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u chatUsage) toAPI(model string) api.Usage {
	return api.Usage{Model: model, InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens, ReasoningTokens: u.CompletionTokensDetails.ReasoningTokens}
}

type errorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

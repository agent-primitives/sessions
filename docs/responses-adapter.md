# Responses model adapter

`model/openai.NewResponses` explicitly selects the OpenAI-compatible Responses protocol. It exposes
`Model` and `StreamModel` for the controller seams, without a vendor SDK. `openai.New` remains the
Chat Completions constructor; changing its path to `/responses` does not change its request format.
The daemon and CLI still select Chat Completions. Responses selection in those hosts is not provided.

## Construct a client

<!-- doccheck: file -->
```go
package example

import (
    "net/http"

    "github.com/agent-primitives/sessions/controller"
    "github.com/agent-primitives/sessions/model/openai"
)

func responses(transport http.RoundTripper) (controller.ModelFunc, controller.StreamFunc, error) {
    client, err := openai.NewResponses(
        openai.WithBaseURL("https://example.openai.azure.com/openai/v1"),
        openai.WithModel("deployment-name"),
        openai.WithHTTPClient(&http.Client{Transport: transport}),
    )
    if err != nil {
        return nil, nil, err
    }
    return client.Model, client.StreamModel, nil
}
```

The default path is `/responses`. Existing `WithBaseURL`, `WithPath`, `WithModel`, `WithAPIKey`,
`WithHeader` and `WithHTTPClient` options apply. Azure's v1 form is
`https://<resource>.openai.azure.com/openai/v1/responses`, with the deployment name as the model.
Credential/header injection, including refreshable keyless authentication, belongs to the caller's
transport. Use HTTPS for remote credentials. Credentials must never enter model Params or durable
execution config. Configured headers are not forwarded across an origin-changing redirect.
A supplied HTTP client is copied, not mutated; set its timeout or use a bounded context. The default
client has a two-minute timeout. There are no automatic retries or model/protocol fallbacks.

## Supported content and controls

- Ordered text messages with `input_text` content for system/user roles and `output_text` for
  assistant history. An empty role uses `user`; unsupported roles refuse.
- Function definitions and AUTO/NONE/REQUIRED choices, including a named REQUIRED choice. Schemas
  are passed through within the neutral JSON domain, with `strict:false` so Responses does not
  silently tighten them. Nil schema is omitted; an explicit empty schema remains `{}`.
- Assistant function calls, using the provider's `call_id` as the neutral tool-call ID, not its
  output-item `id`. Arguments must be JSON objects. Integer arguments beyond the exactly representable
  ±2^53 domain refuse rather than silently rounding; neutral structured values use float64.
- Tool-result text from `ToolResult.Content`, joined in order and correlated by result ID. Empty
  text is supported, but empty tool messages and absent result content refuse; they are not replaced
  by receipt metadata, `Output`, `Error` or a URI.
- Opaque completed Responses reasoning items, as described below.
- Usage maps input/output/reasoning tokens directly; reasoning tokens are already included in output
  tokens, not added again. Missing usage produces zero counts, not an estimate.

The only supported `ModelRequest.Params` are:

| Key | Wire value |
|---|---|
| `reasoning_effort` | `reasoning.effort`: `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max` |
| `max_output_tokens` | Positive decimal integer, including reasoning in the provider's output limit |

Omission leaves the provider default. A model may reject an effort value even though it is a valid
protocol value; that error is returned without retry or downgrade. Unknown parameters refuse before
HTTP. Files, images, audio, structured data parts, non-text tool results, built-in/custom tools,
refusals, annotations and other unmapped semantic output refuse rather than being silently omitted.

## Reasoning retention and replay

Each request sends `store:false` and asks for `reasoning.encrypted_content`. There is no
`previous_response_id`, conversation resource, background job or provider-side history dependency.
Completed reasoning-item JSON bytes are stored in `ReasoningPart.Opaque` with provider `openai`,
model ID, item ID and ordinal. The adapter resends the complete opaque item in its original position;
summary text is not a substitute for encrypted state. This format is not interchangeable with Chat
Completions `reasoning_content`, foreign providers, cross-model state or externalized reasoning URIs.
The adapter refuses a present malformed reasoning part or one missing replayable encrypted content.

**The harness owns complete-history retention.** Append the entire returned assistant message to the
next request, not just `Message.Text()` or its function calls, then append the model-facing tool results.
A stateless adapter cannot detect an entirely omitted historical reasoning part. The host journals the
returned message and serves it verbatim during reconstruction; the harness must build the same next
request live, on resume and on replay. `ModelRequest` contains no execution ID, so this adapter does
not invent `ReasoningPart.ValidityScope`; scope management belongs to the harness.

The controller still hashes the original neutral request, not provider JSON. Provider configuration,
headers and constructor defaults are not independently covered by that hash. The journal records
finalized messages and reported usage, not complete provider HTTP payloads. Neither replay nor
recorded-effect recovery invokes the adapter. Recovery of a trailing model intent without a recorded
completion can make its one required live call under the existing protocol rules.

## Streaming is provisional text only

`StreamModel` reports text deltas through the existing text-only callback. It does not expose typed
function-argument, reasoning or indexed-part deltas. The final message retains the ordered text,
reasoning and tool parts. It is decoded from `response.completed`, with stream identities and content
checked against the completed response; usage is taken from that response once.

Nonstreaming responses are limited to 32 MiB. Streaming accepts less than 8 MiB per SSE line and
32 MiB per assembled SSE record; a large one-line completed response can therefore exceed the
streaming limit even when it fits the nonstreaming limit. Oversized responses fail clearly.
Model-call errors retain stage/category and HTTP status, but omit provider messages, unsupported
field/type values, decoder payloads and transport URLs/details: the host may journal the error text.
For non-200 responses, an error envelope of at most 16 KiB may also supply `error.code` (1–64 ASCII
letters/digits/underscores) and `error.param` (1–128 of those characters plus `.`, `[` and `]`). Invalid
fields are omitted independently, never truncated; malformed, oversized or unreadable envelopes retain
only the status. These are provider-controlled identifiers, not arbitrary message content; `param` may
reflect user-chosen key names, so do not use sensitive names in request structure. Cancellation and
deadline errors from transport/success-body processing remain discoverable with `errors.Is`. Constructor
configuration errors and explicit `Endpoint()` inspection still require the caller's usual redaction.

EOF without completion, failed/incomplete responses, provider errors, malformed/conflicting events
and cancellation fail the call. Text already shown to a caller remains provisional on failure; no
partial executable tool call or successful completion is returned. Deltas are not journaled. The same
finalized message and usage are returned by the streaming and nonstreaming paths, and replay emits
no deltas or provider requests.

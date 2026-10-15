# coordinator-workers, pinned to gpt-5.4-mini

A new version of kaimahi's `agentsuite/testdata/coordinator-workers` at
[75f27d6](https://github.com/kaimahi-agents/kaimahi/tree/75f27d67f40b8ce9beb23206285cf04255f28a4/agentsuite/testdata/coordinator-workers)
(vendored unchanged in `../coordinator-workers`). The suite pins the exact model its agents were
evaluated with; a deployment's `AgentSessionsBinding` says where that model is served, and
`emit render` refuses a binding that serves another one. A different model is a new suite version
and a new evaluation, not a render-time override.

The suite keeps its name. AgentSuite manifests have no version field (the spec's suite identity is
the JCS digest of the graph rooted at `agentsuite.json`, and OCI tags are discovery names only), so
this version is its digest, which `emit validate` prints:

| | `agentsuite.json` digest |
|---|---|
| kaimahi 75f27d6 (`../coordinator-workers`) | `sha256:2d05b90f3ddef4b72eee99e6fad9f2c1e7ba9b2e160029b3767c9628bf008e70` |
| this version | `sha256:306685d5375bb021ae1424f610012441b4381d0d49feab3b4c208ac2ced96c60` |

Every change from 75f27d6:

| File | Change |
|---|---|
| `agents/coordinator.json`, `agents/writer.json`, `agents/reviewer.json` | `model.model`: `example-model` -> `gpt-5.4-mini`. `model.protocol` stays `openai-compatible`. |
| `agentsuite.json` | `agents[].digest` of the three agents, recomputed (JCS SHA-256 of each changed manifest): coordinator `sha256:7868b02c…`, writer `sha256:51cfcd1d…`, reviewer `sha256:a45679fb…`. |

The other eleven files are byte-identical to 75f27d6, so the composition, build profile, catalog,
instruction and eval digests are unchanged. The writer's evals are the suite's own, not re-run here:
this PoC has no eval step (see the README one directory up). `SOURCE` lists each file's git blob id
and sha256; `render/render_test.go` checks them and that the only differences are the ones above.

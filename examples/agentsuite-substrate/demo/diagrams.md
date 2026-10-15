## The two diagrams, checked against the code

Substrate paths are at the commit the adapter's go.mod pins (96dffb2, `SUBSTRATE_REF` in
.github/workflows/substrate-e2e.yml); kaimahi's at 9c5c993; the rest in this repository.

Build half (build.mmd, end of chapter 2):

- AgentSuite files -> kaimahi's agentsuite package: `render.Load` calls `agentsuite.ValidateDirectory`
  (examples/agentsuite-substrate/render/render.go:56); `emit validate` calls it directly
  (examples/agentsuite-substrate/cmd/emit/main.go:109).
- -> emit bundle -> harness image: `bundle` loads the composition (cmd/emit/main.go:77) and writes
  agent.json and instructions.md (cmd/emit/main.go:84,87); up.sh stages them as the image's /agent
  layer and takes the pushed image's digest (examples/agentsuite-substrate/up.sh:298,300,302;
  Dockerfile.harness).
- -> emit render, and image digest -> emit render: `renderCmd` loads the composition
  (cmd/emit/main.go:166); `Render` requires a digest-pinned image (render/render.go:280).
- binding-microvm.json -> emit render: `LoadBinding` (render/render.go:179), used by `Render`
  (render/render.go:275).
- emit render -> actortemplate.json and agentsessionsd.args: cmd/emit/main.go:191,201, the flag from
  `Output.DaemonFlag` (render/render.go:351).
- actortemplate.json -> ActorTemplate in substrate, created by hand: driver.sh chapter 3.
- agentsessionsd.args -> agentsessionsd flag, set by hand: up.sh substitutes it into the Deployment
  (up.sh:377).

Run half (run.mmd, start of chapter 4), each also a row of the traced turn:

- agentctl -> agentsessionsd, Sessions.Exec: cmd/agentctl/main.go:265.
- agentsessionsd (its substrate adapter, in process) -> ate-api, CreateActor and ResumeActor:
  integrations/substrate/adapter.go:58,80.
- ate-api -> worker, resume the actor: ate-api calls the node's atelet (AteomHerder),
  substrate cmd/ateapi/internal/controlapi/workflow_resume.go:676, which drives the worker's ateom,
  substrate cmd/atelet/main.go:1557.
- worker -> micro-VM: substrate cmd/ateom-microvm (Kata + Cloud Hypervisor; the install enables the
  class, setup.log).
- agentsessionsd -> atenet-router, harnesswire stream: the actor is named in the ate-target-actor
  header (runtime/substrate/substrate.go:826,853).
- atenet-router -> worker :443: substrate cmd/atenet/internal/router/ingress/ingress.go:140-142
  ("Route ok", targetAddr workerIP:443).
- worker -> harness, into the micro-VM: the harness listens on HARNESS_ADDR :80
  (render/render.go:297, cmd/harness/main.go:44).
- harness -> agentsessionsd, sink.Model on the same stream: harnesswire/harnesswire.go:126.
- agentsessionsd -> gpt-5.4-mini, chat completions: internal/modelconfig/modelconfig.go:46 builds
  the OpenAI-compatible client from -model and -model-base-url.

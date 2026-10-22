## The two diagrams, checked against the code

Substrate paths are at the commit the adapter's go.mod pins (v0.5.0, `SUBSTRATE_REF` in
.github/workflows/substrate-e2e.yml); kaimahi's at 9c5c993; the rest in this repository.

Build half (build.mmd, end of chapter 2):

- AgentSuite files -> kaimahi's agentsuite package: `render.Load` calls `agentsuite.ValidateDirectory`
  (examples/agentsuite-substrate/render/render.go:57); `emit validate` calls it directly
  (examples/agentsuite-substrate/cmd/emit/main.go:109).
- -> emit bundle -> harness image: `bundle` loads the composition (cmd/emit/main.go:77) and writes
  agent.json and instructions.md (cmd/emit/main.go:84,87); up.sh stages them as the image's /agent
  layer and takes the pushed image's digest (examples/agentsuite-substrate/up.sh:325,327,329;
  Dockerfile.harness).
- -> emit render, and image digest -> emit render: `renderCmd` loads the composition
  (cmd/emit/main.go:166); `Render` requires a digest-pinned image (render/render.go:283).
- binding-microvm.json -> emit render: `LoadBinding` (render/render.go:180), used by `Render`
  (render/render.go:278).
- emit render -> actortemplate.json and agentsessionsd.args: cmd/emit/main.go:191,201, the flag from
  `Output.DaemonFlag` (render/render.go:366).
- actortemplate.json -> ActorTemplate in substrate, created by hand: driver.sh chapter 3.
- agentsessionsd.args -> agentsessionsd flag, set by hand: up.sh substitutes it into the Deployment
  (up.sh:402,404). The daemon is the substrate build, integrations/substrate/cmd/agentsessionsd,
  which registers `-substrate-harness` (integrations/substrate/cmd/agentsessionsd/substrate.go:132).

Run half (run.mmd, start of chapter 4), each also a row of the traced turn:

- agentctl -> agentsessionsd, Sessions.Exec: cmd/agentctl/main.go:292.
- agentsessionsd (its substrate adapter, in process) -> ate-api, CreateActor and ResumeActor:
  integrations/substrate/adapter.go:61,83. The substrate build passes its flags to the server's
  `WithHarnessBackends` hook (integrations/substrate/cmd/agentsessionsd/main.go:22), and the server
  calls the hook's `Backends` at startup (internal/agentsessionsd/agentsessionsd.go:273), which dials
  the ate-api and wraps the connection in the adapter
  (integrations/substrate/cmd/agentsessionsd/substrate.go:246,250).
- ate-api -> worker, resume the actor: ate-api calls the node's atelet (AteomHerder),
  substrate cmd/ateapi/internal/controlapi/workflow_resume.go:765, which sends RestoreWorkload to
  the worker pod's ateom (cmd/atelet/main.go:1319, the client from cmd/atelet/main.go:1616).
- agentsessionsd -> atenet-router, harnesswire stream: the actor is named in the ate-target-actor
  header (runtime/substrate/substrate.go:1450,1485).
- atenet-router -> worker :443: substrate cmd/atenet/internal/router/ingress/ingress.go:140-142
  ("Route ok", targetAddr workerIP:443).
- worker -> harness, "starts it": ateom-microvm's RestoreWorkload (substrate
  cmd/ateom-microvm/restore.go:107) relaunches cloud-hypervisor from the snapshot and resumes the
  guest (restore.go:96-97,184); the Kata + Cloud Hypervisor class is the install's, setup.log.
- worker -> harness, "then forwards into it": the worker pod's ateom starts atunnel with the actor's
  HTTP upstream (substrate cmd/ateom-microvm/main.go:226), `http://<actor veth>:80`
  (internal/ateomnet/net.go:35); atunnel listens on :443
  (internal/ateomtunnel/ateomtunnel.go:82,142; the WorkerPool passes `--atunnel-listen-address=:443`,
  cmd/atecontroller/internal/controllers/workerpool_apply.go:117) and reverse-proxies each request to
  the actor's port, 80 unless the X-Ate-Target-Port header names another
  (internal/atunnel/ingress.go:175-188). The harness listens on HARNESS_ADDR :80
  (render/render.go:312, cmd/harness/main.go:44).
- harness -> agentsessionsd, sink.Model on the same stream: harnesswire/harnesswire.go:225,250.
- agentsessionsd -> gpt-5.4-mini, chat completions: internal/modelconfig/modelconfig.go:47 builds
  the OpenAI-compatible client from -model and -model-base-url.

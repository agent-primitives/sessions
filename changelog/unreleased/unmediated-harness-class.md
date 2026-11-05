---
category: Added
---

- Spec §8.8 defines two harness classes. A mediated harness makes every model call through the
  host, as before (`EXE-36`). An unmediated harness, such as an agent CLI that calls its provider
  itself, makes some or all of its model calls through the runtime's egress instead (`EXE-37`); it
  must declare `REQUIRES_MEMORY_SNAPSHOT` (`EXE-38`), holds only a placeholder provider key that
  the egress replaces (`EXE-40`), and, for the calls outside the host, reports output and usage
  with no `MODEL_CALL` (`EXE-39`) and gives up host-bounded spend and model-call audit; it also
  gives up replay of its executions (`EXE-41`). It must not re-run an interrupted execution
  (`EXE-42`), and a turn interrupted by memory loss fails and is re-prompted, in a new session
  once the compute is gone (`EXE-43`). The provider credential invariant in spec §17 and
  [security](../../docs/security.md#provider-credentials-and-harness-classes) now covers both
  classes. No API, wire, storage or code change; the class is not advertised in `Describe`. See
  [unmediated harnesses](../../docs/harness-authoring.md#unmediated-harnesses).

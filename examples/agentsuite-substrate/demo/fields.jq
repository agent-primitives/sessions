# fields.jq: the ActorTemplate fields chapter 2 shows, one line each: the harness image (digest
# pinned; its /agent layer is what emit bundle staged), the container's env, the sandbox and worker
# selector, and the snapshot settings.
"image     \(.containers[0].image | sub(".*/"; "") | .[:44])…  /agent: those files",
"env       \([.containers[0].env[] | "\(.name)=\(.value)"] | join(" "))",
"sandbox   \(.sandboxConfig.configName) \(.sandboxConfig.sandboxClass)  workers \(.workerSelector.matchLabels | tojson)",
"snapshot  \(.snapshotConfig.preferredFidelity) \(.snapshotConfig.storageLocation)"

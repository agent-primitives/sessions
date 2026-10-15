# template.jq: the parts of an ActorTemplate that emit render sets, in a form that compares equal
# between the rendered file and what the cluster returns. The cluster adds a default the render does
# not set (snapshotConfig.goldenSnapshotConfig, mode GOLDEN_SNAPSHOT_MODE_ENABLED), so it is dropped.
{
  metadata: {atespace: .metadata.atespace, name: .metadata.name},
  containers,
  snapshotConfig: (.snapshotConfig | del(.goldenSnapshotConfig)),
  sandboxConfig,
  workerSelector
}

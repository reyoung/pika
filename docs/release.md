# Install, backup, and upgrade

## Install a release archive

Choose the archive matching the host OS and architecture, then verify it before extraction:

```bash
shasum -a 256 --check SHA256SUMS
tar -xzf pika-go_<os>_<arch>.tar.gz
cd pika-go_<os>_<arch>
herdr plugin link . --enabled
herdr plugin pane open --plugin pika-go --entrypoint symphony --focus
```

The linked directory must remain in place. `herdr-plugin.toml`, `pika-go`, and the packaged defaults belong together. The runtime binary contains immutable Role System Prompts; the packaged instruction directory contains no mutable prompt content.

Inside the opened Symphony workspace, initialize one Optimization from another pane:

```bash
pika-go init --repository /absolute/path/to/repository
```

The daemon reports the instance through Herdr workspace metadata. Commands in the same Herdr workspace discover the correct Unix socket automatically.

## Create an online backup

Back up a running instance through the daemon rather than copying `pika.db` while WAL mode is active:

```bash
pika-go backup --output /absolute/path/to/backups/pika-YYYYMMDD.db
```

The destination must not already exist. The daemon checkpoints pending WAL frames, creates a self-contained SQLite snapshot, applies user-only permissions, and validates both `quick_check` and the schema version before reporting success. `pika-go status --json` exposes database, WAL, provider-event, and tool-payload byte counts for capacity monitoring.

## Upgrade

1. Create and retain an online backup.
2. Request graceful shutdown with `pika-go shutdown`. The command acknowledges entry into `draining`; wait for the Unix socket to disappear after pending Work, runtime effects, and child Agents finish normally. Pika does not kill them.
3. Unlink the old plugin directory with `herdr plugin unlink pika-go`.
4. Verify and extract the new release archive into a new directory.
5. Link the new directory with `herdr plugin link <new-directory> --enabled` and open its Symphony pane.
6. Confirm `pika-go status --json` reports the expected instance and protocol version before starting more Work.

Database migrations run during daemon startup. A newer unsupported schema, checksum mismatch, failed migration, failed SQLite integrity check, or invalid online domain state prevents scheduling. Keep the backup and the old release until the upgraded daemon has completed a real workflow transition.

CLI and daemon control requests carry an explicit protocol version. A mismatched client fails instead of attempting a mutation against an incompatible daemon.

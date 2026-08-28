# Install, backup, and upgrade

## Install from source

`herdr plugin install` clones the repository and runs `make install`, which builds the host `pika-go` binary next to `herdr-plugin.toml`. The same target works in a local checkout:

```bash
make install
./pika-go install
```

By default, `make install` also copies the CLI into `/usr/local/bin`. Override `PREFIX` to choose another installation prefix, or set `PREFIX=` to only build the plugin-root binary.

For development against the current checkout instead of a stable installation, use `herdr plugin link .`; rebuilding then updates the linked executable in place.

## Install a release archive

Choose the archive matching the host OS and architecture, then verify it before extraction:

```bash
shasum -a 256 --check SHA256SUMS
tar -xzf pika-go_<os>_<arch>.tar.gz
cd pika-go_<os>_<arch>
./pika-go install
./pika-go kick-off --repository /absolute/path/to/repository
```

The extracted release directory may be removed after installation. `pika-go install` copies the binary and its embedded manifest into `$XDG_DATA_HOME/pika-go/plugin`, or `~/.local/share/pika-go/plugin` when `XDG_DATA_HOME` is unset. That installed directory must remain in place because Herdr reloads its manifest by path. The binary contains the immutable Role System Prompts and empty instruction templates.

`kick-off` must run from a shell pane inside Herdr. Before creating layout it verifies `session.resume_agents_on_restore = false`; when necessary it asks permission to update the active Herdr configuration and reload the server. It then creates a dedicated workspace, opens the installed Symphony daemon pane, waits for health, starts `pika-go init` in the new workspace's root pane, and then focuses that workspace, tab, and pane. Interactive backend, model, reasoning-effort, and Cursor launch-permission inputs are numbered choices rather than free-form IDs or raw argv. The root pane remains the preferred first Baseline Agent pane. For non-interactive startup, preconfigure Herdr and choose one of:

```bash
pika-go kick-off --repository /absolute/path/to/repository --defaults
pika-go kick-off --repository /absolute/path/to/repository --config /absolute/path/to/config.toml
```

For manual recovery or debugging, open the `symphony` plugin pane and run `pika-go init --repository /absolute/path/to/repository` from another pane in the same workspace. The daemon reports the instance through Herdr workspace metadata, so later commands in that workspace discover the correct Unix socket automatically.

## Create an online backup

Back up a running instance through the daemon rather than copying `pika.db` while WAL mode is active:

```bash
pika-go backup --output /absolute/path/to/backups/pika-YYYYMMDD.db
```

The destination must not already exist. The daemon checkpoints pending WAL frames, creates a self-contained SQLite snapshot, applies user-only permissions, and validates both `quick_check` and the schema version before reporting success. `pika-go status --json` exposes database, WAL, provider-event, and tool-payload byte counts for capacity monitoring.

## Upgrade

1. Create and retain an online backup.
2. Request graceful shutdown with `pika-go shutdown`. The command acknowledges entry into `draining`; wait for the Unix socket to disappear after pending Work, runtime effects, and child Agents finish normally. Pika does not kill them.
3. Verify and extract the new release archive.
4. Run the new `./pika-go install`; it replaces the installed binary and manifest using atomic file updates, then updates the Herdr registration. Open its Symphony pane.
5. Confirm `pika-go status --json` reports the expected instance and protocol version before starting more Work.

Database migrations run during daemon startup. A newer unsupported schema, checksum mismatch, failed migration, failed SQLite integrity check, or invalid online domain state prevents scheduling. Keep the backup and the old release until the upgraded daemon has completed a real workflow transition.

CLI and daemon control requests carry an explicit protocol version. A mismatched client fails instead of attempting a mutation against an incompatible daemon.

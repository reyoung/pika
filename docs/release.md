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

`kick-off` must run from a shell pane inside Herdr. Before creating layout it verifies `session.resume_agents_on_restore = false`; when necessary it asks permission to update the active Herdr configuration and reload the server. It then creates `<repository>-pika-workspace` beside the source repository, creates its Pika-owned base linked worktree, and opens Herdr with the durable Workspace as cwd. The daemon receives `PIKA_GO_WORKSPACE`, owns `pika.toml`, `pika.db`, `.pika.lock`, artifacts, and all Pika worktrees there, and writes the replaceable Herdr IDs to `herdr/binding.json`. Interactive backend, model, reasoning-effort, and Cursor launch-permission inputs are numbered choices rather than free-form IDs or raw argv. For non-interactive startup, preconfigure Herdr and choose one of:

The first start with an update-capable build also copies that executable to `runtime/daemon/generations/<sha256>/pika-go` and records it in `runtime/daemon/current.json`. Herdr starts the managed generation from then on, so replacing a developer checkout binary cannot invalidate a running pane.

```bash
pika-go kick-off --repository /absolute/path/to/repository --defaults
pika-go kick-off --repository /absolute/path/to/repository --config /absolute/path/to/config.toml
```

Resume existing durable state with:

```bash
pika-go open /absolute/path/to/repository-pika-workspace
```

Running `pika-go kick-off` inside that directory has the same auto-resume behavior. Existing `pika.toml` is reused; init is not repeated. Do not move the Workspace or its source Git common directory. Their absolute and filesystem identities are validated at daemon startup.

Pre-Workspace releases stored instances below Herdr's plugin config/state directories. Migration is explicit and requires a stopped legacy daemon:

```bash
pika-go workspace legacy-list
pika-go workspace import --instance INSTANCE_ID --workspace /absolute/path/to/new-workspace
```

Import uses a consistent SQLite copy and preserves configuration, instructions, evidence/log/context/runtime artifacts, base/Best/Attempt linked worktrees, and dirty Git index/working files. It never automatically imports or deletes the legacy instance.

## Create an online backup

Back up a running instance through the daemon rather than copying `pika.db` while WAL mode is active:

```bash
pika-go backup --output /absolute/path/to/backups/pika-YYYYMMDD.db
```

The destination must not already exist. The daemon checkpoints pending WAL frames, creates a self-contained SQLite snapshot, applies user-only permissions, and validates both `quick_check` and the schema version before reporting success. `pika-go status --json` exposes database, WAL, provider-event, and tool-payload byte counts for capacity monitoring.

## Hot-update a running Workspace daemon

Build or obtain a local candidate, then run:

```sh
pika-go update --binary ./pika-go
pika-go update status
```

The command copies the candidate into the current Optimization Workspace and rejects a different control protocol, handoff protocol, Workspace format, SQLite schema, operating system, or architecture. Those changes require the cold backup/shutdown/resume procedure below.

For a compatible candidate, the old daemon starts it in a quiescent state with inherited listener and lock descriptors. Active Herdr panes and Codex/Cursor Agent Sessions are not retired or relaunched. After the candidate reports ready, the old daemon commits `current.json`; the candidate then resumes reconciliation and scheduling. The public Unix socket path and inode stay unchanged. If readiness or stabilization fails within 15 seconds, the old generation is started from the Workspace store and `update.json` ends in `rolled_back`. Run the real process acceptance matrix with `make hot-update-integration`.

The two real-provider hot-reload targets use a 20-minute process watchdog, but they do not treat elapsed wall time as provider progress. They fail immediately when a Pika-owned active Agent is `blocked`, and fail after two continuous minutes without any domain event, Session transition, provider/tool journal growth, or Herdr pane revision. The clock resets only on such observable progress. Their Follow-up check uses a real provider Stop hook from a settled-idle first turn; tests must not inject a synthetic Stop while Herdr still reports the target as `working`.

## Cold upgrade with maintenance hold

Do not use `pika-go shutdown` for a recoverable cold upgrade. Shutdown permanently changes the Optimization to `draining`; maintenance changes only daemon/runtime process state and leaves the Optimization, Work lineage, revisions, receipts, and flow version untouched.

The supported sequence is schema-compatible hot handoff to a temporary maintenance bridge, maintenance prepare, cold open in holding, validation, and explicit resume. Protocol versions are not increased for this operation: the bridge and target must report handoff protocol 1, control protocol 2, and Workspace format 1.

The commands below are the production runbook. They are intentionally split at stop points. Do not continue past a failed assertion, and do not perform these steps until the bridge and target commits, binaries, digests, and test evidence have been reviewed.

```sh
export WORKSPACE=/absolute/path/to/optimization-workspace
export SOURCE=/absolute/path/to/current-schema20/pika-go
export BRIDGE=/absolute/path/to/schema20/pika-go-maintenance-bridge
export TARGET=/absolute/path/to/schema23/pika-go
export RUN=/absolute/path/to/retained/migration-records
mkdir -p "$RUN"
export HERDR_SOCKET_PATH="$(jq -r .socket_path "$WORKSPACE/herdr/binding.json")"
export SOCKET="$(jq -r .daemon_socket "$WORKSPACE/herdr/binding.json")"
cp "$WORKSPACE/herdr/binding.json" "$RUN/herdr-binding-before.json"
test -S "$HERDR_SOCKET_PATH"

"$SOURCE" update-probe >"$RUN/source-probe.json"
"$BRIDGE" update-probe >"$RUN/bridge-probe.json"
"$TARGET" update-probe >"$RUN/target-probe.json"
sha256sum "$SOURCE" "$BRIDGE" "$TARGET" | tee "$RUN/binary-sha256.txt"
export SOURCE_DIGEST="$(sha256sum "$SOURCE" | awk '{print $1}')"
export BRIDGE_DIGEST="$(sha256sum "$BRIDGE" | awk '{print $1}')"
export TARGET_DIGEST="$(sha256sum "$TARGET" | awk '{print $1}')"
```

Stop unless the reviewed bridge probe is exactly schema 20 / handoff 1 / control 2 / Workspace 1 and the target probe is schema 23 / handoff 1 / control 2 / Workspace 1.

### 1. Capture the live anchor and online backup

Refresh every observed value; do not replace it with an older value. For the `welm_v45_80a3_attention` migration, the last planning anchors were Optimization `d3cb218ff03d705c3349003e0acdeabb`, revision 59, Best sequence 3, Best SHA `c86eb02733ee2a21440229137761a2300adf04b8`, and live generation digest `181dbd29062ae5707a8e82419a35b59a454ba0e6e7affa9a7722087a7f7f8104`. They are lower bounds/checkpoints, not values to write.

```sh
"$SOURCE" status --socket "$SOCKET" --json >"$RUN/status-before.json"
"$SOURCE" backup --socket "$SOCKET" --output "$RUN/online-schema20.db"
sqlite3 "$RUN/online-schema20.db" \
  "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name;" |
while IFS= read -r table; do
  printf '%s\t' "$table"
  sqlite3 "$RUN/online-schema20.db" "SELECT COUNT(*) FROM \"$table\";"
done >"$RUN/row-counts-before.tsv"
sqlite3 "$RUN/online-schema20.db" "PRAGMA integrity_check;" | tee "$RUN/online-integrity.txt"

cp "$WORKSPACE/herdr/binding.json" "$RUN/binding-before.json"
stat -Lc '%d:%i %a %U:%G %n' "$WORKSPACE"/runtime/*.sock >"$RUN/socket-before.txt"
ps -eo pid,ppid,lstart,args >"$RUN/processes-before.txt"
export SOURCE_DAEMON_PID=<pid-from-processes-before>
readlink -f "/proc/$SOURCE_DAEMON_PID/exe" | tee "$RUN/source-daemon-exe.txt"
sha256sum "/proc/$SOURCE_DAEMON_PID/exe" | tee "$RUN/source-daemon-sha256.txt"
test "$(sha256sum "/proc/$SOURCE_DAEMON_PID/exe" | awk '{print $1}')" = "$SOURCE_DIGEST"
```

Record the active Work, Agent Session, Agent name, pane/terminal identity, provider/tool byte counts, Optimization revision, Best sequence/SHA, binding, daemon PID/digest, and socket inode. Stop if the online backup does not pass `integrity_check`, the Optimization is not `optimizing`, flow is not 1, or any anchor regresses.

### 2. Hot handoff to the schema-20 bridge

```sh
"$SOURCE" update --workspace "$WORKSPACE" --binary "$BRIDGE"
"$SOURCE" update status --workspace "$WORKSPACE" --json | tee "$RUN/bridge-update.json"
"$BRIDGE" status --socket "$SOCKET" --json >"$RUN/status-on-bridge.json"
stat -Lc '%d:%i %a %U:%G %n' "$WORKSPACE"/runtime/*.sock >"$RUN/socket-on-bridge.txt"
ps -eo pid,ppid,lstart,args >"$RUN/processes-on-bridge.txt"
export BRIDGE_DAEMON_PID=<pid-from-processes-on-bridge>
readlink -f "/proc/$BRIDGE_DAEMON_PID/exe" | tee "$RUN/bridge-daemon-exe.txt"
sha256sum "/proc/$BRIDGE_DAEMON_PID/exe" | tee "$RUN/bridge-daemon-sha256.txt"
test "$(sha256sum "/proc/$BRIDGE_DAEMON_PID/exe" | awk '{print $1}')" = "$BRIDGE_DIGEST"
```

Stop and rely on normal hot-update rollback unless update state is `committed`, the daemon digest equals `BRIDGE_DIGEST`, and socket inode plus every active Session/name/pane/terminal identity is unchanged. Do not continue if a second Agent Session appeared.

### 3. Prepare maintenance and wait for ready

```sh
"$BRIDGE" maintenance prepare --workspace "$WORKSPACE" \
  --request-id "cold-schema23-$(date -u +%Y%m%dT%H%M%SZ)" \
  --to-digest "$TARGET_DIGEST" \
  --to-version schema23 \
  --json | tee "$RUN/maintenance-prepare.json"

while :; do
  "$BRIDGE" maintenance status --workspace "$WORKSPACE" --json |
    tee "$RUN/maintenance-latest.json"
  state="$(jq -r .state "$RUN/maintenance-latest.json")"
  test "$state" = ready && break
  test "$state" = failed && exit 1
  sleep 2
done
```

Prepare atomically closes reconciliation, outbox-dispatch, and Follow-Up-promotion entry points, but keeps the same Unix socket, MCP endpoint, and provider-event hooks available. It freezes the active Work/Session identities at acceptance. Those Work items must reach their own terminal domain operations through the existing Agents; pending successor Work and runtime outbox entries are allowed to remain. Do not pause, Ctrl-C, close, kill, answer for, or replace a blocked Agent. If it cannot reach terminal, leave the bridge and socket online and stop.

After `ready`, the bridge gracefully finishes the terminal MCP response, exits, and removes the socket:

```sh
test ! -S "$(jq -r .daemon_socket "$WORKSPACE/herdr/binding.json")"
sqlite3 "$WORKSPACE/pika.db" ".backup '$RUN/drained-schema20.db'"
sqlite3 "$RUN/drained-schema20.db" "PRAGMA integrity_check;" |
  tee "$RUN/drained-integrity.txt"
```

Stop unless the offline backup is schema 20 and valid, the Optimization remains `optimizing` with flow version 1, the frozen Work is terminal, and pending successor Work/outbox rows remain represented.

### 4. Cold-open schema 23 in holding and validate

```sh
"$TARGET" install
"$TARGET" open "$WORKSPACE" --no-focus
cmp "$RUN/herdr-binding-before.json" "$WORKSPACE/herdr/binding.json"
"$TARGET" maintenance status --workspace "$WORKSPACE" --json |
  tee "$RUN/maintenance-holding.json"
"$TARGET" status --socket "$SOCKET" --json >"$RUN/status-holding.json"
export TARGET_DAEMON_PID=<pid-from-reviewed-process-inventory>
readlink -f "/proc/$TARGET_DAEMON_PID/exe" | tee "$RUN/target-daemon-exe.txt"
sha256sum "/proc/$TARGET_DAEMON_PID/exe" | tee "$RUN/target-daemon-sha256.txt"
test "$(sha256sum "/proc/$TARGET_DAEMON_PID/exe" | awk '{print $1}')" = "$TARGET_DIGEST"
sqlite3 "$WORKSPACE/pika.db" "SELECT MAX(version) FROM migrations;" |
  tee "$RUN/schema-holding.txt"
sqlite3 "$WORKSPACE/pika.db" "PRAGMA integrity_check;" |
  tee "$RUN/holding-integrity.txt"
```

The cold daemon may migrate/open SQLite and serve health, maintenance status, ordinary status, MCP, provider events, and read-only Workbench data, but it must not retire old Sessions or start reconciliation/dispatch while holding. Stop unless:

- maintenance is `holding`, its holding generation digest equals `TARGET_DIGEST`, and `/proc/<daemon-pid>/exe` hashes to the reviewed target;
- schema is 23 and `integrity_check` is `ok`;
- Optimization ID is unchanged, status is still `optimizing`, and `flow_version` is 1;
- revision and Best sequence/SHA did not regress;
- every pre-existing table's row count is at least its captured count, and schema-21–23 KDA tables are structurally valid without enabling flow-v2 for this Optimization;
- provider/tool counts did not regress and no runtime dispatch, Session retirement, or new Agent occurred.

### 5. Rebind the read-only WebUI, then resume

The maintenance-aware `open` command uses `HERDR_SOCKET_PATH` to re-enter the frozen Herdr workspace and daemon pane. It must preserve the binding byte-for-byte and reuse the exact daemon socket; it must not create or rename a workspace, tab, or pane. After Holding is validated, gracefully stop the old WebUI and restart it from the target binary on the same address without token rotation:

```sh
kill -TERM "$(cat "$WORKSPACE/runtime/webui/pid")"
while kill -0 "$(cat "$WORKSPACE/runtime/webui/pid" 2>/dev/null)" 2>/dev/null; do sleep 1; done
sha256sum "$WORKSPACE/runtime/webui/token" | tee "$RUN/webui-token-before-restart.sha256"
nohup "$TARGET" webui --workspace "$WORKSPACE" --listen 0.0.0.0:8080 \
  >"$RUN/webui-target.log" 2>&1 &
while test ! -s "$WORKSPACE/runtime/webui/pid"; do sleep 1; done
export WEBUI_PID="$(cat "$WORKSPACE/runtime/webui/pid")"
test "$(sha256sum "/proc/$WEBUI_PID/exe" | awk '{print $1}')" = "$TARGET_DIGEST"
sha256sum "$WORKSPACE/runtime/webui/token" | tee "$RUN/webui-token-after-restart.sha256"
cmp "$RUN/webui-token-before-restart.sha256" "$RUN/webui-token-after-restart.sha256"
```

The background process is the documented WebUI sidecar: it writes its own PID file and removes it on graceful exit. Verify its authenticated read-only Workbench endpoint is ready before resume; stop if it exited, its executable digest differs, its listen address changed, or the persistent token changed.

```sh
"$TARGET" maintenance resume --workspace "$WORKSPACE" \
  --request-id "resume-schema23-$(date -u +%Y%m%dT%H%M%SZ)" \
  --json | tee "$RUN/maintenance-resumed.json"
```

Resume first retires terminal old-generation Sessions, recovers uncertain effects, starts runtime, performs one initial dispatch, and only then records `resumed`. Confirm exactly one successor Agent/Session, continuous Attempt/Work lineage, at least one new provider/tool/domain event, and the first post-migration Work terminal operation. Retain both backups and the bridge binary/commit until this terminal gate passes.

### Rollback before resume

- Bridge hot-update failure: use its automatic rollback and stop.
- Agent blocked before ready: leave bridge/socket online; do not pause, interrupt, close, or replace it.
- Target holding validation failure: send SIGTERM only to the reviewed holding target daemon, wait for it to stop, atomically restore `drained-schema20.db`, install/open the retained bridge, and inspect holding before resume:

```sh
kill -TERM "$HOLDING_DAEMON_PID"
while kill -0 "$HOLDING_DAEMON_PID" 2>/dev/null; do sleep 1; done
test ! -S "$(jq -r .daemon_socket "$WORKSPACE/herdr/binding.json")"
ROLLBACK_ASIDE="$WORKSPACE/runtime/schema23-holding-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$ROLLBACK_ASIDE"
for file in pika.db pika.db-wal pika.db-shm; do
  test ! -e "$WORKSPACE/$file" || mv "$WORKSPACE/$file" "$ROLLBACK_ASIDE/$file"
done
sync "$ROLLBACK_ASIDE" "$WORKSPACE"
install -m 600 "$RUN/drained-schema20.db" "$WORKSPACE/pika.db.restore"
mv "$WORKSPACE/pika.db.restore" "$WORKSPACE/pika.db"
python3 - "$WORKSPACE/pika.db" "$WORKSPACE" <<'PY'
import os, sys
fd = os.open(sys.argv[1], os.O_RDONLY)
os.fsync(fd)
os.close(fd)
fd = os.open(sys.argv[2], os.O_RDONLY | os.O_DIRECTORY)
os.fsync(fd)
os.close(fd)
PY
test ! -e "$WORKSPACE/pika.db-wal"
test ! -e "$WORKSPACE/pika.db-shm"
test "$(sqlite3 "$WORKSPACE/pika.db" 'PRAGMA integrity_check;')" = ok
test "$(sqlite3 "$WORKSPACE/pika.db" 'SELECT MAX(version) FROM migrations;')" = 20
"$BRIDGE" install
"$BRIDGE" open "$WORKSPACE" --no-focus
export ROLLBACK_DAEMON_PID=<pid-from-reviewed-process-inventory>
test "$(sha256sum "/proc/$ROLLBACK_DAEMON_PID/exe" | awk '{print $1}')" = "$BRIDGE_DIGEST"
"$BRIDGE" maintenance status --workspace "$WORKSPACE" --json |
  tee "$RUN/rollback-holding.json"
"$BRIDGE" maintenance resume --workspace "$WORKSPACE" \
  --request-id "rollback-resume-$(date -u +%Y%m%dT%H%M%SZ)" --json |
  tee "$RUN/rollback-resumed.json"
```

A holding Workspace may be reclaimed only by the original `from_generation` digest (the schema-20 bridge) or reopened by the existing `holding_generation` / `to_generation` digest. Arbitrary third digests and truncated SHA-256 values are rejected. Reclaiming never starts runtime by itself. Stop if it does not remain holding or if restored data does not match the explicitly recorded drained-backup time point.

Generation identities in `runtime/daemon/maintenance.json` are 64-character lowercase SHA-256 digests. `failed` is persisted only when frozen Work identities cannot be observed; a new `maintenance prepare` may start after `failed`. Capture/persist errors during prepare do not write `failed` and do not freeze the live generation.

After maintenance has resumed, never overwrite new progress with the old backup. Repair forward on the new daemon. Any exceptional restore requires an explicit data-loss time point and supervisor approval. Never update Optimization status directly with SQL, and never substitute domain shutdown for maintenance.

CLI and daemon control requests continue to carry protocol version 2. A mismatched client fails instead of attempting a mutation against an incompatible daemon.

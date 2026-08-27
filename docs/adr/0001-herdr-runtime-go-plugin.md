# Use Herdr as the terminal runtime behind a Go plugin

Pika will ship as a Go single binary and run its Symphony daemon in a visible Herdr pane. Herdr owns workspaces, panes, recognized Agent processes, and direct human interaction; Pika owns optimization workflow, persistence, prompts, and MCP contracts. This replaces the old Web application runtime without binding Pika to Herdr's Rust internals or falling back to unstructured tmux control.

## Consequences

- The plugin uses Herdr's local socket API and plugin-provided config/state directories.
- The daemon is a pane process, not a Herdr startup hook; startup hooks are one-shot and do not supervise daemons.
- macOS and Linux are the initial distribution targets.

# Separate domain authority from runtime observation

SQLite transactions and Role-scoped terminal MCP operations are authoritative for Optimization state; Herdr is authoritative only for pane layout, process presence, interaction state, and session references. An Agent becoming `idle` or `done`, terminal text, or a natural-language final answer never completes Work. This prevents screen detection and provider-specific lifecycle gaps from corrupting the optimization state machine.

## Consequences

- Every mutating MCP operation is authorized by Role, Work, and Agent Session and is idempotent.
- Runtime reconciliation may create a fresh Agent Session, but it cannot invent a domain result.
- Herdr and Pika state are reconciled after restart instead of copied into one another.

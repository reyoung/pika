---
status: accepted
---

# Use fresh sessions and a per-Work ledger for Backend fallback

Pika models fallback as an ordered Backend Fallback Chain owned by one Agent configuration. Capacity exhaustion and authentication failure atomically exclude the selected Backend Endpoint in a persistent per-Work, per-chain ledger; recovery creates a fresh Pika and provider Session on the next Endpoint, using the existing Conversation Journal, Recovery Context, and Git workspace instead of hot-switching or resuming provider state.

This deliberately keeps failure classification conservative: network, context-window, session-budget, fatal, and unknown failures do not advance the chain. Codex is classified only from authoritative failure outcomes, Cursor Headless only exposes authentication failure from its login preflight, and Cursor ACP remains a legacy runtime adapter that cannot be added by init or reconfiguration.

The consequences are that fallback never increases Agent concurrency, all-excluded Work can remain durably runnable-but-blocked, and unknown provider errors may require retrying the current Endpoint rather than opportunistically moving forward. A chain configuration change produces a new SHA-256 identity and retries from the primary Endpoint while retaining old ledger rows for audit.

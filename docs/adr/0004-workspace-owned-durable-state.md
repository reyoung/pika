# Optimization Workspace owns durable state

Pika stores configuration, SQLite history, evidence, runtime records, and every Pika-owned linked Git worktree under one stable Optimization Workspace; Herdr Workspace IDs and plugin storage remain replaceable runtime concerns. The Source Repository keeps the shared Git common directory, while a stable Workspace ID namespaces Pika branches, because this preserves the user's checkout and permits multiple independent Optimizations without pretending the Workspace is portable when that external Git identity is absent.

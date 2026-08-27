# User instruction overlays

Pika-Go creates one empty, user-editable Markdown overlay for each Role. The files are created in the Herdr plugin configuration directory by `pika-go init`; this release directory contains only this explanation.

The immutable Baseline, Baseline Verification, Iteration, Integration, and Follow-up System Prompts are embedded in the `pika-go` binary. They are not templates for these files and cannot be edited with `pika-go edit-instruction`. The Role catalog and filenames are defined in `docs/design/role-contracts.md`.

---
name: recallstack-memory
description: Use Recallstack only for prior decisions, non-local history, recurring issue context, and other context that is not cheaper to verify directly in the current workspace.
---

# Recallstack Memory (Google Antigravity Local)

Required setup:

1. `recallstack login <CODE>`
2. `recallstack workspace use`
3. `recallstack project use`
4. `recallstack project` to verify the target

Retrieval protocol:

1. Use the current thread and local files/logs/tests for facts that can be verified directly.
2. Query Recallstack for prior decisions, rationale, related work, recurring issue history, user preferences, or other non-local context.
3. Use `recallstack memory query --query "<task>" --mode standard` by default.
4. Use `mode=quick` for fast checks, `deep` for fuller context, and `forensic` for investigative retrieval.
5. Raw writes to `global` are forbidden. Ensure the configured target is non-global.

Antigravity hook behavior:

1. `recallstack agent install antigravity` installs this skill and a JSON Stop hook.
2. The Stop hook reads Antigravity's transcript path, reconstructs completed turns, and queues memory ingestion in the background.
3. The background worker runs an isolated JSON-schema extraction pass before ingesting so memory quality matches the Codex integration.
4. Installing Antigravity CLI is recommended for native Antigravity synthesis when Recallstack supports its non-interactive extraction command. Until then the hook uses the configured fallback worker and then raw transcript extraction if no worker is available.
5. The hook returns immediately so the Antigravity loop is not delayed.
6. Do not spend thread space describing memory housekeeping unless the user asked, the hook failed, or the result materially changes the answer.

Durable source guidance:

1. Use `recallstack memory source ingest` for plans, handovers, specs, checklists, research notes, copied docs, meeting notes, and verified issue-remedy runbooks.
2. Durable ingest is the default when the user says `remember this`, `store this`, `save this`, or asks for a long-lived reference note.
3. Check durable memory before drafting a new durable artifact if prior documentation may already exist.
4. After a remedy is verified and likely reusable, ingest it durably as a concise issue-remedy note with searchable metadata.

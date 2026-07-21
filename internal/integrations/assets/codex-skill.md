---
name: recallstack-memory
description: "MANDATORY: Always validate the Recallstack target. Query memory only when history, rationale, prior work, or non-local context makes Recallstack the right source. Codex hooks ingest turns asynchronously."
---

# Recallstack Memory (Codex Local)

Mandatory rule:

- Always validate the Recallstack target before work.
- Do not query memory by reflex just because a new turn started.
- Use memory only when it is the right source of truth for the user's intent.
- Codex hooks handle async end-of-turn ingest automatically. Do not narrate or manually run turn ingest unless the user explicitly asks for a manual Recallstack action.
- Keep Recallstack operations in the background. Do not narrate turn ids, query wording, ingest steps, or other memory housekeeping unless the user asked, the operation failed, or the memory result materially changes the answer.

Required setup (once per repository):

1. Authenticate globally:
   `recallstack login <CODE>`
2. Set workspace context for this repo:
   `recallstack workspace use`
3. Set non-global default project for this repo:
   `recallstack project use`
4. Verify current target:
   `recallstack project`
5. Optional inline override per command:
   add `--project <projectSlug|workspaceSlug/projectSlug>`

Setup validation gate (run before any task work):

1. Confirm this repository target is configured:
   `recallstack project`
   Expected shape (values vary):
   `{"workspace_config_path":"<repo>/.recallstack/workspace.json","project":"<workspaceSlug>/<projectSlug>","target":"<workspaceSlug>/<projectSlug>"}`
2. If `recallstack project` fails or required fields are null, diagnose runtime config:
   `recallstack config`
   Expected shape (values vary):
   `{"effective_base_url":"...","active_workspace":{"id":"...","slug":"..."},"configured_workspace":{"id":"...","slug":"..."}}`
3. If `recallstack project` fails or required fields are null, confirm signed-in identity:
   `recallstack whoami`
   Expected shape (values vary):
   `{"id":"...","email":"...","auth_type":"jwt","workspace":{"id":"...","kind":"PERSONAL|TEAM","role":"ADMIN|MEMBER"}}`
4. If the target is still invalid:
   - run `recallstack login <CODE>`
   - run `recallstack workspace use`
   - run `recallstack project use`
   - rerun `recallstack project`
   - only rerun `recallstack config` and `recallstack whoami` if `project` still fails
5. Only continue to task execution after `recallstack project` passes with a non-global target.

Retrieval protocol:

1. Decide the source of truth before querying memory:
   - Use the current thread first when the answer is already established in the conversation.
   - Use code, logs, config, tests, and local files for directly verifiable current-state facts.
   - Use Recallstack memory for rationale, prior decisions, historical context, related prior work, recurring issue history, user preferences, or context that is not cheap to verify locally.
2. If memory is the right source, query it directly:
   `recallstack memory query --query "<state/proof-oriented task>" --mode standard`
   Add `--synthesize` to draft a local answer from the retrieved evidence, or pass `--worker-model <model>` to both enable synthesis and choose the local worker model.
3. Use `mode=quick` for fast checks, `standard` for default recall, `deep` for fuller context, and `forensic` for investigative retrieval.
4. Do not use `--turn-id` for Codex retrieval. Codex hooks own turn ingestion and there is no manual query/ingest turn pairing to preserve.
5. Raw writes to `global` are forbidden. Ensure the configured target is non-global.

Codex hook behavior (installed by `recallstack agent install codex`):

1. `SessionStart` records session metadata for the current repo.
2. `Stop` immediately queues background processing and returns without delaying the thread.
3. The background worker reads the latest completed turn from the Codex transcript.
4. The worker runs an ephemeral `codex exec --ephemeral --disable codex_hooks` extraction pass to turn the transcript slice into a Recallstack-ready summary.
5. Set the background extraction model by reinstalling with `recallstack agent install codex --worker-model <model>`.
6. Retryable ingest failures such as rate limits or unavailable backend responses are cached locally and retried in the background for up to 24 hours. Auth-required ingest failures are also cached locally and replayed after the next successful login.
7. The worker ingests that processed summary into the selected non-global project.
8. Because ingest is hook-driven, do not spend thread space describing memory housekeeping.

Routing guidance (mandatory):

1. Codex hooks already ingest every turn, including user questions, research discussions, tradeoffs, interim reasoning, decisions, outcomes, and verified findings.
2. Non-evidence work still matters. If the turn is exploratory, comparative, or research-driven, let the hook capture the options considered, tradeoffs, and current leaning rather than trying to restate them manually in the thread.
3. Avoid raw terminal transcripts and noisy operational narration by default. The hook’s processing pass is responsible for condensing actions and consequences into retrievable memory.
4. Use `memory source ingest` (MCP: `memory_ingest_source`) for durable artifacts the user is likely to want again: plans, handovers, specs, checklists, research notes, copied docs, meeting notes, or verified issue-remedy runbooks.
5. Durable ingest is the default when the user says `remember this`, `store this`, `save this`, or asks you to write/update a plan, handover, checklist, spec, or long-lived project note.
6. Use `memory_query` only when the question is about history, rationale, prior related work, recurring issue history, user preference history, or context that is not cheaply verifiable in the repo. Keep `memory_query_direct` for exact durable passages.
7. Do not query memory for facts that are cheaper and more reliable to verify directly in code, config, logs, or tests. Example: `What auth options have we today?` should usually be answered from the repo, not memory.
8. For new work, frame memory queries around current state or prior related work, not the desired implementation outcome or an open-ended recommendation. Prefer queries like `what already exists for X`, `find prior related work on X`, `why did we choose X`, or `is X already implemented`. Avoid queries like `how should we improve X`, `add X`, `implement Y`, or `fix Z`.
9. Use `memory_query_direct` when exact durable-source passages matter, when the user asks for an existing plan/handover/spec/checklist, or before drafting a new durable artifact if prior documentation may already exist.
10. When an issue recurs, debugging starts looping, environment drift is suspected, or a familiar runtime error appears, query Recallstack memory before widening the search. Use `memory_query` first, then `memory_query_direct` if you need exact durable passages.
11. Treat optional client-side synthesis as a hint, not proof. Cross-check strong claims against the evidence list and local code before relying on them.
12. Use `memory_get_source` when excerpt-level results are insufficient, or when the user asks for the full document/body.
13. Example split:
   - `Why did we move away from OOP to functional programming in TypeScript?` -> query memory.
   - `What auth options have we today?` -> inspect code first.
14. Do not pollute the thread with memory activity. Recallstack use should usually be silent. Only mention it when the user explicitly asks, the operation fails, or the retrieved memory materially affects the answer. If you must mention it, keep it to one short sentence.

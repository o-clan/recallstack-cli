# Recallstack Memory (Claude Code Local)

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

Retrieval protocol:

1. Decide the source of truth before querying memory:
   - use the current thread for already-established answers
   - inspect code, logs, config, and tests for directly verifiable current-state facts
   - query Recallstack for rationale, prior decisions, historical context, related prior work, recurring issue history, user preference history, or non-local context
   - keep Recallstack operations in the background and avoid narrating turn ids, query wording, ingest steps, or other memory housekeeping unless the user asked, the operation failed, or the memory result materially changes the answer
2. If memory is the right source, query it directly:
   `recallstack memory query --query "<state/proof-oriented task>" --mode standard`
   Add `--synthesize` to draft a local answer from the retrieved evidence, or pass `--worker-model <model>` to both enable synthesis and choose the local worker model.
3. Use `mode=quick` for fast checks, `standard` for default recall, `deep` for fuller context, and `forensic` for investigative retrieval.
4. Do not use `--turn-id` for Claude retrieval. Claude hooks own turn ingestion and there is no manual query/ingest pairing to preserve.
5. Raw writes to `global` are forbidden. Ensure the configured target is non-global.

Claude hook behavior (installed by `recallstack agent install claude`):

1. The Claude `Stop` hook immediately queues a background worker and returns without delaying the main interaction loop.
2. The background worker reads the latest completed turn from the Claude transcript and current Stop payload.
3. The worker runs a headless `claude -p --output-format json --json-schema ... --no-session-persistence` extraction pass to produce a clean Recallstack summary without polluting the user's normal thread history.
4. Set the background extraction model by reinstalling with `recallstack agent install claude --worker-model <model>`.
5. Retryable ingest failures such as rate limits or unavailable backend responses are cached locally and retried in the background for up to 24 hours. Auth-required ingest failures are also cached locally and replayed after the next successful login.
6. The hook ingests that processed summary into the selected non-global project.
7. Because ingest is hook-driven, do not spend thread space describing memory housekeeping.

Routing guidance:

1. Claude hooks already ingest every turn, including user questions, research discussions, tradeoffs, interim reasoning, decisions, outcomes, and verified findings.
2. Non-evidence work still matters. If the turn is exploratory, comparative, or research-driven, let the hook capture the options considered, tradeoffs, and current leaning rather than trying to restate them manually in the thread.
3. Avoid raw terminal transcripts and noisy operational narration by default. The hook’s postprocessing pass is responsible for condensing actions and consequences into retrievable memory.
4. Durable context must use `recallstack memory source ingest` (MCP: `memory_ingest_source`) for plans, handovers, specs, checklists, research notes, copied docs, meeting notes, and verified issue-remedy runbooks.
5. Durable ingest is the default when the user says `remember this`, `store this`, `save this`, or asks you to write/update a long-lived plan, handover, checklist, spec, or reference note.
6. Use `memory_query` for blended recall and `memory_query_direct` for exact source passages or existing durable artifacts only when memory is the right source. Prefer `mode=standard` by default, `quick` for low-latency checks, `deep` for fuller context, and `forensic` for investigative retrieval.
7. Do not query memory for facts that are cheaper and more reliable to verify directly in code, config, logs, or tests.
8. For new work, frame memory queries around current state or prior related work, not the desired implementation outcome or an open-ended recommendation. Prefer `what already exists for X`, `find prior related work on X`, `why did we choose X`, or `is X already implemented`.
9. When an issue recurs, debugging starts looping, environment drift is suspected, or a familiar runtime error appears, query Recallstack memory before widening the search.
10. Check durable memory before drafting a new plan/spec/handover if prior documentation may already exist.
11. After a remedy is verified and likely reusable, ingest it durably as a concise issue-remedy note with a searchable title and metadata.
12. Treat optional client-side synthesis as a hint, not proof. Cross-check strong claims against the evidence list and local code before relying on them.
13. Use `memory_get_source` for full durable-source body drilldown.
14. Do not pollute the thread with memory activity. Recallstack use should usually be silent. If memory activity must be mentioned, keep it to one short sentence.

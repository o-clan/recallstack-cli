---
description: Use Recallstack only for prior decisions, non-local history, recurring issue context, and other context that is not cheaper to verify directly in the current workspace.
---

# Recallstack Memory (GitHub Copilot Local)

Required setup (once per repository):

1. `recallstack login <CODE>`
2. `recallstack workspace use`
3. `recallstack project use`
4. `recallstack project` (verify target)
5. Optional per-command override:
   add `--project <projectSlug|workspaceSlug/projectSlug>`

Retrieval protocol:

1. Decide the source of truth before querying memory:
   - use the current thread for already-established answers
   - inspect code, logs, config, and tests for directly verifiable current-state facts
   - query Recallstack for rationale, prior decisions, historical context, related prior work, recurring issue history, user preference history, or non-local context
   - keep Recallstack operations in the background and avoid narrating query wording, ingest steps, or other memory housekeeping unless the user asked, the operation failed, or the memory result materially changes the answer
2. If memory is the right source, call:
   `recallstack memory query --query "<state/proof-oriented task>" --mode standard`
   Add `--synthesize` to draft a local answer from the retrieved evidence, or pass `--worker-model <model>` to both enable synthesis and choose the local worker model.
3. Use `mode=quick` for fast checks, `standard` for default recall, `deep` for fuller context, and `forensic` for investigative retrieval.
4. Do not use manual turn ids for GitHub Copilot retrieval. Hooks own turn ingestion and there is no manual query/ingest pairing to preserve.
5. Raw writes to `global` are forbidden. Ensure the configured target is non-global.

GitHub Copilot hook behavior (installed by `recallstack agent install copilot`):

1. This integration supports GitHub Copilot CLI and the GitHub Copilot agent in VS Code.
2. Copilot CLI installs repo-local hook config under `.github/hooks/` for `sessionStart`, `userPromptSubmitted`, and `sessionEnd`.
3. VS Code installs repo-local hook config under `.github/hooks/` for `SessionStart`, `UserPromptSubmit`, and `Stop`.
4. Both variants point to the same Recallstack hook worker.
5. The worker reconstructs the finished turn from Copilot transcript/session-state artifacts, then runs a headless `copilot -p` extraction pass from an isolated temp config dir so the user's normal Copilot history is not polluted.
6. Set the background extraction model by reinstalling with `recallstack agent install copilot --worker-model <model>`.
7. Retryable ingest failures such as rate limits or unavailable backend responses are cached locally and retried in the background for up to 24 hours. Auth-required ingest failures are also cached locally and replayed after the next successful login.
8. Stop/session-end processing is queued in the background so the agent loop is not delayed.
9. Because ingest is hook-driven, do not spend thread space describing memory housekeeping.

Routing guidance:

1. GitHub Copilot hooks already ingest every turn, including user questions, research discussions, tradeoffs, interim reasoning, decisions, outcomes, and verified findings.
2. Non-evidence work still matters. If the turn is exploratory, comparative, or research-driven, let the hook capture the options considered, tradeoffs, and current leaning rather than trying to restate them manually in the thread.
3. Avoid raw terminal transcripts and noisy operational narration by default. The hook’s postprocessing pass is responsible for condensing actions and consequences into retrievable memory.
4. Use `recallstack memory source ingest` for durable plans, handovers, specs, checklists, research notes, copied docs, meeting notes, and verified issue-remedy runbooks.
5. Durable ingest is the default when the user says `remember this`, `store this`, `save this`, or asks for a long-lived plan, handover, checklist, spec, or reference note.
6. Use `memory_query` for blended recall only when the question is about history, rationale, prior related work, recurring issue history, user preference history, or non-local context. Prefer `mode=standard` by default, `quick` for low-latency checks, `deep` for fuller context, and `forensic` for investigative retrieval.
7. Do not query memory for facts that are cheaper and more reliable to verify directly in code, config, logs, or tests.
8. For new work, frame memory queries around current state or prior related work, not the desired implementation outcome or an open-ended recommendation. Prefer `what already exists for X`, `find prior related work on X`, `why did we choose X`, or `is X already implemented`.
9. When an issue recurs, debugging starts looping, environment drift is suspected, or a familiar runtime error appears, query Recallstack memory before widening the search.
10. Use `memory_query_direct` for exact durable-source passages or existing durable artifacts.
11. Check durable memory before drafting a new durable artifact if prior documentation may already exist.
12. After a remedy is verified and likely reusable, ingest it durably as a concise issue-remedy note with a searchable title and metadata.
13. Treat optional client-side synthesis as a hint, not proof. Cross-check strong claims against the evidence list and local code before relying on them.
14. Use `memory_get_source` for full-body drilldown.
15. Do not pollute the thread with memory activity. Recallstack use should usually be silent. If memory activity must be mentioned, keep it to one short sentence.

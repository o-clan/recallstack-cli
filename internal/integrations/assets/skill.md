---
name: recallstack
description: Use when the person decides something, corrects you, states a preference, leaves a question open or changes focus; when a plan goes against one of their decisions; when they narrow, reword, drop or take back a thought; and when they ask why something is the way it is. Runs recallstack note, hold, resolve, adjust, drop, undo and why.
---

# Recallstack

Each session opens with a `<recallstack_brief>`: the person's goals, decisions in force, open questions and taste. They are the person's intent, not facts about the code; check the code for those. Don't recite the brief. Mid-session, `<recallstack_update>` lines tell you what changed elsewhere; an adjusted-away or dropped thought stops applying from then on.

## Record what the person says

When the person decides something, corrects you, leaves a question open, states a preference, or the focus changes:

```
recallstack note <goal|focus|decision|question|taste> "<what>" --quote "<their exact words>"
```

Options: `--why "<why>"`, `--instead-of "<option>"` (repeat for each losing option), `--replaces <ref>` when it's different now, `--answers <ref>` when it answers an open question, `--where everywhere|project|project:<name>|path:<path>`. Taste defaults to everywhere, the rest to this project. For focus, write `"<what>. Next: <next step>"`.

The command prints one line. Don't add anything about it.

## When a plan goes against a decision in force

Don't act on it. Run:

```
recallstack hold <ref> --plan "<one line>"
```

Edits are paused until the person answers. Paste the decision block it prints into your question, using the decision's words, not its ref:

```
Before I <verb>: my plan <one-line plan>, and that goes against a decision you made.

  ● <decision>
    why: <why>
    <Agent> · <date> · "<quote>"

<What the plan gains, and what it costs against the decision.>

1. Change the plan: <the alternative that keeps the decision>.
2. Go ahead anyway: I'll note it as an exception to that decision, for <scope> only. The decision stays in force.
```

Recommend one of the two. Then stop and wait. Their answer settles it:

```
recallstack resolve <hold ref> change-plan
recallstack resolve <hold ref> exception --text "<what is allowed, and where>" --quote "<the exact words>"
recallstack resolve <hold ref> replace --text "<the new decision>" --quote "<the exact words>"
recallstack resolve <hold ref> cancel
```

Use `replace` only when they say the decision itself is wrong now. After an exception, say once: "If you've changed your mind about the decision itself, say so and I'll replace it."

## When two of the person's rules disagree (`Needs` in the brief)

Ask only when the work touches it. Run `recallstack hold <C ref> --plan "<one line>"`, show both sides as `a` and `b`, and ask "Which holds: a, b, or neither?" without recommending. Then:

```
recallstack resolve <C ref> a --quote "<their words>"        (or b)
recallstack resolve <C ref> neither --text "<what holds instead>" --quote "<their words>"
recallstack resolve <C ref> later
```

## When the person corrects a thought

```
recallstack adjust <ref> --where <scope> --quote "<their words>"      it applies somewhere else, or more narrowly
recallstack adjust <ref> --text "<new wording>" --quote "<their words>"
recallstack adjust <ref> --why "<reason>" --quote "<their words>"
recallstack drop <ref> --quote "<their words>"                        it doesn't apply anymore
recallstack undo                                                       they take back what they just said
```

Act only on what the person says about a thought, never on your own reading. If it isn't clear which thought they mean, ask once. The one-line result is the confirmation; add at most "Dropped." or similar. If the result says a file still says the same thing, offer once to edit that file to match.

## When asked why something is the way it is

```
recallstack why "<topic or ref>"
```

Answer in two or three lines from what it prints: the decision and its reason, the option that lost, then the person's words with agent and date. Add no reasons that aren't recorded. If nothing is recorded, say so, don't guess, and offer to note the reason if they give one.

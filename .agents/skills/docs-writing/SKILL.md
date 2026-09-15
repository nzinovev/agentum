---
name: docs-writing
description: Use when writing or editing repository documentation — README.md, any page under docs/, CONTRIBUTING.md, SECURITY.md, CODE_OF_CONDUCT.md — or when adding an entry to CHANGELOG.md. Covers how a rule is stated, what justification is allowed, when a table replaces prose, and what a changelog entry must say. Read it before writing the text, not after.
---

# Writing this repository's documentation

Repository-local skill: it governs how THIS repository is written, and nothing
else. It covers the Markdown written for people: `README.md`, `docs/*.md`,
`CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `CHANGELOG.md`.
Comments in Go are the `go-comments` skill. Prompts under `packs/` are
instructions an agent executes, not text a reader reads — leave their wording
alone. A project built with Agentum writes its own rules in its own skill; do
not copy these into a pack, a prompt, or a product page.

Terms come from `docs/domain-model.md`: `run`, `task` (a work item, not an
entity in the MVP), `stage invocation`, `project`, `repository`, `local
checkout`, `actor`. Do not coin a second word for something that file names.

<!-- shared-rules:begin — identical in both repository skills; `make skills-check` compares them -->
## How a rule is written

**1. State the rule once, and state it affirmatively.** A rule names what is
required, what happens when it is not met, and what the reader gets back: a
status code, a refusal, a stopped run. Do not restate the same rule beside
itself in a second wording.
*Violation:* the paragraph names a requirement but not its consequence, or
names the same requirement twice in different words.

**2. One sentence, one claim.** Concrete subject, active voice. A sentence
carrying more than one inserted clause — dash, parenthesis, colon — is two
sentences.
*Violation:* the reader has to pick separate statements out of one period.

**3. A reason is one fact in a sentence of its own.** Give it when the reader
cannot derive it: the cost of a paid call, the cost of a migration, the absence
of a table, the absence of production data. If there is no such fact, leave the
reason out.
*Violation:* the reason is carried by an image — a bill, a promise, honesty —
instead of a named fact; or it rides inside the sentence that states the rule.

**4. The system does not lie, promise, pay, or stay honest.** It returns,
records, refuses, stops, writes, skips. Name the mechanism and the value.
*Violation:* a system noun — a check, a refusal, a record, a field, a run — is
the subject of a verb of speech, intent, or morality: lies, promises, admits,
wants, learns, pays, is honest.

**5. Keep every exact value.** Status codes, error codes, header and field
names, environment variables, limits, defaults, file names, commands. Dropping
one of them is a regress, not a simplification. A paragraph with nothing to cut
stays as it is.
*Violation:* the diff removes a value and puts nothing in its place.

**6. A short contrast that names a boundary stays.** "A pause, not a failure",
"project-owned, not pack-owned". It defines an edge in four words; do not expand
it into two sentences.

**7. No internal decision numbers.** Never write `ADR 0005`, `D7`, `step 10.3`
or `F.6.1` into this repository — the documents they point at are not in it.
State the reasoning so it stands on its own: "the third refusal point", "the
write path speaks one schema".
*Violation:* a line this change adds matches `ADR [0-9]` or `\bD[0-9]`.
<!-- shared-rules:end -->

## Tables, not prose

**8. A list of cases and their answers is a table.** Case in the left column,
answer in the right. Prose that walks through three cases in a row is a table
written out longhand, and `docs/api.md` already has the tables next to it.
*Violation:* three or more case → answer pairs in a row in prose; prose that
repeats the table beside it.

## A changelog entry

**9. An entry says what changed, who notices it, and what to do.** In that
order, under one of `Added`, `Changed`, `Fixed`, `Removed`, with the PR number.

    - **<One sentence: what changed, in the reader's terms.>** <What it was
      before, or when it shows.> <What the reader does; "nothing" is an answer.>
      (#NN)

The bold lead is one sentence; the body is at most six lines.
*Violation:* the first sentence does not name the change; the entry does not say
who notices it; the entry does not say what to do.

## Before → after, from this repository

Rule 1, 3, 4 — `docs/api.md`:

> **`Idempotency-Key` is mandatory.** An optional guard against a double click
> is no guard: the client that omits it pays for the call twice and learns it
> from the bill. Absent header → `400 bad_input` naming it.

> **`Idempotency-Key` is required.** Without it, a retried request runs a second
> check and pays for it. A request without the header returns `400 bad_input`
> naming the header.

Rule 4 — `docs/models.md`:

> The explicit check is honest about its boundary: a model that emits one line
> and goes quiet passes it.

> The check covers one thing: the model produced a first output line. A model
> that emits one line and then goes quiet passes it; mid-run silence is the
> first-event watchdog's and the idle cap's job.

Rule 9 — `CHANGELOG.md`:

> - **A `variant` in a model-check request is refused, not echoed-and-dropped.**
>   The response and events carried the requested variant while the check ran
>   without it — the silently-dropped-parameter move the model rules forbid
>   everywhere else.

> - **A model-check request carrying `variant` returns `400 bad_input` naming
>   the field.** Before this, the response and events echoed the variant while
>   the check ran without it. If you send `variant` to
>   `POST /api/v1/models/test`, drop the field until the adapter accepts one.
>   (#NN)

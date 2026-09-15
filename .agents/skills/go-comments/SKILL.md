---
name: go-comments
description: Use when writing or editing comments in Go files in this repository: package and declaration doc comments that `go doc` prints, comments inside function bodies, test comments, and the comment blocks in internal/store/queries/*.sql that sqlc copies into generated code. Covers how a doc comment opens, what justification is allowed, and how a test comment names the property it protects.
---

# Comments in this repository's Go code

Repository-local skill: it governs how THIS repository's Go is commented, and
nothing else. A doc comment is the package's public documentation: `go doc`
prints it, and it is read by someone working out what the code does. Markdown
written for people is the `docs-writing` skill.

Terms come from `docs/domain-model.md`: `run`, `stage invocation`, `project`,
`repository`, `local checkout`, `actor`.

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

## A doc comment

**10. Open with the name of the declaration and what it does.** The reason is
the next sentence, not the first one.
*Violation:* the first sentence is the justification and the name arrives later
or not at all.

**11. Keep the reason.** `AGENTS.md` and `CONTRIBUTING.md` both require a
comment to say *why*. Simplifying removes the image, never the knowledge. A
comment you can no longer reconstruct the decision from is a regress.
*Violation:* after the edit, the reason is gone and it did not move into the
next sentence.

**12. A test comment names the property the test protects.** Not the mechanics
of the test, not a verdict on the code.
*Violation:* "keeps the wiring honest", "makes sure things are fine" — a
judgment where a property belongs.

**13. A generated comment is edited at its source.** The comments in
`internal/store/sqlc/*.sql.go` come from `internal/store/queries/*.sql`: edit
the `.sql` file, then run `make sqlc-gen`. Never edit the generated file.
*Violation:* the diff touches `internal/store/sqlc/*.sql.go` with no matching
change in `internal/store/queries/*.sql`.

## What this skill does not ask for

- Do not add comments that are missing. This is about what is written, not about
  coverage.
- Do not change code to make a comment simpler. If the comment is hard because
  the code is tangled, fix the comment and write the code down separately.

## Before → after, from this repository

Rule 4 — `internal/models/catalog.go`:

> // An empty-yet-Available catalog is treated as not obtained rather than as
> // "the runtime runs nothing" — the second reading would refuse every model
> // with a text that lies about the reason.

> // An empty catalog that reports Available counts as not obtained. Reading it
> // as "the runtime runs nothing" would refuse every model and name a reason
> // that is wrong.

Rule 4 — `internal/runner/checks.go`:

> // Ran reflects whether any check actually executed (!set.Empty()), so an
> // empty set is recorded honestly rather than as a cleared gate.

> // Ran reflects whether any check actually executed (!set.Empty()), so an
> // empty set is recorded as "no checks ran" rather than as a passed gate.

Rule 12 — `internal/runner/evidence_test.go`:

> // TestCaptureStageOutputs_NilStoreIsANoop keeps the unit-test wiring honest:

> // TestCaptureStageOutputs_NilStoreIsANoop: capture with a nil store writes
> // nothing and returns no error, so a unit test without a store runs the same
> // path as a test with one.

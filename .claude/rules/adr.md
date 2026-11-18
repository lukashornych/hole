# Architectural Decision Records

Every line of work whose reasoning clears the bar below ends with **one record in
`documentation/adr/`** saying what was done, when, why, and how it relates to the rest. The record
replaces the plan it grew out of. Template: `documentation/adr/TEMPLATE.md`. Index and conventions
summary: `documentation/adr/README.md`.

## Before starting work

Search `documentation/adr/` for the area you are about to touch (`rg -l "internal/<package>"
documentation/adr/`) and read the **Consequences & open follow-ups** section of every hit. Prior
decisions and knowingly-deferred work live there; a change that contradicts one needs a new record
that `supersedes` it, not a silent reversal.

## When a record is required — the bar

**The default is no record.** Most work does not clear this bar, and that is the intended outcome:
every low-value record dilutes the folder and trains the next reader to skim past the ones that
matter. A thin ADR is worse than none.

Write one only when **all three** hold. If you are arguing yourself into one of them, the answer is
no.

1. **There was a genuine fork.** A competent engineer might reasonably have chosen a different
   option, and it was rejected for a reason the code does not show. No fork — no decision — no
   record. "It was hard" is not a fork; "we picked B over A because A breaks X" is.
2. **The reasoning outlives the change.** It constrains or informs work *beyond the files you
   touched* — a contract other packages code against, an invariant a future change could silently
   break, an option someone will otherwise re-propose. If the reasoning only matters to someone
   already reading that file, it belongs **in that file**, as a comment.
3. **Nothing cheaper carries it.** A commit message and a Go doc comment are the first choice; the
   developer documentation in `documentation/developer/` is the second. Reach for an ADR only when
   the *reasoning* (not the description of how things work) has to be findable by someone who does
   not yet know which file to open.

**Size is not the test, in either direction.** A one-line change can clear it — a single default
value that carries a measured decision and rejected alternatives. A thousand-line mechanical
refactor with no fork in it does not clear it at all.

### Never a record

Typo and formatting fixes; renames; dependency bumps; pinned digest or version bumps; regenerated
golden files; test fixture tweaks; single-call-site refactors; config changes whose rationale is
obvious from the value itself; documentation edits; anything a reader of the diff plus its commit
message would already understand.

### Where sub-threshold reasoning goes

It still gets written down — just not here. Put it in the commit message when it explains *why this
change*, and in a code comment or doc comment when it explains *why this code stays this way*. A
comment at the site beats a record in a folder for anything file-local, because the next person to
touch that line will actually see it.

### Batching

**One record per line of work, not per commit or per PR.** A campaign spanning several PRs gets one
record covering all of them. Several small related changes in one area get **one** record at the end,
if they clear the bar together — not one each. If a sub-threshold change later turns out to matter,
fold it into the next record that covers its area rather than back-filling its own.

## Where and what to name it

- **Default — one file:** `documentation/adr/YYYY-MM-DD-<slug>.md`.
- **Only when justified — a directory:** `documentation/adr/YYYY-MM-DD-<slug>/README.md` plus
  supporting files, when the work produced *several independent decisions* **and** evidence worth
  keeping. Keep no file that cannot be given a one-line reason in the record's *Supporting
  material* section.

**Which date.** The date the decision was **accepted** — normally the merge date of the
implementing PR (`git log -1 --format=%ad --date=short <merge-commit>`). Not the date the document
was written, not the day the work started. Tie-breaks: several PRs → the last one that completed
the line of work; decision accepted but not yet implemented → the date it was accepted, with
`status: proposed`; never merged → the date it was abandoned, with `status: rejected`.

`date:` never changes once set, and must match the date in the filename — the index generator
fails when they disagree. `updated:` carries date **and** time and moves with every substantive
edit.

## Front matter

Required on every record — it is what makes the folder greppable (`rg -l "status: proposed"
documentation/adr/`) and the index generated rather than hand-maintained:

```yaml
title, date, updated, status, kind, issues, prs, areas, supersedes, superseded-by, relates
```

- `status`: `accepted` | `partially-implemented` | `proposed` | `superseded` | `rejected` | `reverted`
- `kind`: `feature` | `optimization` | `fix` | `refactor` | `infrastructure` | `process`
- `areas`: source paths (`internal/<package>`, `assets/<dir>`, `cmd/hole`, `scripts/`, …), so `rg`
  finds the record from a file path
- Use `[]` for empty lists rather than omitting the key.

## Content rules

Sections and their required/omit-if-empty status are defined in `documentation/adr/TEMPLATE.md`.
Beyond that:

- **Delete sections you have nothing for.** Never leave "N/A" — a record padded with empty headings
  teaches the next reader to skim past them.
- **Options considered must be honest.** Record the alternative that was genuinely on the table
  with its real advantages. Do not invent a strawman to fill the section; if there was one viable
  path, say so in a sentence.
- **Every rejection carries its reason, next to the option it rejects.** A declined option gets a
  **Rejected because** line; a campaign's *Rejected outright* table gets a filled *Rejected because*
  column. Name the concrete blocker — a docker/podman or compose limitation, kernel, nftables or
  CoreDNS behavior, a dependency that costs more than it carries, a flat measurement — not a
  preference, and say what would have to change for it to be worth revisiting. An unexplained veto
  is worse than no entry: the next agent re-proposes it, or worse, obeys it without knowing whether
  it still applies.
- **Key technical details stay shallow.** Entry points, invariants, and anything that will look
  like a bug to someone who wasn't here. Pointers to code, not a re-description of it — the code
  and `documentation/developer/` are the source of truth and the record must not rot alongside them.
- **Quote numbers in Verification.** Name the test that proves it, or the manual scenario and what
  it ran on (suite — unit / integration / e2e; runtime — docker or podman and its version; host
  OS); for optimizations give before/after and the conditions they were measured under.
- **Cross-link in both directions.** Adding `relates`/`supersedes` to a new record means editing the
  older one too — and bumping the older record's `status` to `superseded` plus its `superseded-by`.
- **Regenerate the index** in the same commit: `scripts/generate-adr-index.sh`. Never hand-edit the
  table between the `ADR-INDEX` markers — it is overwritten. `--check` verifies it is current and
  is safe to run from CI or a hook (`make lint` runs it).

## No customer attribution

**Never introduce the name of a customer, employer or third-party user into a decision record, its
supporting files or filenames, the developer or user documentation, code (identifiers, comments,
test fixtures, golden files), `.claude/**`, or the commit messages that carry any of them.** Keep
every number; drop every name. A name scrubbed from a file but left in `git log` has not been
scrubbed.

A record is written to be read by someone who was not there, and the reporter's identity is never
the part that helps them. What helps is the shape of the problem: which runtime and version, which
host OS and kernel setup, how many allow-list entries, what the gateway logged and what the sandbox
did with it.

**Forbidden**: customer, company, brand or project names, standalone or embedded in an identifier;
private hostnames, internal registry or proxy URLs and allow-list domains that identify someone's
infrastructure (public, generic domains such as `github.com` or a package registry are fine); real
host paths, usernames and project directory names from someone's machine; tokens, credentials and
any value copied from a real environment; internal source documents that cannot be published (cite
these as "(internal, §X)", never link or quote them).

**Keep, always**: every number, ratio and distribution — allow-list sizes, startup and teardown
times, image sizes, subnet counts, connection and DNS-query counts.

**How to say it instead.** Describe the setup, not the person: "a corporate laptop behind an
intercepting proxy, podman 5.2 on macOS". Where two environments must be told apart, label them by
shape or by letter ("host A" / "host B") and define the label once. For sample data, give the
*shape* and never the value: "an internal registry under a private second-level domain",
"a 40-character token". Generic vocabulary (`registry.example.com`, `/home/user/project`) is fine; a value
that carries someone's own identity is not — replace it with its generic equivalent and say you did.

**When you find a violation in an existing record**, say so rather than fixing it silently: the
anonymization may need a decision about which label to use, and rewriting history is the author's
call, not yours.

## Plans and analyses, and how they end

In-flight working notes live in two git-ignored folders (`documentation/.gitignore`):

- **`documentation/plans/<branch-or-topic>.md`** — an implementation plan, written when one is
  requested. A plan with supporting files may become a folder of the same name.
- **`documentation/analysis/<topic>.md`** — an analysis: the facts the developer and their team need
  to make a decision, without an implementation plan.

Both hold *intent*, never outcome: nothing in them is evidence of what shipped, and they must never
be read as such. **They are never committed** — the folders are git-ignored so that `git add -A`
cannot silently violate the rule. For the same reason, no tracked file may link into them: carry the
conclusion into the tracked document (or a record) instead.

**Every plan must leave the disk when its work finalizes** — merged, abandoned, or decided against.
There are exactly two exits, and no third:

1. **It becomes a record**, if the work clears the bar above. Write the ADR, then delete the plan.
2. **It is deleted outright**, if the work does not. The commit message of the last change carries
   anything worth keeping.

**An analysis is deleted only when an implementation plan has been created from it and the
developer agrees with that plan.** Carry the facts that matter for the decision into the plan first
(they later reach the record through it — typically *Why* and *Options considered*). Until then the
analysis stays on disk, whatever else happens to the work; never delete it on your own judgement.

**Because these files are not in git, deleting them is irreversible.** Everything of lasting value
must be inside the record *before* the file goes — the rejected options and why they lost,
measurements that cannot be regenerated, ordering hazards a future step could trip over. Read the
plan through once against the finished record and ask what would be unrecoverable, rather than
deleting on the assumption it was all captured.

**A plan left behind is worse than no plan at all.** It reads as current intent, and the next reader
cannot tell whether it shipped, half-shipped, or was dropped — they have to re-derive that from git,
which is the exact cost this convention exists to remove.

**A partially-implemented plan is usually a record, not a deletion.** When some items shipped and
others were dropped, the dropped ones carry a rejection reason that git cannot show and that someone
will otherwise re-propose — that is a genuine fork, and it clears the bar. Write down *why* they
lost, not merely that they did.

Keep only material with future value that a record cannot absorb: measurements that cannot be
regenerated, reproduction scenarios, advisory verdicts that were relied on. Never keep raw
`docker inspect` or `nft list ruleset` dumps, packet captures or logs — capture their conclusions
and drop the bytes.

## Commit

`docs: record <decision> as an ADR`, with `Refs: #<issue-id>` when the work has an issue, following
[guidelines — git workflow](../../documentation/developer/guidelines.md#git-workflow). Committing
the record together with the implementation is fine and preferred; the record must not land in a
later PR than the work it describes.

# Architectural decision records

Every change to Hole whose reasoning has to outlive it leaves one record here explaining **what
was done, when, why, which alternatives were declined and why they lost, and what it relates to**. A
record replaces the plan it grew out of: plans describe intent that may never have shipped, records
describe what actually happened. Most changes do not need one — the bar defaults to no.

- **Writing one:** [`TEMPLATE.md`](TEMPLATE.md) — two shapes, point decision and campaign.
- **Rules** — when a record is required, which date it carries, what to keep and what to delete:
  `.claude/rules/adr.md`.
- **Starting new work?** Search here first. `rg -l "internal/<package>" documentation/adr/` and read the
  **Consequences & open follow-ups** section of every hit — that is where the known traps and the
  deliberately-deferred work live.

## Conventions in one paragraph

A record is `YYYY-MM-DD-<slug>.md` in this folder — the date is when the decision was **accepted**,
normally the merge date of the implementing PR, so `ls` reads chronologically. When a line of work
produced several decisions *and* evidence worth keeping, it becomes `YYYY-MM-DD-<slug>/` with the
record at `README.md` and the evidence beside it — never more files than earn their place. Every
record opens with YAML front matter (`status`, `kind`, `issues`, `prs`, `areas`, `relates`,
`supersedes`), which is what makes this folder greppable — `rg -l "status: proposed"
documentation/adr/` — and the index below generated rather than hand-maintained.

## Index

Newest first. **Generated** by `scripts/generate-adr-index.sh` from each record's front matter — edit
the records, then re-run it; edits made directly to the table are overwritten. `--check` fails when
the table is stale, and the script also reports records with missing front-matter fields or a
filename date that disagrees with `date:`.

<!-- ADR-INDEX:START -->

| Date | Record | Kind | Status | Refs |
|------|--------|------|--------|------|
| 2026-11-12 | [Expose npm-installed agent CLIs by bare name through a baked wrapper declared in command-bare.json](2026-11-12-command-bare-wrappers.md) | feature | accepted | #67 |

<!-- ADR-INDEX:END -->

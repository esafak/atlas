# `schema apply` fan-out contract

`atlas schema apply --env NAME` freezes the expanded HCL environment set before
it opens or changes a target. The expanded set is sorted by a redacted target
identity (`target-` followed by the first 12 hex characters of SHA-256(URL));
duplicate URLs are rejected. Credentials and raw URLs are never included in
terminal or JSON output.

The command first computes every target plan. A plan contains the current and
desired state fingerprints, the exact SQL statements, diagnostics, typed risk
classes, and a SHA-256 plan hash. A batch report has the version
`atlas.schema.apply.fanout/v1`, a cohort hash, and the ordered plan records.
`--report FILE` writes this report with mode `0600`; at most 100 target
summaries are printed inline.

Each expanded target must resolve to a distinct disposable `dev` URL. The
command rejects a shared dev URL rather than allowing normalization state to
leak between plans. The fixture owner provisions the ephemeral TiDB, loads the
global dependency before invoking Atlas, resets each target database before a
batch, and removes all target databases after the batch (including failure
cleanup). Atlas consumes these URLs and closes its connections; it does not
create cluster resources or print credentials.

`--dry-run` plans and reports without executing target DDL. Otherwise one
interactive confirmation authorizes the complete batch. Targets are then
applied sequentially. A target whose plan is not explicitly allowed is
reported as failed when it contains a risk class. The supported repeatable
`--allow-risk=CLASS` values are `destructive`, `index`, `foreign-key`,
`column-rewrite`, and `data-dependent`; `--auto-approve` cannot be combined
with this flag. Risk overrides require a second confirmation, require `--report`,
and are therefore bound to the saved cohort and per-target plan hashes.
They do not permit data transformations or a blanket override.

The command acquires the target lock and rechecks the current fingerprint
before each apply. A changed target is skipped as drifted and is reported as
retryable. `--retry-report FILE` restricts execution to the exact target and
plan-hash set in a prior report; changed hashes or an expanded cohort are
rejected. Single-target apply keeps its existing behavior.

Each saved plan record includes a `status`, optional `error`, and optional
`warning`. An unlock failure after a committed apply is reported as a warning
while retaining `success` (or `no-op`); the terminal also points operators to
the saved report.

TiDB versions where `GET_LOCK` is unsupported or behaves as a no-op use
fingerprint revalidation as the fallback guard. This does not exclude external
writers; operators must stop writers that do not participate in the Atlas lock
protocol and treat the post-plan race as residual risk.

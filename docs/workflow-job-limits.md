# GitHub job time limits

`potluck runners jobs` shows each job's runtime limit in seconds and where it
came from. JSON includes `limit_s`, `limit_source`, `started_at` and `deadline`.
The runtime limit is separate from the box's renewable lease and the lender's
hard lifetime ceiling (`ttl_max_s`). The host must fit the runtime plus 300
seconds to boot and register. A job that needs more stays waiting with that
capacity reason; the broker never silently shortens its requested runtime.

The exported `RunsOn` rule checks the configured fallback runtime plus startup
against the same alias and fitness rule as placement. It describes default
eligibility; an individual workflow can request a different runtime. A size
alias selects its binding without bypassing image, OS, trust, availability or
host lifetime checks.

For push and workflow_dispatch runs, the broker reads the workflow file at the
run's immutable head SHA. It uses a literal positive integer `timeout-minutes`
when a unique static job key or literal `name` identifies the job. It refuses to
guess when names compete, a matrix or reusable workflow appears, or the name or
timeout uses an expression. PR workflow revision identity is also unresolved:
the run's code SHA does not prove the workflow revision. Other events currently
use the fallback too. These cases print the reason in `limit_source`.

The fallback is `potluck runners set job_ttl_s <seconds>` (3600 when unset),
changed live for the next job. A workflow that omits timeout-minutes uses this
explicit broker policy. This is not GitHub's own default of 360 minutes: GitHub
still enforces its independently evaluated timeout. The broker accepts literal
limits up to its five-day policy ceiling; the chosen host must still fit them.

GitHub's `started_at` fixes the runtime deadline, including when a trusted job
takes a runner minted for another job. Each actual job owns one selected
budget and execution clock; the box holder refers to it through the assignment,
so another job running on that holder's own box keeps its own clock. If GitHub omits that timestamp, the broker
records its first observed start once and says so in the source. Duplicate start
deliveries do not extend the deadline. The original job keeps its selected
runtime policy across re-serves, even when a trusted taker used a different
budget or the live fallback setting changes. The normal box watch (30 seconds by
default) requests release at the deadline and prints that its job time limit was
reached, a stop was requested, and it will not be re-run. The first proven
broker timeout decision or GitHub timed_out conclusion is stored on the actual
job, so late completion or another job using its box cannot erase the reason. An earlier expired lease, a reclaimed box or
an unexpected host loss still follows the lost-runner recovery policy.

New GitHub Apps request Contents: read to read workflow files, using a separate
one-repository read token. Existing Apps may need their permission setting
updated and installation owners to accept that update in GitHub. Until then the
job uses the configured fallback and prints the missing-permission explanation.

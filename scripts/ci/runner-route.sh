#!/usr/bin/env bash
# Picks the runs-on label for a CI job: ROUTE_LABEL (a self-hosted scale set) when the capacity report at
# CAPACITY_URL says it has a free runner, else ROUTE_FALLBACK. The workflows take ROUTE_LABEL and CAPACITY_URL from
# the repository variable CI_ROUTE_LABEL and the repository secret CI_CAPACITY_URL; with either unset (a fork, a fresh copy of the
# repository) the fallback is picked without a request. A report is trusted only if it is fresh (at most 90 s old),
# ready, no guard closed, GitHub not refusing runners, and free[ROUTE_LABEL] >= 1. Any doubt (no answer within 3 s,
# no jq, an unreadable report) picks the fallback. ROUTING=off (the repository variable CI_ROUTING; unset is on)
# always picks the fallback. Writes label=<it> to $GITHUB_OUTPUT when set.
# CAPACITY_JSON, when set, is the report to read instead of fetching it (tests).
set -u
label=${ROUTE_FALLBACK:?}
if [ "${ROUTING:-}" = off ]; then
  why="routing is off (CI_ROUTING)"
elif [ -z "${ROUTE_LABEL:-}" ] || { [ -z "${CAPACITY_URL:-}" ] && [ -z "${CAPACITY_JSON+x}" ]; }; then
  why="no route configured (CI_ROUTE_LABEL and CI_CAPACITY_URL)"
elif ! command -v jq >/dev/null; then
  why="no jq on the route runner"
else
  body=${CAPACITY_JSON-$(curl -fsS --proto '=https' --max-time 3 "$CAPACITY_URL" || true)}
  if printf '%s' "$body" | jq -e --arg p "$ROUTE_LABEL" '
      .schema == "thefactory-capacity-v1" and .ready == true
      and (.pool.closed | type == "array" and length == 0)
      and .pool.github_refusing == false
      and ((now - ((.at // "") | sub("\\.[0-9]+"; "") | sub("\\+00:00$"; "Z") | fromdateiso8601)) as $age
           | $age >= 0 and $age <= 90)
      and (.free[$p] | type == "number" and . >= 1)' >/dev/null 2>&1; then
    label=$ROUTE_LABEL why="a free $ROUTE_LABEL runner"
  else
    why="no free $ROUTE_LABEL runner (or the report did not answer)"
  fi
fi
echo "runner route: $label ($why)"
[ -n "${GITHUB_OUTPUT:-}" ] && echo "label=$label" >> "$GITHUB_OUTPUT"
exit 0

#!/usr/bin/env bash
# Auto-merge open pull requests whose CI has passed.
#
# Runs from .github/workflows/automerge.yml on the DEFAULT BRANCH (via
# workflow_run and schedule), so a pull request cannot change this script or
# the rules it applies. A PR is squash-merged only when ALL of these hold:
#
#   - it targets main, is open, is not a draft, and has no hold label;
#   - it is not from a bot account (unless ALLOW_BOTS=true);
#   - it does not touch PROTECTED_PATHS (CI, release and build definitions:
#     a PR runs its own copy of the workflows, so one that edits them could
#     make its own checks pass) and does not delete tests;
#   - every check in REQUIRED_CHECKS ran on its head commit and succeeded,
#     and every other check on that commit finished without failing;
#   - those checks ran after main's current head commit, so they tested the
#     PR against the code it would actually merge into;
#   - GitHub reports it mergeable with no conflicts.
#
# Anything else is left for a maintainer, with one explanatory comment and a
# label, so the author knows why. At most one PR is merged per run, because a
# merge moves main and makes every other PR's checks stale.
#
# Environment:
#   GH_TOKEN          token with contents:write and pull-requests:write
#   GITHUB_REPOSITORY owner/name (set by Actions)
#   REQUIRED_CHECKS   space-separated check names that must pass; a name also
#                     covers its matrix runs, so "test" requires every
#                     "test (...)" run to pass
#   PROTECTED_PATHS   space-separated path prefixes a PR must not touch
#   CI_WORKFLOW       workflow file to run on main after a merge (optional)
#   ALLOW_BOTS        "true" to also merge bot-authored PRs (default false)
#   DRY_RUN           "true" to report decisions without merging
set -euo pipefail

for tool in gh jq awk; do
  command -v "$tool" >/dev/null || { echo "automerge: $tool is required" >&2; exit 1; }
done

repo="${GITHUB_REPOSITORY:?}"
required="${REQUIRED_CHECKS:-}"
protected="${PROTECTED_PATHS:-.github/}"
allow_bots="${ALLOW_BOTS:-false}"
dry_run="${DRY_RUN:-false}"

hold_label="needs-maintainer-review"
stale_label="needs-update"
marker="<!-- automerge-bot -->"

log() { printf '%s\n' "$*"; }

ensure_label() {
  [ "$dry_run" = true ] && return
  gh label create "$1" -R "$repo" -c "$2" -d "$3" >/dev/null 2>&1 || true
}

# comment_once posts a comment unless the bot already left the same reason.
comment_once() {
  local pr="$1" reason="$2" body="$3"
  local tag="${marker}<!-- reason:${reason} -->"
  if gh api "repos/$repo/issues/$pr/comments" --paginate --jq '.[].body' | grep -qF "$tag"; then
    return
  fi
  if [ "$dry_run" = true ]; then
    log "  would comment ($reason)"
    return
  fi
  gh pr comment "$pr" -R "$repo" --body "$tag
$body" >/dev/null
}

# matching_checks prints the distinct check names on the current PR that a
# required name covers: the name itself and its matrix runs "<name> (...)".
matching_checks() {
  awk -F'\t' -v n="$1" '$1 == n || index($1, n " (") == 1 { print $1 }' <<<"$checks" | sort -u
}

hold() {
  local pr="$1" label="$2" reason="$3" body="$4"
  log "  HOLD #$pr: $reason"
  [ "$dry_run" = true ] || gh pr edit "$pr" -R "$repo" --add-label "$label" >/dev/null 2>&1 || true
  comment_once "$pr" "$reason" "$body"
}

if [ -z "$required" ]; then
  log "REQUIRED_CHECKS is empty: this repository has no CI to gate on, so nothing is merged automatically."
  exit 0
fi

ensure_label "$hold_label" "d93f0b" "Auto-merge skipped this PR; a maintainer must review it"
ensure_label "$stale_label" "fbca04" "CI ran against an older main; update the branch to re-run it"

main_sha=$(gh api "repos/$repo/commits/main" --jq '.sha')
main_time=$(gh api "repos/$repo/commits/main" --jq '.commit.committer.date')
log "main is $main_sha ($main_time)"

prs=$(gh pr list -R "$repo" --state open --base main --limit 100 \
  --json number,isDraft,headRefOid,author,labels,title)

for pr in $(jq -r '.[].number' <<<"$prs"); do
  row=$(jq -c --argjson n "$pr" '.[] | select(.number == $n)' <<<"$prs")
  sha=$(jq -r '.headRefOid' <<<"$row")
  author=$(jq -r '.author.login' <<<"$row")
  is_bot=$(jq -r '.author.is_bot // false' <<<"$row")
  log "PR #$pr by $author at $sha"

  if [ "$(jq -r '.isDraft' <<<"$row")" = true ]; then
    log "  skip: draft"; continue
  fi
  if jq -e --arg l "$hold_label" '[.labels[].name] | index($l)' <<<"$row" >/dev/null || \
     jq -e '[.labels[].name] | index("do-not-merge")' <<<"$row" >/dev/null; then
    log "  skip: on hold"; continue
  fi
  if [ "$allow_bots" != true ] && { [ "$is_bot" = true ] || [[ "$author" == *"[bot]" ]] || [[ "$author" == app/* ]]; }; then
    log "  skip: bot-authored"; continue
  fi

  # Files: protected paths and deleted tests both need a human.
  #
  # The list comes from comparing main's current head with the PR head, not
  # from the PR's files endpoint: that endpoint diffs against the base commit
  # GitHub recorded for the PR, which can lag behind main, so a PR that merged
  # main would be charged with main's own changes (e.g. to .github/).
  files=$(gh api "repos/$repo/compare/$main_sha...$sha" \
    --jq '.files[] | "\(.status) \(.filename)"')
  touched=""
  for prefix in $protected; do
    hit=$(awk -v p="$prefix" 'index($2, p) == 1 { print $2 }' <<<"$files" | head -5)
    [ -n "$hit" ] && touched="$touched $hit"
  done
  if [ -n "$touched" ]; then
    hold "$pr" "$hold_label" "protected-paths" "Auto-merge skipped this PR because it changes files that define how CI, builds or releases run:
$(for f in $touched; do printf -- '- `%s`\n' "$f"; done)
A pull request runs its own copy of these files, so a change to them could alter the checks that would approve it. A maintainer will review it."
    continue
  fi
  deleted_tests=$(awk '$1 == "removed" && ($2 ~ /_test\.go$/ || $2 ~ /(^|\/)testdata\//) { print $2 }' <<<"$files" | head -5)
  if [ -n "$deleted_tests" ]; then
    hold "$pr" "$hold_label" "deleted-tests" "Auto-merge skipped this PR because it deletes tests or test fixtures:
$(for f in $deleted_tests; do printf -- '- `%s`\n' "$f"; done)
Passing CI cannot show that removed tests were safe to remove, so a maintainer will review it."
    continue
  fi

  # Checks on the head commit, excluding this bot's own job.
  checks=$(gh api "repos/$repo/commits/$sha/check-runs?per_page=100" --paginate \
    --jq '.check_runs[] | select(.name != "automerge") | [.name, .status, (.conclusion // ""), (.completed_at // "")] | @tsv')
  if [ -z "$checks" ]; then
    log "  wait: no checks have run yet"; continue
  fi

  missing="" failed="" pending=""
  for name in $required; do
    # A required name also covers a matrix job's runs, which GitHub names
    # "<job> (<value>)", e.g. "test (ubuntu-latest)". Every such run must pass.
    runs=$(matching_checks "$name")
    if [ -z "$runs" ]; then missing="$missing $name"; continue; fi
    while IFS= read -r run; do
      # A check can run more than once (re-runs); use the latest attempt.
      line=$(awk -F'\t' -v n="$run" '$1 == n' <<<"$checks" | sort -t$'\t' -k4,4 | tail -1)
      IFS=$'\t' read -r _ status conclusion _ <<<"$line"
      if [ "$status" != completed ]; then pending="$pending $run"
      elif [ "$conclusion" != success ]; then failed="$failed $run"; fi
    done <<<"$runs"
  done
  others_bad=$(awk -F'\t' '$2 == "completed" && $3 != "success" && $3 != "neutral" && $3 != "skipped" { print $1 }' <<<"$checks" | sort -u | tr '\n' ' ')
  others_pending=$(awk -F'\t' '$2 != "completed" { print $1 }' <<<"$checks" | sort -u | tr '\n' ' ')

  if [ -n "$failed$others_bad" ]; then
    log "  skip: failing checks:$failed $others_bad"; continue
  fi
  if [ -n "$pending$others_pending$missing" ]; then
    log "  wait: pending or missing checks:$pending $others_pending$missing"; continue
  fi

  # Freshness: every required check must have finished after main last
  # moved, or it tested the PR against code that is no longer main.
  stale=""
  for name in $required; do
    while IFS= read -r run; do
      done_at=$(awk -F'\t' -v n="$run" '$1 == n { print $4 }' <<<"$checks" | sort | tail -1)
      if [[ "$done_at" < "$main_time" ]]; then stale="$stale $run"; fi
    done < <(matching_checks "$name")
  done
  if [ -n "$stale" ]; then
    hold "$pr" "$stale_label" "stale" "Main has moved since this PR's checks ran, so they no longer show it works with the current code. Please update your branch (the **Update branch** button, or merge or rebase \`main\`) to re-run CI; it will be merged automatically once the new checks pass."
    continue
  fi
  # The branch has been updated since it was marked stale; say so.
  if jq -e --arg l "$stale_label" '[.labels[].name] | index($l)' <<<"$row" >/dev/null; then
    [ "$dry_run" = true ] || gh pr edit "$pr" -R "$repo" --remove-label "$stale_label" >/dev/null 2>&1 || true
  fi

  state=$(gh pr view "$pr" -R "$repo" --json mergeable,mergeStateStatus --jq '"\(.mergeable) \(.mergeStateStatus)"')
  if [ "${state%% *}" != MERGEABLE ]; then
    log "  skip: not mergeable ($state)"; continue
  fi

  if [ "$dry_run" = true ]; then
    log "  WOULD MERGE #$pr"
    exit 0
  fi

  # The squash message is the PR's own title and description. GitHub's
  # default folds in every intermediate commit message and its trailers
  # (maintainer merge commits, tool attributions), which then accumulate on
  # main as co-author credit for commits that were never the author's work.
  title=$(jq -r '.title' <<<"$row")
  body=$(gh pr view "$pr" -R "$repo" --json body --jq '.body // ""')

  # --match-head-commit refuses the merge if the author pushed after the
  # checks we just read, so we never merge untested code.
  if gh pr merge "$pr" -R "$repo" --squash --match-head-commit "$sha" \
      --subject "$title (#$pr)" --body "$body"; then
    log "  MERGED #$pr"
    gh pr edit "$pr" -R "$repo" --remove-label "$stale_label" >/dev/null 2>&1 || true
    # GitHub does not act on "Closes #N" when the merge is made with the
    # workflow token, so close the PR's linked issues here.
    for issue in $(gh pr view "$pr" -R "$repo" --json closingIssuesReferences \
        --jq '.closingIssuesReferences[].number'); do
      gh issue close "$issue" -R "$repo" --reason completed \
        --comment "Resolved by #$pr, which has been merged." >/dev/null 2>&1 \
        && log "  closed #$issue" || log "  could not close #$issue"
    done
    # Pushes made with the workflow token do not trigger workflows, so run
    # CI on main explicitly to confirm the merged result.
    if [ -n "${CI_WORKFLOW:-}" ]; then
      gh workflow run "$CI_WORKFLOW" -R "$repo" --ref main || log "  could not dispatch $CI_WORKFLOW on main"
    fi
    exit 0
  fi
  log "  merge failed for #$pr"
done

log "nothing to merge"

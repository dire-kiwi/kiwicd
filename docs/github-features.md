# GitHub features we could replicate

A list of GitHub (mostly GitHub Actions) features kiwicd doesn't have yet, for choosing what to
build next. It leaves out what already works (see *What works* in the [README](../README.md)) and
environments with per-environment secrets, which are in review (PR #9).

Each item has a rough **value** (how much it would help running real workflows on kiwicd) and
**effort** (S: a day or less, M: a few days, L: a week or more), and notes on how it fits kiwicd.

## Suggested order

1. Commit statuses / check runs on GitHub
2. A repository's own `pull_request` workflows
3. `schedule` triggers
4. Artifacts (`actions/upload-artifact` / `download-artifact`)
5. Caching (`actions/cache`, `setup-*` caches)
6. Step summaries and annotations in the UI
7. Reusable workflows (`workflow_call`)
8. Job `services:` and `container:`

The first three make kiwicd usable as the CI of a GitHub repository rather than only its
deployer; 4–8 are what common third-party workflows need to run unchanged.

## Triggers

| Feature | Value | Effort | Notes |
|---|---|---|---|
| `pull_request` on the repository itself | High | M | Today only another repository's `repository_pull_request`. The GitHub App already delivers `pull_request` events and the mirror fetches `refs/pull/N/head`. Fork PRs must not get secrets or protected runners (protected refs already give that rule); needs `github.event.pull_request`, `github.head_ref`/`base_ref`, and the merge commit or head as `github.sha`. |
| `pull_request_target` | Medium | S (after the above) | Runs the base branch's workflow with secrets. Easy to misuse; maybe leave out or make opt-in. |
| `schedule` (cron) | High | S–M | Read from the default branch, evaluated in UTC, minimum interval of 5 minutes as on GitHub. Also needed for things like nightly builds and dependency checks. |
| `workflow_run` | Medium | S | Run after another workflow completes (`types: [completed]`, `workflows:`, `branches:`). Common for deploy-after-CI. |
| `repository_dispatch` | Medium | S | `POST /api/repos/OWNER/NAME/dispatches` with `event_type` and `client_payload`, authenticated with an API token (see [API tokens](#access-and-api)). |
| `release`, `create`, `delete` | Low | S | Tag/branch created or deleted; `release` would need releases (see [Repository features](#repository-features)). |
| `merge_group` | Low | M | Only useful with a merge queue. |
| `issues`, `issue_comment`, … | Low | M | GitHub App events, e.g. `/deploy` comment commands. Only worth it for ChatOps. |
| `on.<event>.types` for all events | Medium | S | Activity types filtering, alongside new events. |

## Workflow syntax

| Feature | Value | Effort | Notes |
|---|---|---|---|
| Reusable workflows (`jobs.<id>.uses`, `on: workflow_call`) | High | L | Inputs, secrets (`secrets: inherit`), outputs, nesting limit, local (`./.github/workflows/x.yml`) and remote (`owner/repo/.github/workflows/x.yml@ref`) callers. The scheduler has to expand the called workflow's jobs into the run's plan. |
| Job `services:` | High | M | Databases for tests. act supports them on Docker; kiwicd jobs on host labels and isolated jobs (`--job-image`) would need the service containers on the job's network. |
| Job `container:` on host labels | Medium | M | Same plumbing as services. |
| Job-level `concurrency` | Medium | S | Workflow-level groups exist; extend the same queueing to jobs. |
| Job-level `continue-on-error` and `strategy.max-parallel` | Medium | S | The scheduler handles neither (act covers step-level `continue-on-error`). |
| `jobs.<id>.permissions` / `permissions:` | Low | S | Parse and show; would matter once `KIWICD_TOKEN` can do more than read (see below). |
| `run-name` | Low | S | Run title from an expression. |
| Workflow validation in the UI | Medium | S | Show parse errors for a pushed workflow instead of silently skipping it (GitHub shows a failed "invalid workflow" run). |

## Runs and logs UI

| Feature | Value | Effort | Notes |
|---|---|---|---|
| Step summaries (`GITHUB_STEP_SUMMARY`) | High | S–M | act writes them; upload with the job result and render the Markdown on the run's summary page. |
| Annotations (`::error file=…,line=…::`, `::warning`, `::notice`) | Medium | S–M | List them on the run page; send them to GitHub check runs (below). |
| Log search, line links, download | Medium | S | Permalink to a line (`#step:3:42`), search within a job, download the raw log. |
| Collapsible log groups (`::group::`/`::endgroup::`) | Medium | S | Group headers are styled today but don't fold. |
| Re-run failed jobs only / re-run one job | Medium | M | Keep successful jobs' results and outputs; GitHub's "Re-run failed jobs". |
| Debug logging (`ACTIONS_STEP_DEBUG`, `ACTIONS_RUNNER_DEBUG`) | Low | S | Re-run with debug logging switched on. |
| Filters on the runs list | Medium | S | By workflow, branch, event, status, actor. |
| Workflow status badges | Medium | S | `GET /api/repos/OWNER/NAME/badge/WORKFLOW.svg?branch=…`; public only if the repository is. |
| Job timing / usage | Low | S | Durations per job and step, queue time, totals per workflow. |
| Approvals for deployments | Medium | M | Environment "required reviewers": a job waits until someone approves it. Builds on environments (PR #9). |
| Wait timers on environments | Low | S | Same mechanism as approvals. |

## Artifacts and caching

| Feature | Value | Effort | Notes |
|---|---|---|---|
| Artifacts (`actions/upload-artifact@v4`, `download-artifact@v4`) | High | M–L | The v4 actions talk to the Actions results service (Twirp + blob upload URLs). act has an artifact server for v3 only, so kiwicd would implement the v4 protocol on the server, store artifacts under the data directory with retention, and list/download them on the run page. |
| Cache (`actions/cache@v4`, `setup-node`/`setup-go` caches) | High | M | The cache service v2 protocol (also Twirp), keyed by repository and ref with GitHub's scoping (a branch reads its own and the default branch's caches, so an unprotected branch can't poison the default branch's). act has a cache server that kiwicd could host centrally instead of per agent. |
| Tool cache (`RUNNER_TOOL_CACHE`) | Low | S | Already per agent; document how to keep it between isolated jobs. |

## GitHub integration

| Feature | Value | Effort | Notes |
|---|---|---|---|
| Commit statuses / check runs | High | M | Through the GitHub App (needs the `checks: write` permission): a check run per job, with status, link to the kiwicd run, summary and annotations. Makes kiwicd results show on PRs and usable in branch protection. |
| Re-run from GitHub | Low | S | Handle the `check_run` `rerequested` action. |
| `GITHUB_TOKEN` that can talk to GitHub | Medium | M | An installation token scoped by `permissions:`, for steps that comment on PRs, create releases, etc. Only for protected refs, and opt-in per repository. |
| Deployments API | Low | S | Report environment deployments to GitHub so they show on the PR and repository page. |
| OIDC tokens (`id-token: write`) | Medium | M | kiwicd as an OIDC issuer (`ACTIONS_ID_TOKEN_REQUEST_URL`) so jobs can get cloud credentials without stored secrets. Claims like GitHub's: `repository`, `ref`, `environment`, `sub`. |

## Access and API

| Feature | Value | Effort | Notes |
|---|---|---|---|
| Users and roles | Medium | M | Everyone signed in is admin today. Read-only viewers, per-repository maintainers. |
| Personal API tokens | Medium | S | For scripts and `repository_dispatch`, instead of the admin password. |
| Public read-only view of public repositories' runs | Medium | S–M | Like GitHub's public Actions logs, with secrets already masked. |
| Audit log | Low | S | Who changed secrets, protected refs, readers, runners. |
| Organization-level secrets and variables with repository access lists | Low | S | Between global and per repository. |
| Runner groups | Low | M | Limit runner labels to repositories, like reserved labels but configurable in the UI. |
| Ephemeral / autoscaled runners | Medium | L | Register a runner for one job (`--ephemeral`), and a hook to start VMs or containers when jobs queue. |

## Repository features

kiwicd hosts and mirrors repositories mainly to run workflows; these are only worth it if it's
meant to become more of a forge.

| Feature | Value | Effort | Notes |
|---|---|---|---|
| Browse code and commits | Low | M | File tree and commit list per ref, for hosted repositories. |
| Releases | Low | M | Tags with notes and uploaded assets; would also give the `release` trigger. |
| Packages / container registry | Low | L | Use an existing registry instead. |
| Pages | Low | M | site-deploy covers this. |
| Pull requests, issues, code review | — | — | Out of scope: use GitHub (or Forgejo) and mirror it. |

# kiwicd

kiwicd runs GitHub Actions workflows on your own machines. A Go server hosts (or mirrors) git
repositories, triggers workflows on push, on a schedule or by hand, schedules jobs and shows
live logs in a React UI. Agents connect to it the way self-hosted GitHub runners do and run each
job with [act](https://github.com/nektos/act), through our fork
[dire-kiwi/act](https://github.com/dire-kiwi/act) (the `act/` submodule).

```
 git push ──► kiwicd-server ──► scheduler ──► queued jobs ◄── long-poll ── kiwicd-agent ──► act
              (repos, runs,        (needs, if,                 (register,      (steps on the host
               UI, SSE logs)        matrix, runs-on)            logs, result)   or in Docker)
```

## What works

- **Workflows** from `.github/workflows/*.yml`, parsed with act's model: `on: push` with
  `branches`/`tags`/`paths` filters (and their `-ignore` forms), `on: pull_request` with `types`,
  `branches` and `paths` (see [Pull requests](#pull-requests)), `workflow_dispatch` with inputs,
  `schedule` (see below), `workflow_run`, `repository_dispatch`, `create` and `delete` (see
  [More events](#more-events)), `types` for the events that have activity types, `needs`, job
  `if`, `strategy.matrix` (include/exclude, fail-fast), job `outputs`, `env`, `defaults`,
  `timeout-minutes`, `secrets`, `vars`.
- **Steps** run by act: `run`, local and remote actions (`composite`, `node`, `docker`),
  `docker://` images, `GITHUB_OUTPUT`/`GITHUB_ENV`/`GITHUB_PATH`/`GITHUB_STEP_SUMMARY`, workflow
  commands, `::add-mask::`, `hashFiles()`. `actions/checkout` of the workflow's own repository is
  served from the source archive the agent downloads.
- **Services and job containers**: `jobs.<id>.services` (`image`, `env`, `ports`, `options`,
  `credentials`, `volumes`) and `jobs.<id>.container` on every runner, e.g. a database for tests
  (see [Services](#services)).
- **Repositories**: *hosted* (push to `https://SERVER/git/OWNER/NAME.git`, which triggers
  workflows immediately) or *mirror* (a remote URL fetched every minute or on a webhook,
  `POST /api/hooks/OWNER/NAME` with GitHub's `X-Hub-Signature-256`, or through the
  [GitHub App](#github-app)). A mirror's first fetch only records existing branches and tags;
  later changes trigger runs.
- **Private mirrors**: credentials in an HTTPS URL, or an SSH URL with a per-mirror key. kiwicd
  can generate an ed25519 deploy key (add the public key on GitHub under *Settings → Deploy
  keys*) or take a pasted private key without a passphrase. Each mirror has its own
  `known_hosts`: pasted (`ssh-keyscan` output) or, by default, the host key trusted on the first
  connection and pinned after that. Keys and host keys live under `DATA/ssh/<repo id>/`.
- **Uploaded workflows**: workflows you can't or don't want to commit to a repository can be
  uploaded to kiwicd for it (*Workflows* tab, or
  `curl -u admin -T deploy.yml https://SERVER/api/repos/OWNER/NAME/uploaded-workflows/deploy.yml`;
  `DELETE` removes one). They behave like files in `.github/workflows/` on every branch: pushes
  run them (subject to their own `branches`/`paths` filters), they can be dispatched on any ref,
  and jobs check out the pushed commit. Runs show them as `kiwicd:deploy.yml`, separate from a
  committed file of the same name. To keep a public repository's deployment out of it, prefer a
  private repository that listens with `repository_push`.
- **Reusable workflows**: `jobs.<id>.uses` calls a workflow with `on: workflow_call`, from the
  same repository or another one, with typed `with:` inputs, `secrets:` (or `secrets: inherit`),
  outputs for `needs.<id>.outputs`, and `strategy.matrix`, `needs` and `if` on the calling job,
  nested up to four levels (see [Reusable workflows](#reusable-workflows)).
- **Schedules**: `on: schedule` with `- cron: '30 5 * * 1-5'` entries runs a workflow as on
  GitHub: five POSIX cron fields (minute, hour, day of month, month, day of week; `*/15`, ranges,
  lists and names like `MON` work; `@daily` and the like don't), in UTC or in the entry's
  `timezone` (`timezone: Pacific/Auckland`). Workflows on the default branch, committed or
  uploaded, run at its latest commit with `github.event_name` `schedule` and
  `github.event.schedule` set to the cron string, and get secrets, as runs from the default
  branch do. A schedule runs at most every five minutes, and from when kiwicd first sees it on
  the default branch: pushing it there, uploading it or making its branch the default. A
  restart doesn't run a slot twice; a slot missed while the server was down runs when it starts,
  but after a longer outage only the latest one does. The *Workflows* tab shows each schedule
  and when it next runs.
- **Deploying from another repository**: a workflow with `on: repository_push` runs when
  another repository is pushed to, so a private repository can hold a public one's deployment
  (see [below](#deploy-a-public-repository-from-a-private-one)). One with
  `on: repository_pull_request` runs on another repository's pull requests, from GitHub's
  `pull_request` webhook to its mirror, e.g. for preview deployments (see
  [Pull request previews](#pull-request-previews)).
- **Concurrency groups**: a workflow's top-level `concurrency` (`group`, `cancel-in-progress`;
  expressions can use `github`, `inputs` and `vars`) lets one run of a repository's group run at
  a time, as on GitHub. A new run waits for the group's running one, which it cancels with
  `cancel-in-progress`, and cancels any other that's waiting. A run's page says when it waits.
  `pull_request` runs' groups are apart from other runs': their workflow comes from the pull
  request, which mustn't cancel or hold up a deploy by naming its group.
- **Re-runs**: re-run all of a finished run's jobs, the ones that failed or were cancelled, or
  one job. The last two also re-run the jobs that need them, and the new attempt keeps the other
  jobs' results, outputs and logs (a job that calls a reusable workflow is kept or re-run whole,
  with its called jobs). **Debug logging** is on for a re-run with *Enable debug
  logging* ticked, or with a secret or variable `ACTIONS_STEP_DEBUG` (shows `::debug::` messages)
  or `ACTIONS_RUNNER_DEBUG` (also act's own debug messages) set to `true`; jobs then get
  `RUNNER_DEBUG=1`.
- **Status badges**: `https://SERVER/api/repos/OWNER/NAME/badge/ci.yml.svg` shows how the latest
  finished run of `.github/workflows/ci.yml` on the default branch ended (`?branch=` for
  another, `&event=` for one event; `kiwicd:NAME.yml.svg` for an uploaded workflow). Badges
  need someone signed in who may see the repository, unless the repository's **Settings → Status badges** makes them public, which
  shows its workflows' names and results to anyone; that page also has each badge's Markdown.
- **Protected refs**: only runs from a repository's protected refs get its secrets and the global
  ones. A run's workflow comes from its ref, so otherwise anyone who can push a branch (a
  collaborator, a bot like Dependabot) could add a workflow there that reads every secret. The
  default branch is always protected; add more under the repository's **Settings → Protected
  refs** or with
  `curl -u admin -X PATCH -d '{"protected_refs":["release/*","refs/tags/v*"]}' https://SERVER/api/repos/OWNER/NAME`.
  Patterns are branch names as in a `push` filter (`*` doesn't match `/`, `**` does) or full refs
  (`refs/tags/v*`). Runs from other refs, including dispatches on them and runs on pull requests,
  get only `secrets.KIWICD_TOKEN`, and their page and job logs say so. Whether a run gets secrets is
  decided when it's created (a re-run decides again), and a ref unprotected since then stops
  jobs that haven't started yet from getting them. Such runs' jobs can still use any runner
  whose labels match, and on a runner that keeps state between jobs they could leave something
  behind that reads a later protected job's secrets. Keep runners for protected refs with the
  server's `--protected-label`, or isolate jobs (see [Isolating jobs](#isolating-jobs)).
- **Job tokens**: every job gets `secrets.KIWICD_TOKEN`, which can fetch its repository over
  `/git/` while the job runs (and, for `repository_push`, `repository_pull_request`, scheduled and
  dispatched runs from a protected ref, the repositories its workflow listens to that list it as a
  reader). It can't push or use the API.
- **Agents** register with a one-hour registration token, then use a long-lived runner token.
  `runs-on` labels must all match the agent's labels; every agent also has `self-hosted`,
  the OS (`Linux`) and architecture (`X64`/`ARM64`) labels.
- **GitHub App**: one app, created from *Settings → GitHub*, sends kiwicd the pushes and pull
  requests of every repository it's installed on (no webhook per repository), lets mirrors of
  private ones fetch without a deploy key, shows their runs on GitHub as check runs (usable in
  branch protection, re-run from GitHub), and lets GitHub users, organizations' and teams'
  members sign in (see [GitHub App](#github-app)).
- **Roles and audit log**: admin, maintainer (of all or some repositories) and viewer, given to
  GitHub logins, organizations and teams; API tokens limited to a role and repositories; a log
  of who changed secrets, variables, settings, runners, roles and tokens (see
  [Access](#access)).
- **UI**: runs, live job logs per step with folding `::group::` sections, links to lines
  (`#step:3:42`), search and downloads (a job's log, or a run's as a zip), queue times and
  durations of jobs and steps on a timeline, re-run and cancel, filters on the runs list,
  repositories and workflows, manual dispatch, runners, secrets and variables (global or per
  repository), roles, API tokens and the audit log.
- **Step summaries and annotations**: a run's Summary page shows what each job's steps wrote
  to `$GITHUB_STEP_SUMMARY`, rendered from Markdown, and lists its annotations: `::error`,
  `::warning` and `::notice` workflow commands (with `file`, `line`, `endLine`, `col` and
  `title`) and why a job failed ("Process completed with exit code 1."), each linking to its
  line in the job's log. As on GitHub, a step's summary may be up to 1 MiB (a larger one is
  dropped with a warning), a job keeps up to 20 of them (and 8 MiB in all) and 50 annotations
  from workflow commands; the rest stay in the log. Secrets are masked in both. Summaries are
  sanitized before they're shown, since a workflow can write anything into them: no scripts,
  styles, forms or frames, and images other than `data:` ones are shown as links. They're
  served at `GET /api/runs/{id}/reports`.
- **Command line and MCP**: `kiwicd` lists, starts, watches and cancels runs, prints their logs
  and sets secrets and variables; `kiwicd mcp` gives agents the same as MCP tools (see
  [Command line](#command-line)).

Not yet: artifacts and caches, job-level `concurrency`, `pull_request_target` (it runs the base
branch's workflow with secrets on a pull request's event, which is easy to misuse by checking out
the pull request's code), approval before a first-time contributor's pull request runs (GitHub's
*approve and run*), `merge_group`, `issues`, `issue_comment`, `release`.

## Pull requests

A mirror of a GitHub repository runs its own `on: pull_request` workflows, as GitHub does:

```yaml
# .github/workflows/ci.yml
on:
  pull_request:
    types: [opened, synchronize, reopened]   # the default
    branches: [main]                         # base branches; branches-ignore works too
    paths: ['src/**']                        # files the pull request changes; paths-ignore too
jobs:
  test:
    runs-on: self-hosted
    steps:
      - uses: actions/checkout@v4
      - run: make test
```

- **Events come from GitHub's `pull_request` webhook**, through the [GitHub App](#github-app) or
  a webhook on the repository to the mirror's webhook URL (set up as for
  [Pull request previews](#pull-request-previews)). kiwicd doesn't poll for pull requests.
- **They run on a merge commit**, as on GitHub: for each event kiwicd fetches the pull request's
  head (`refs/pull/N/head`) and its base branch, merges the head into the base branch's current
  tip and keeps the commit at `refs/pull/N/merge` in the mirror. `github.sha` (`GITHUB_SHA`) is
  that merge commit, not the head (which is `github.event.pull_request.head.sha`), and the
  workflow files that run are the merge commit's. kiwicd makes the commit itself rather than
  fetching GitHub's `refs/pull/N/merge`, which GitHub updates in the background and may be stale
  when the event comes. The same head and base give the same commit. As on GitHub, a pull request
  that doesn't merge cleanly runs nothing until its conflicts are resolved. This needs git 2.38
  or later on the server.
- `github.event_name` is `pull_request`, `github.ref` is `refs/pull/N/merge` (`github.ref_name`
  `N/merge`), `github.head_ref`/`GITHUB_HEAD_REF` and `github.base_ref`/`GITHUB_BASE_REF` are the
  head and base branches, and `github.event` is GitHub's `pull_request` event, except that
  `repository` is kiwicd's mirror (as for [previews](#pull-request-previews)). The run's title is
  `#N action: title`.
- **No secrets**: anyone who can open a pull request writes its workflow, so its jobs get only
  `secrets.KIWICD_TOKEN` (which fetches just this repository), whatever the repository's
  protected refs, and [protected](#server-configuration) and reserved runner labels keep them
  off those runners: a job that asks for one fails.
- **Pull requests from forks don't run** unless the repository allows them, in its
  **Settings → Pull requests** or with
  `curl -u admin -X PATCH -d '{"fork_pull_requests":true}' https://SERVER/api/repos/OWNER/NAME`.
  The setting is the repository's, not the workflow's, since the workflow comes from the fork.
  Allowed fork pull requests run like the repository's own: no secrets, no protected runners,
  but the fork's code runs on your runners, so use runners that isolate jobs
  ([Isolating jobs](#isolating-jobs)). There's no approval step for first-time contributors yet.
- `pull_request_target` isn't supported. For a job that needs secrets on a pull request, such as
  a preview deployment, use `repository_pull_request` from another repository (below), which
  runs that repository's default branch.

## Reusable workflows

A job with `uses:` runs the jobs of another workflow, as on GitHub:

```yaml
# .github/workflows/ci.yml
jobs:
  build:
    strategy:
      matrix:
        os: [linux, arm]
    uses: ./.github/workflows/build.yml           # same repository and commit
    with:
      target: ${{ matrix.os }}
    secrets:
      token: ${{ secrets.REGISTRY_TOKEN }}         # or: secrets: inherit
  deploy:
    needs: build
    uses: owner/shared/.github/workflows/deploy.yml@v1
    with:
      image: ${{ needs.build.outputs.image }}
```

```yaml
# .github/workflows/build.yml
on:
  workflow_call:
    inputs:
      target: { type: string, required: true }
      push: { type: boolean, default: true }
    secrets:
      token: { required: true }
    outputs:
      image: { value: "${{ jobs.image.outputs.ref }}" }
jobs:
  image:
    runs-on: self-hosted
    outputs:
      ref: ${{ steps.build.outputs.ref }}
    steps: ...
```

- **Where workflows come from.** `./.github/workflows/x.yml` is read from the same repository and
  commit as the workflow calling it. `owner/repo/.github/workflows/x.yml@ref` (a branch, tag or
  commit) is read from the kiwicd repository `owner/repo`, hosted or mirrored, if it is the run's
  own repository or lists it as a reader in its **Settings → Job access** (as for
  `KIWICD_TOKEN`: the repository being read agrees, here to any run of the listed one). A name
  kiwicd doesn't have is read from GitHub: public repositories without credentials, and private
  ones the [GitHub App](#github-app) is installed on with its installation token, but only for runs
  from protected refs, so that a branch anyone can push can't read them through the app. Mirror a
  repository to use it from runs of other refs.
- **When.** As on GitHub, a run reads every workflow it calls when it's created, down to four
  levels (the run's own workflow and three below it), at most 50 calls; a call that can't be
  read, a loop, an unknown or missing required input or secret fails the run as an invalid
  workflow. The run's page shows each called workflow and the commit it was read at, and a re-run
  uses the same ones.
- **Jobs.** The called workflow's jobs run as part of the run, named `build (linux) / image` as on
  GitHub, and grouped under the calling job on the run's page. The calling job (one per matrix
  combination) finishes with their result and the workflow's `outputs`; with several
  combinations, `needs.build.outputs` has the last one's. `inputs` are converted to their types
  (`string`, `boolean`, `number`) and take their defaults; `github`, `vars` and `github.event`
  are the calling run's, and `actions/checkout` checks out the run's repository, as on GitHub.
  `strategy.fail-fast` and cancelling reach the called jobs.
- **Secrets.** A called workflow's jobs get only what the calling job passes: the `secrets:`
  mapping, evaluated with the caller's secrets, or all of them with `secrets: inherit`, and
  `KIWICD_TOKEN`. Runs from refs that aren't protected still get none, so their mappings are
  empty.
- Agents run called jobs from protocol 2 on: upgrade the server first, then the agents. Older
  agents keep taking other jobs.

## Services

Jobs can have service containers and run in a container of their own, as on GitHub, whatever
runner they're on:

```yaml
jobs:
  test:
    runs-on: self-hosted
    services:
      postgres:
        image: postgres:17
        env:
          POSTGRES_PASSWORD: postgres
        ports: ["5432"]
        options: >-
          --health-cmd pg_isready --health-interval 2s --health-retries 30
    steps:
      - uses: actions/checkout@v4
      - run: go test ./...
        env:
          DATABASE_URL: postgres://postgres:postgres@localhost:${{ job.services.postgres.ports[5432] }}/postgres
```

- **A job on the agent's machine** reaches its services on `localhost`, at the host port Docker
  picked for each of their `ports` (`job.services.<id>.ports[<port>]`). kiwicd publishes them
  on `127.0.0.1` only, unless the workflow names an address (`0.0.0.0:8080:80`). A fixed host
  port (`5432:5432`) works too, but then two jobs using it can't run on the machine at once.
- **A job with `container:`** runs its steps in that container, which reaches its services by
  their IDs (`postgres:5432`) on a network the job's containers share. Without services, a
  container job on the agent's machine uses the machine's network, as act does.
- Steps wait for services' health checks (`--health-cmd`) before they start. The job context
  also has `job.services.<id>.id` and `.network`, and `job.container.id` and `.network`.
- Services and job containers run in Docker, so the agent needs it (`DOCKER_HOST`, or the
  default socket). An agent that can't reach Docker fails such jobs at once and says why; other
  jobs aren't affected.
- Services never get the Docker socket, and job containers get it only from `--docker-socket`
  (see [Isolating jobs](#isolating-jobs)).
- The agent removes a job's service containers, job container and network when the job ends or
  is cancelled, and those of jobs it didn't finish when it starts. They're named
  `kiwicd-r<runner>-j<job>-...`.
- On agents that isolate jobs (`--job-image`), services join the job's own network, and the
  restrictions on container options and volumes apply to them as well. Their ports aren't
  published on the machine: jobs reach them by name, and `job.services.<id>.ports[<port>]` is
  the port itself.

## Deploy a public repository from a private one

A public repository shouldn't carry its deployment: host names, addresses, users, scripts, or
secrets that any branch's workflow could read. Put all of that in a private repository instead,
and have it listen for pushes to the public one:

```yaml
# .github/workflows/deploy.yml in the private repository, e.g. owner/app-deploy
name: Deploy app
on:
  repository_push:
    repositories: [owner/app]   # one or more; a string works for one
    branches: [main]            # also tags, paths and their -ignore forms, as for push
  workflow_dispatch:            # deploy by hand: the app's main branch
jobs:
  deploy:
    runs-on: [self-hosted, deploy]
    steps:
      - uses: actions/checkout@v4   # the private repository: scripts, config
      - name: Clone the app
        env:
          TOKEN: ${{ secrets.KIWICD_TOKEN }}
        run: |
          git -c http.extraHeader="Authorization: Bearer $TOKEN" \
            clone --quiet "$GITHUB_SERVER_URL/git/owner/app.git" app
          git -C app checkout --quiet "${{ github.event.after || 'main' }}"
      - run: ./deploy.sh app
        env:
          DEPLOY_KEY: ${{ secrets.DEPLOY_KEY }}
```

Add both repositories to kiwicd (a mirror of a private GitHub repository needs a deploy key) and
set the secrets and variables on the private one. The public repository needs no workflow, secret
or variable in kiwicd, so nothing pushed to it can reach them. For the job to clone the public
repository, add the private one to its readers, in its **Settings → Job access** or with:

```sh
curl -u admin -X PATCH -d '{"readers":["owner/app-deploy"]}' https://SERVER/api/repos/owner/app
```

- Listeners are read from the private repository's **default branch** when the push arrives, like
  GitHub's `repository_dispatch`. The run belongs to the private repository: `github.repository`,
  `github.sha` and `github.ref` (and `actions/checkout`) are its default branch, and its secrets
  and variables apply, since the default branch is protected.
- `github.event` is the public repository's push event: `github.event.repository.full_name`,
  `github.event.ref`, `github.event.after` (the pushed commit), `github.event.head_commit`. The
  run's title is `owner/app: <commit subject>`, and its page shows the pushed commit.
- `secrets.KIWICD_TOKEN` fetches the repositories the workflow lists under `repository_push` that
  list the listening repository as a reader, in runs started by `repository_push`, a schedule or
  by hand. The repository being read has to agree, since any repository's default branch can
  listen to any other, including a public one's where outside contributions land. Runs started by
  a push to the listening repository only get that repository, since their workflow can come from
  any branch, and so do dispatches on refs that aren't protected.
- GitHub doesn't know `repository_push`, so turn off GitHub Actions on the private repository if
  it's on GitHub, or GitHub reports the workflow as invalid.

### Pull request previews

The private repository can also run on the public one's pull requests, to deploy a preview of
each and remove it when the pull request closes:

```yaml
# .github/workflows/previews.yml in owner/app-deploy
name: Previews
on:
  repository_pull_request:
    repositories: [owner/app]
    types: [opened, synchronize, reopened, closed]   # the default leaves out closed
    branches: [main]                                 # base branches; branches-ignore works too
concurrency:
  group: pr-${{ github.event.number }}
  cancel-in-progress: true
jobs:
  deploy:
    if: github.event.action != 'closed'
    runs-on: [self-hosted, deploy]
    steps:
      - uses: actions/checkout@v4
      - name: Fetch the pull request
        env:
          TOKEN: ${{ secrets.KIWICD_TOKEN }}
          PR: ${{ github.event.number }}
          SHA: ${{ github.event.pull_request.head.sha }}
        run: |
          git init --quiet app
          git -C app -c http.extraHeader="Authorization: Bearer $TOKEN" \
            fetch --quiet "$GITHUB_SERVER_URL/git/owner/app.git" "refs/pull/$PR/head"
          git -C app checkout --quiet "$SHA"
      - run: ./deploy-preview.sh app "pr${{ github.event.number }}"
  remove:
    if: github.event.action == 'closed'
    runs-on: [self-hosted, deploy]
    steps:
      - uses: actions/checkout@v4
      - run: ./remove-preview.sh "pr${{ github.event.number }}"
```

- **Events come from GitHub's webhook.** The public repository has to be a mirror in kiwicd, and
  either the [GitHub App](#github-app) is installed on it, or on GitHub it needs a webhook
  (*Settings → Webhooks*) to the mirror's webhook URL (shown in its **Settings**,
  `https://SERVER/api/hooks/owner/app`), content type `application/json`, the mirror's webhook
  secret, and the *Pull requests* event (add *Pushes* too and pushes don't wait for the next
  poll). Not both, or each pull request event starts its runs twice. kiwicd has to be reachable
  from GitHub. kiwicd doesn't poll GitHub for pull
  requests: a git mirror can't tell a fork's pull request from the repository's own, and GitHub
  keeps `refs/pull/N/head` after a pull request closes. If kiwicd misses an event (it was down,
  say), redeliver it from the webhook's *Recent Deliveries* on GitHub.
- For each event the mirror fetches `refs/pull/N/head`, so jobs can fetch it from kiwicd with
  `KIWICD_TOKEN` (with the same reader opt-in as for pushes), and then starts the listeners from
  their repositories' default branches, with their secrets, as for `repository_push`. Events are
  handled one at a time in the order they arrive.
- `github.event` is GitHub's `pull_request` event: `action`, `number`, `before`/`after` (for
  `synchronize`), `sender` and `pull_request` (`head.sha`, `head.ref`, `head.repo.full_name`,
  `base.ref`, `merged`, `title`, ...), except that `repository` is kiwicd's mirror, as for
  `repository_push`: `repository.full_name` is `owner/app` and `repository.clone_url` is on kiwicd.
  The run's title is `owner/app #N action: title`, and its page shows the pull request's head.
- `types` defaults to `opened`, `synchronize` and `reopened`, as GitHub's `pull_request` does;
  any action GitHub sends can be listed. `paths` filters aren't supported.
- **Pull requests from forks don't start runs** unless the trigger has `forks: true`. Anyone can
  open one on a public repository, and the run gets the private repository's secrets: with
  `forks: true` the workflow decides what to do with the fork's code, and must not build or run
  it in a job that has secrets or deploy credentials. A pull request is from a fork when its
  `github.event.pull_request.head.repo.full_name` differs from `base.repo.full_name` (`head.repo`
  is null once the fork is deleted).
- **Ordering:** two quick pushes to a pull request, or a push and then a close, start runs that
  could overlap, and a slow deploy could bring back a preview the close removed. Give the workflow
  a `concurrency` group per pull request as above: a new run waits for the group's running one,
  and `cancel-in-progress` stops it rather than waiting for it to finish. Since a cancelled job can
  stop part way through a deploy, the close's removal should cope with a half-deployed preview.

## More events

As on GitHub, a workflow can also run after another one, on request, or when a branch or tag is
created or deleted.

### After another workflow: `workflow_run`

```yaml
# .github/workflows/deploy.yml
name: Deploy
on:
  workflow_run:
    workflows: [CI]               # names of this repository's workflows (their file without a name)
    types: [completed]            # requested, in_progress, completed; all by default
    branches: [main]              # the CI run's branch; branches-ignore works too
jobs:
  deploy:
    if: github.event.workflow_run.conclusion == 'success'
    runs-on: [self-hosted, deploy]
    steps:
      - uses: actions/checkout@v4
        with:
          ref: ${{ github.event.workflow_run.head_sha }}
      - run: ./deploy.sh
```

- The workflow runs from the repository's **default branch** (its file there, at the tip), not
  from the run it follows. `github.event.workflow_run` has the run's `id`, `name`, `run_number`,
  `run_attempt`, `event`, `status`, `conclusion` (with `completed`: `success`, `failure`,
  `cancelled`), `head_branch`, `head_sha`, `path` and `html_url`; `github.event.action` is the
  activity type. For a [pull request](#pull-requests)'s run, `head_branch` is the pull request's
  head branch, which a fork can call `main` too. Its runs page links to the run it followed.
- **It gets the default branch's secrets, whatever ref the run it follows was for**, as on
  GitHub. That's what makes deploy-after-CI work, and also what makes it risky: a CI run on a
  branch that anyone can push to (a collaborator's, Dependabot's) or on a pull request runs code
  you haven't reviewed,
  and anything it leaves behind or reports (job outputs, files on a runner that keeps state,
  values it pushed somewhere) is under that branch's control. Treat what comes from the run you
  follow as untrusted input: check `head_branch` and `event`, build from `head_sha` yourself rather
  than reusing what the run built, and don't pass its values to a shell unquoted. Workflows are
  matched by name, and a workflow added on any branch can be called `CI`: keep `branches` to the
  protected ones for anything that deploys.
- Runs that follow each other stop after three levels, as on GitHub: in A → B → C → D → E, where
  each follows the one before with `workflow_run`, E doesn't run (the server logs that it
  didn't). A workflow that follows itself is caught the same way.
- Every run of the repository counts, whatever started it: pushes, dispatches, re-runs,
  `repository_push` and the events below.

### On request: `repository_dispatch`

```yaml
on:
  repository_dispatch:
    types: [deploy]               # event types; any by default
```

```sh
curl -u admin -X POST -d '{"event_type":"deploy","client_payload":{"env":"prod"}}' \
  https://SERVER/api/repos/OWNER/NAME/dispatches
kiwicd repo dispatch OWNER/NAME deploy --payload '{"env":"prod"}'
```

The request is GitHub's *create a repository dispatch event*, authenticated like the rest of the
API (an [API token](#command-line) works). It starts the default branch's workflows whose `types`
include `event_type` (1 to 100 characters) and returns their runs, `{"run_ids": [...]}`, which may
be none. `github.event.action` is the event type and `github.event.client_payload` the payload
(a JSON object). The [GitHub App](#github-app) also takes GitHub's `repository_dispatch`
webhook, for mirrors of the repositories it's installed on: the mirror is fetched first, so the
run is of its default branch's latest commit. GitHub only sends that webhook to apps, for
dispatches that a GitHub App made on GitHub (not ones made with a personal token).

### Branches and tags: `create` and `delete`

`on: create` runs when a push or a mirror's fetch creates a branch or tag, from the workflow file
in its commit and at its ref, like a push to it (so an unprotected branch gets no secrets).
`on: delete` runs when one is deleted, from the default branch. Neither takes filters, as on
GitHub; check `github.event.ref_type` (`branch` or `tag`) and `github.event.ref` (the short name)
in the job's `if`. A mirror's first fetch doesn't create anything.

## GitHub App

kiwicd can have one GitHub App. Create it under **Settings → GitHub**: pick your account or an
organization, and kiwicd sends you to GitHub with a manifest, where you confirm; GitHub sends you
back and kiwicd stores the app's credentials (encrypted, like secrets). Set `--public-url` first:
the app's webhook and sign-in URLs are made from it, and GitHub has to reach the webhook. The app
is private, so it can only be installed on the account that owns it; create it in the
organization whose repositories you mirror. An existing app can be entered by hand instead
(*Use an existing app*), or with `PUT /api/github/app` (`app_id`, `client_id`, `client_secret`,
`private_key`, `webhook_secret`).

Then **install the app** on the repositories kiwicd mirrors (*Install on repositories*). For
each of them:

- **Webhooks**: GitHub sends their `push` and `pull_request` events to
  `https://SERVER/api/github/webhook`, and kiwicd hands each to the mirrors of that repository
  (mirrors whose URL is it on GitHub, in HTTPS, SSH or `git@github.com:` form), as if it had come
  to the mirror's own webhook URL: pushes sync the mirror and pull requests start its
  `pull_request` workflows and `repository_pull_request` listeners. `repository_dispatch` events
  (GitHub sends them for dispatches made by a GitHub App) start the mirror's
  `repository_dispatch` workflows. Remove any webhook you added to the repository for kiwicd,
  or its pull requests start runs twice. Events for repositories without a mirror are ignored.
- **Fetching**: a mirror whose URL is `https://github.com/OWNER/NAME.git` (without credentials)
  fetches with a short-lived installation token, limited to reading that repository, so a private
  one needs no deploy key. The new-mirror form lists the repositories the app is installed on.

- **Check runs**: each job of a mirror's runs shows on the commit (and its pull requests) as a
  check run named `workflow / job`, with matrix values as in the job's name, e.g.
  `CI / test (1.23, linux)`. It's created when the job is queued and updated when it starts and
  finishes, with the conclusion, a short summary (duration, error, steps) and a link to the job
  in kiwicd (made from `--public-url`). A finished job's check run also carries its step
  summaries (cut to GitHub's limit) and its first 50 annotations, which GitHub shows on the
  pull request's files; ones without a file go on `.github`, as GitHub Actions puts them. Push and dispatched runs report on their commit, pull
  request runs on the pull request's head. A workflow file that doesn't parse gets one failed
  check run. Branch protection can require these checks by name. **Re-run** on GitHub (one
  check, or all of a commit's) starts a new attempt of the runs they belong to, unless the
  latest attempt is still going; anyone who can re-run checks there can do that. Talking to
  GitHub happens in the background and never fails or holds up a run: errors are logged and
  retried a few times, and a later change of the run catches up.

  Runs of `repository_push` and `repository_pull_request` listeners belong to the listening
  repository, often a private one holding a public one's deployment, and uploaded workflows are
  kept out of the repository for the same reason; reporting them would show their workflow and
  job names and links to kiwicd to everyone who can see the repository on GitHub. So they're
  reported only if the listening (or, for uploaded workflows, the mirror's) repository's
  **Settings → GitHub checks** is on (or
  `curl -u admin -X PATCH -d '{"report_checks":true}' https://SERVER/api/repos/OWNER/NAME`).
  Listeners' check runs go on the source repository's pushed commit or pull request head, named
  `OWNER/NAME: workflow / job` after the listener.

The app also lets runs from protected refs call [reusable workflows](#reusable-workflows) in
private repositories it's installed on.

The app asks for read access to contents, pull requests and organization members, write access
to checks, and for the push, pull request, check run and check suite events.

**Apps made before kiwicd reported check runs** lack the checks permission; *Settings → GitHub*
says so, and which installations haven't accepted it. On GitHub, open the app's settings
(*Settings → Developer settings → GitHub Apps*, the organization's for an organization's app),
then *Permissions & events*: set *Repository permissions → Checks* to *Read and write*, tick the
*Check run* and *Check suite* events, and save. GitHub then asks each installation's owner to
accept the new permissions (*Settings → Applications → Installed GitHub Apps*, or the
organization's, *Configure*, and *Review request*); until they do, runs there aren't reported
and kiwicd only logs a warning now and then.

### Sign in with GitHub

Once the app is set up, the sign-in page offers *Sign in with GitHub*, through the app's OAuth
client. People may sign in if they have a [role](#access): their login has one, or an
organization or team they're a member of does (checked through the app, which has to be
installed on the organization). The app's owner (the user, or the organization's members) is
made an admin when the app is created. Runs they start by hand show their login. Membership is
checked when someone signs in, and GitHub sessions last a week. The admin password keeps
working, for the API and git too.

## Access

Everyone signed in has a role:

| Role | May |
| --- | --- |
| **viewer** | see repositories, workflows, runs and their logs, and runners; fetch repositories over `/git/` |
| **maintainer** of every repository, or of some (`owner/name` or `owner/*`) | also, for those repositories: secrets and variables, protected refs, readers, mirrors' default branch and SSH settings (not their URL, nor letting forks' pull requests run, which only admins do), uploaded workflows, syncing, starting, re-running and cancelling runs, and pushing to hosted ones |
| **admin** | everything, including global secrets and variables, adding and removing repositories, runners, the GitHub App, roles, everyone's API tokens and the audit log |

The admin password is always an admin. Roles are given under **Settings → GitHub** (or
`PUT /api/roles` with `{"kind": "user"|"org"|"team", "name": "LOGIN"|"ORG"|"ORG/TEAM-SLUG",
"role": ..., "repos": [...]}`, and `DELETE /api/roles?kind=&name=`) to GitHub logins, to an
organization's members and to a team's; someone matched by several gets the most they give, so
an organization can be viewers and one team maintainers. Changing a role applies at once, to
sessions and tokens too; people left without one are signed out and their API tokens revoked.
Organization and team membership is only checked when someone signs in, so a new organization
or team role reaches its members when they next sign in. Under it, *People who have signed in*
shows everyone's role now.

The server enforces roles on every API route and on git: what someone can't use is refused
(`403`), and repositories a token can't see don't exist (`404`). The UI, `kiwicd` and
`kiwicd mcp` leave out what the user or token can't use; `GET /api/me` says what that is
(`access`).

**API tokens** act as whoever made them, limited by their scope: a role no higher than the
maker's (`viewer` makes a read-only token) and, optionally, some repositories, which are then
the only ones the token sees and of which it is at most a maintainer, for a system that starts
runs or an agent's `kiwicd mcp`. A token never has more than its maker has now, so lowering
someone's role lowers their tokens'. Tokens without a scope, including all made before scopes
existed, have their maker's full rights. Tokens with a scope can't make or revoke other tokens
(except themselves, for `kiwicd logout`). Everyone sees and revokes their own tokens; admins
see everyone's.

**The audit log** (*Audit* in the menu, or `GET /api/audit?repo=&before=&limit=`) records who
changed secrets and variables (never their values), repositories (created, deleted, protected
refs, readers, mirror URL, default branch, SSH key and known hosts), uploaded workflows,
runners (registered, removed, registration tokens made), roles, API tokens and the GitHub App,
with the API token and client address used. Changes kiwicd makes itself, such as revoking
tokens after the admin password changes, show as `kiwicd`. Entries are kept for good.

Upgrading from before roles turns the GitHub sign-in allow list into admin roles, as it gave
everyone the admin's access. People allowed through an organization keep their sessions if
there was only one organization; otherwise they sign in again.

## Command line

`kiwicd` (`go build -o bin/ ./cmd/kiwicd`) uses the API with an API token. `kiwicd login` asks
for the admin password, makes a token with it and saves the server and token in
`~/.config/kiwicd/cli.json` (mode 0600); the password itself isn't kept. People who sign in
with GitHub make a token under **Settings → API tokens** and pass it with `--token`.
`--role` and `--repo` (with the password) limit the token login makes; `kiwicd whoami` shows
your role.
`KIWICD_URL` and `KIWICD_API_TOKEN` override the saved ones, e.g. in scripts.

```sh
kiwicd login --url https://SERVER
kiwicd run list --repo owner/app             # kiwicd runs, for short
kiwicd run list --workflow ci.yml --branch main --status failure
kiwicd run view 42                           # jobs and steps
kiwicd run logs 42 --failed                  # or --job build, --follow
kiwicd run start owner/app deploy.yml --ref main -i target=prod --watch
kiwicd run cancel 42; kiwicd run rerun 42 --watch
kiwicd run rerun 42 --failed                 # or --job build: keeps the other jobs' results
kiwicd workflow list owner/app
kiwicd repo dispatch owner/app deploy --payload '{"env":"prod"}'   # a repository_dispatch event
echo -n "$VALUE" | kiwicd secret set DEPLOY_KEY --repo owner/app   # or type it at the prompt
kiwicd secret list; kiwicd variable set REGION nz-north --repo owner/app
kiwicd api GET repos/owner/app               # any other API call
```

Secrets and variables without `--repo` are the global ones. `--watch` exits non-zero unless the
run succeeds, and list and view commands take `--json`. `kiwicd help` lists every command.

API tokens act as whoever made them, limited by their [scope](#access), as a `Bearer` token or
a Basic password, for the API and git over `/git/`. The admin's can last forever; those of people signed in with GitHub last at
most 90 days, since organization membership is only checked when they sign in. Changing the
admin password revokes the admin's tokens, as it does their sessions. Tokens are listed and
revoked under **Settings → API tokens**, or with `kiwicd token list` and `kiwicd token revoke`;
`kiwicd logout` revokes its own. `kiwicd token create NAME --role viewer --repo owner/app` makes
a read-only token that sees only `owner/app`.

### MCP

`kiwicd mcp` is an MCP server on stdio, with the saved login, for agents such as Claude Code:

```sh
claude mcp add kiwicd -- kiwicd mcp              # or: kiwicd mcp --read-only
```

Its tools list repositories, workflows, runs and runners, get a run with its jobs and steps,
read the end of a job's log (or one step's), wait for a run to finish, start, re-run and cancel
runs, send `repository_dispatch` events, and list, set and delete secrets and variables. Secrets
stay write-only: the agent can set them but not read them back. `--read-only` leaves out the tools that change anything, as does a
viewer's or read-only token; the server says in its instructions what the token may do. Give it
its own token (`KIWICD_API_TOKEN`), perhaps limited to a few repositories, to revoke it
separately from yours.

## Layout

| Path | |
|---|---|
| `cmd/kiwicd-server`, `internal/server` | HTTP API, git hosting, scheduler, SQLite store |
| `cmd/kiwicd-agent`, `internal/agent` | Registration, polling, running jobs with act, log reporting |
| `cmd/kiwicd`, `internal/cli` | Command line client and MCP server |
| `internal/protocol` | Types shared by server and agent |
| `internal/wf` | Job-level expression evaluation and trigger filters on top of act |
| `web/` | React UI (Vite), embedded into the server binary from `web/dist` |
| `act/` | Submodule: `dire-kiwi/act`, branch `kiwicd` |

## Develop

```sh
git submodule update --init
(cd web && npm ci && npm run build)      # optional; the server runs without a UI
go build -o bin/ ./cmd/...
KIWICD_ADMIN_PASSWORD=pw bin/kiwicd-server --data ./data --listen 127.0.0.1:8080
```

In another terminal, create a registration token on the Runners page (or
`curl -u admin:pw -X POST localhost:8080/api/runners/registration-token`) and start an agent:

```sh
bin/kiwicd-agent register --url http://127.0.0.1:8080 --token TOKEN --name dev --labels deploy
bin/kiwicd-agent run
```

Create a hosted repository in the UI, add it as a remote and push:

```sh
git remote add kiwicd http://admin:pw@127.0.0.1:8080/git/me/project.git
git push kiwicd main
```

`cd web && npm run dev` serves the UI with hot reload and proxies `/api` to port 18080.

Tests: `go test ./internal/...`.

### Agent labels

`--labels` takes act_runner-style specs: `name` or `name:host` runs jobs on the agent's machine;
`name:docker://IMAGE` runs them in a container (needs Docker). Remote actions are cloned from
`https://github.com` (`--actions-url`).

### Isolating jobs

Jobs on the agent's machine run as the agent's user: they can read its registration (and with
it take other jobs and their secrets), leave processes behind that read later jobs' secrets, and
change what later jobs use (`~/.gitconfig`, Go and npm caches, cloned actions). Only run trusted
code that way. For anything else, start the agent with `--job-image IMAGE`
(`KIWICD_JOB_IMAGE`): each job then runs in a new container of that image, removed afterwards
along with any process it left, and none of the agent's files are in it. Labels without a
`docker://` image of their own, and GitHub's implicit ones (`self-hosted`, `linux`, ...), use
the job image. The agent needs Docker; its jobs don't get it. Also:

- Each job has a bridge network of its own, so jobs can't reach each other's containers. Its
  services, `docker://` steps and the builds of its Docker actions use it too. The network's
  interface is `--job-bridge-prefix` (default `kiwicd-`, at most 7 characters) plus the job ID,
  so a firewall can match jobs' traffic, e.g. `iptables -I DOCKER-USER -i kiwicd-+ -d
  10.0.0.0/8 -j DROP`.
- Workflows may not give their `container` or `services` options or volumes that reach outside
  the container: host paths, named volumes (they outlive the job), `--privileged`, devices,
  capabilities, other namespaces, `--env-file`. Options such as `--health-cmd`, `--user`,
  `-e`, `--memory` and `--tmpfs` work as usual. Service ports aren't published on the machine
  (see [Services](#services)).
- act's shared tool cache volume is left out and Docker actions are rebuilt for each job.
- `--job-options` (`KIWICD_JOB_OPTIONS`) are `docker create` options for every job, service and
  step container, after the workflow's own, e.g. `--memory 4g --cpus 2 --pids-limit 4096`.
- The agent removes each job's containers, volumes, networks and Docker action images when the
  job ends, and those of jobs it didn't finish when it starts. They're named
  `kiwicd-r<runner>-j<job>-...`.

Container jobs don't get the Docker socket unless the agent is given one with `--docker-socket
/var/run/docker.sock` (`KIWICD_DOCKER_SOCKET`), which hands its jobs the machine. Give it only to
a runner whose label is reserved for trusted repositories (see `--reserve-label`).

Runners that don't isolate jobs, or that hold anything a job from an unprotected ref shouldn't
reach, can be kept for jobs from protected refs: start the server with `--protected-label
LABEL` and give those runners the label. Jobs from other refs that ask for the label fail at
once, saying why; those that don't ask for it wait for another runner, and the job's page says
which runners won't take it. Every runner has `self-hosted`, so `--protected-label self-hosted`
keeps them all for protected refs and jobs from other refs don't run at all.

### Server configuration

| Flag / variable | |
|---|---|
| `KIWICD_ADMIN_PASSWORD` | Required. UI login (user `admin`), git push and API basic auth, as an admin. People can also [sign in with GitHub](#sign-in-with-github), with a [role](#access) |
| `--data` / `KIWICD_DATA` | Database and repositories |
| `--public-url` / `KIWICD_PUBLIC_URL` | URL used in clone URLs, `github.server_url` and links |
| `--retention-days` / `KIWICD_RETENTION_DAYS` | Days to keep completed runs and their logs (default 30, 0 keeps them forever); also passed to jobs as `GITHUB_RETENTION_DAYS` |
| `--client-ip-header` / `KIWICD_CLIENT_IP_HEADER` | Behind a reverse proxy: the header it puts the client's address in (`X-Forwarded-For`, `CF-Connecting-IP`), so wrong admin passwords are limited per client (10, then one a minute) rather than per proxy. Only set it if every request comes through that proxy, since clients can send the header themselves |
| `KIWICD_REGISTRATION_TOKEN` | Optional long-lived registration token, for agents deployed next to the server |
| `--reserve-label` / `KIWICD_RESERVED_LABELS` | Keep a runner label for some repositories and refs: `LABEL=OWNER/REPO[@REF][,...]`, repeatable (the variable holds them separated by spaces). A bare `OWNER/REPO` means its default branch; `REF` is a branch, a full ref (`refs/tags/v*`) or a pattern (`@*` for any branch). A runner with a reserved label takes only jobs of runs those allow, whatever labels the jobs ask for, and jobs elsewhere that ask for the label fail at once. For runners that hold deploy credentials, reach production or have the Docker socket |
| `--protected-label` / `KIWICD_PROTECTED_LABELS` | Runners with this label take only jobs from protected refs, of any repository, whatever labels the jobs ask for; jobs from other refs that ask for it fail at once. Repeatable (the variable holds them separated by spaces). None by default |
| `--max-log-lines` / `KIWICD_MAX_LOG_LINES` | Most log lines stored per job (default 1000000, `-1` for no limit). Later output is dropped after a notice; the job keeps running |
| `--max-log-bytes` / `KIWICD_MAX_LOG_BYTES` | Most log bytes stored per job (default 104857600). Lines longer than 64 KiB are cut |
| `--github-url` / `KIWICD_GITHUB_URL` | GitHub's web URL, for the GitHub App and signing in (default `https://github.com`; GitHub Enterprise: `https://HOST`) |
| `--github-api-url` / `KIWICD_GITHUB_API_URL` | GitHub's API URL (default `https://api.github.com`; GitHub Enterprise: `https://HOST/api/v3`) |
| `KIWICD_SECRET_KEY` | Key that encrypts secrets and SSH deploy keys in the database: 32 bytes as base64 or hex (`openssl rand -base64 32`). Default: `<data>/secret.key`, generated on first start |

#### The secret key

Secret values, mirror deploy keys and the GitHub App's credentials are stored encrypted (AES-256-GCM). The key is
`KIWICD_SECRET_KEY` or, if that's unset, `<data>/secret.key` (mode 0600), which the server
generates on first start. **Back the key up separately from the database**: a backup of the
database alone doesn't reveal any secrets, and without the key it can't be decrypted.
`secret.key` sits in the data directory, so back up the data directory without it, or set
`KIWICD_SECRET_KEY` and keep the key somewhere else.

The server won't start with a key that doesn't match the database, and it won't generate a new
key when `secret.key` is missing but the database already has encrypted values. Restore the
original key instead. To switch from `secret.key` to `KIWICD_SECRET_KEY`, set the variable to the
file's contents. On upgrade from a release that stored them in plain text, existing secrets and
deploy keys are encrypted at startup.

## The act fork

Changes kiwicd needs from act go on the `kiwicd` branch of `dire-kiwi/act`; `upstream` is
nektos/act. So far:

- `Config.ActionsURL`: clone remote actions from GitHub even though `GITHUB_SERVER_URL` points at
  kiwicd.
- `docker://` steps of host jobs use the default network and the workspace's container path, as
  Docker actions already did (they tried to join a job container that doesn't exist).
- For isolated jobs: `Config.ContainerNamePrefix` (unique names for a job's containers,
  volumes and action images), `ToolCacheVolume`, `RestrictContainers` (refuse workflow container
  options and volumes that reach the host), `ContainerOptions` applied after the workflow's own to
  job and service containers and to `docker://` steps, and a user-defined `ContainerNetworkMode`
  used for services and for building Docker actions too.
- `Config.WorkflowCallInputs`: the `inputs` of a reusable workflow's job, which the server
  evaluates from the calling job's `with:`, since kiwicd runs called jobs itself rather than
  through act's own reusable workflow support.
- Services for host jobs: on a network of their own, with their ports published on the host
  (on `Config.ServicePortHostIP` unless the workflow names an address; not at all under
  `RestrictContainers`). `job.services.<id>.id`, `.network` and `.ports`, and `job.container.id`
  and `.network`, are filled in, and a number indexes an object (`ports[5432]`). Services don't
  get the Docker socket, and one service's registry credentials no longer replace the job
  container's.

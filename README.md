# kiwicd

kiwicd runs GitHub Actions workflows on your own machines. A Go server hosts (or mirrors) git
repositories, triggers workflows on push or by hand, schedules jobs and shows live logs in a
React UI. Agents connect to it the way self-hosted GitHub runners do and run each job with
[act](https://github.com/nektos/act), through our fork
[dire-kiwi/act](https://github.com/dire-kiwi/act) (the `act/` submodule).

```
 git push ──► kiwicd-server ──► scheduler ──► queued jobs ◄── long-poll ── kiwicd-agent ──► act
              (repos, runs,        (needs, if,                 (register,      (steps on the host
               UI, SSE logs)        matrix, runs-on)            logs, result)   or in Docker)
```

## What works

- **Workflows** from `.github/workflows/*.yml`, parsed with act's model: `on: push` with
  `branches`/`tags`/`paths` filters (and their `-ignore` forms), `workflow_dispatch` with inputs,
  `needs`, job `if`, `strategy.matrix` (include/exclude, fail-fast, `max-parallel`), job
  `outputs`, `env`, `defaults`, `timeout-minutes`, `secrets`, `vars`, `run-name` (the run's
  title, from `github`, `inputs` and `vars`).
- **Job `continue-on-error`**, also from an expression such as `${{ matrix.experimental }}`: the
  job still shows as failed, but its failure doesn't fail the run or cancel the rest of its
  matrix, and jobs that need it see it as successful, as on GitHub.
- **`permissions:`** (workflow and job) are checked, stored and shown on the run and job pages,
  and sent to agents with each job, but have no effect yet: `KIWICD_TOKEN` can only read.
- **Invalid workflows**: a workflow that fails to parse or validate gets a failed run showing
  the error, as on GitHub, when its triggers would have run it (or can't be read at all). That
  includes uploaded workflows, which are also checked on upload, and `repository_push`
  listeners. Errors found when the run starts (a `concurrency` group or `run-name` that can't be
  evaluated) fail the run the same way.
- **Steps** run by act: `run`, local and remote actions (`composite`, `node`, `docker`),
  `docker://` images, `GITHUB_OUTPUT`/`GITHUB_ENV`/`GITHUB_PATH`/`GITHUB_STEP_SUMMARY`, workflow
  commands, `::add-mask::`, `hashFiles()`. `actions/checkout` of the workflow's own repository is
  served from the source archive the agent downloads.
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
  A job's `concurrency` (`jobs.<id>.concurrency`; expressions can also use `needs` and
  `matrix`) does the same for the repository's jobs: a job waits as *pending* while another of
  its group is queued or running. Job groups are separate from workflow groups, unlike on
  GitHub, so a job doesn't wait for a run whose workflow group has the same name.
- **Protected refs**: only runs from a repository's protected refs get its secrets and the global
  ones. A run's workflow comes from its ref, so otherwise anyone who can push a branch (a
  collaborator, a bot like Dependabot) could add a workflow there that reads every secret. The
  default branch is always protected; add more under the repository's **Settings → Protected
  refs** or with
  `curl -u admin -X PATCH -d '{"protected_refs":["release/*","refs/tags/v*"]}' https://SERVER/api/repos/OWNER/NAME`.
  Patterns are branch names as in a `push` filter (`*` doesn't match `/`, `**` does) or full refs
  (`refs/tags/v*`). Runs from other refs, including dispatches on them, get only
  `secrets.KIWICD_TOKEN`, and their page and job logs say so. Whether a run gets secrets is
  decided when it's created (a re-run decides again), and a ref unprotected since then stops
  jobs that haven't started yet from getting them. Such runs' jobs can still use any runner
  whose labels match, and on a runner that keeps state between jobs they could leave something
  behind that reads a later protected job's secrets. Keep runners for protected refs with the
  server's `--protected-label`, or isolate jobs (see [Isolating jobs](#isolating-jobs)).
- **Job tokens**: every job gets `secrets.KIWICD_TOKEN`, which can fetch its repository over
  `/git/` while the job runs (and, for `repository_push`, `repository_pull_request` and dispatched
  runs from a protected ref, the repositories its workflow listens to that list it as a reader). It
  can't push or use the API.
- **Agents** register with a one-hour registration token, then use a long-lived runner token.
  `runs-on` labels must all match the agent's labels; every agent also has `self-hosted`,
  the OS (`Linux`) and architecture (`X64`/`ARM64`) labels.
- **GitHub App**: one app, created from *Settings → GitHub*, sends kiwicd the pushes and pull
  requests of every repository it's installed on (no webhook per repository), lets mirrors of
  private ones fetch without a deploy key, and lets allowed GitHub users or organizations'
  members sign in (see [GitHub App](#github-app)).
- **UI**: runs, live job logs per step, re-run and cancel, repositories and workflows, manual
  dispatch, runners, secrets and variables (global or per repository), API tokens.
- **Command line and MCP**: `kiwicd` lists, starts, watches and cancels runs, prints their logs
  and sets secrets and variables; `kiwicd mcp` gives agents the same as MCP tools (see
  [Command line](#command-line)).

Not yet: reusable workflows (`jobs.<id>.uses`), job `container:`/`services:` on host labels,
artifacts and caches, a repository's own `pull_request` workflows
(only other repositories' `repository_pull_request`), commit statuses on GitHub.

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
  list the listening repository as a reader, in runs started by `repository_push` or by hand. The
  repository being read has to agree, since any repository's default branch can listen to any
  other, including a public one's where outside contributions land. Runs started by a push to the
  listening repository only get that repository, since their workflow can come from any branch,
  and so do dispatches on refs that aren't protected.
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

## GitHub App

kiwicd can have one GitHub App. Create it under **Settings → GitHub**: pick your account or an
organization, and kiwicd sends you to GitHub with a manifest, where you confirm; GitHub sends you
back and kiwicd stores the app's credentials (encrypted, like secrets). Set `--public-url` first:
the app's webhook and sign-in URLs are made from it, and GitHub has to reach the webhook. The app
is private, so it can only be installed on the account that owns it; create it in the
organization whose repositories you mirror. An existing app can be entered by hand instead
(*Use an existing app*), or with `PUT /api/github/app` (`app_id`, `client_id`, `client_secret`,
`private_key`, `webhook_secret`, `allowed_users`, `allowed_orgs`).

Then **install the app** on the repositories kiwicd mirrors (*Install on repositories*). For
each of them:

- **Webhooks**: GitHub sends their `push` and `pull_request` events to
  `https://SERVER/api/github/webhook`, and kiwicd hands each to the mirrors of that repository
  (mirrors whose URL is it on GitHub, in HTTPS, SSH or `git@github.com:` form), as if it had come
  to the mirror's own webhook URL: pushes sync the mirror and pull requests start
  `repository_pull_request` listeners. Remove any webhook you added to the repository for kiwicd,
  or its pull requests start runs twice. Events for repositories without a mirror are ignored.
- **Fetching**: a mirror whose URL is `https://github.com/OWNER/NAME.git` (without credentials)
  fetches with a short-lived installation token, limited to reading that repository, so a private
  one needs no deploy key. The new-mirror form lists the repositories the app is installed on.

The app asks for read access to contents, pull requests and organization members, and for the
push and pull request events.

### Sign in with GitHub

Once the app is set up, the sign-in page offers *Sign in with GitHub*, through the app's OAuth
client. Who may sign in is set under **Settings → GitHub**: GitHub logins, and organizations whose
members may (checked through the app, which has to be installed on the organization). The app's
owner is allowed when it's created. Everyone signed in can do what the admin can; runs they start
by hand show their login. Membership is checked when someone signs in, and GitHub sessions last a
week. Removing a user or organization from the list signs out everyone signed in with GitHub,
and revokes their API tokens; so does removing the app. The admin password keeps working, for
the API and git too.

## Command line

`kiwicd` (`go build -o bin/ ./cmd/kiwicd`) uses the API with an API token. `kiwicd login` asks
for the admin password, makes a token with it and saves the server and token in
`~/.config/kiwicd/cli.json` (mode 0600); the password itself isn't kept. People who sign in
with GitHub make a token under **Settings → API tokens** and pass it with `--token`.
`KIWICD_URL` and `KIWICD_API_TOKEN` override the saved ones, e.g. in scripts.

```sh
kiwicd login --url https://SERVER
kiwicd run list --repo owner/app             # kiwicd runs, for short
kiwicd run view 42                           # jobs and steps
kiwicd run logs 42 --failed                  # or --job build, --follow
kiwicd run start owner/app deploy.yml --ref main -i target=prod --watch
kiwicd run cancel 42; kiwicd run rerun 42 --watch
kiwicd workflow list owner/app
echo -n "$VALUE" | kiwicd secret set DEPLOY_KEY --repo owner/app   # or type it at the prompt
kiwicd secret list; kiwicd variable set REGION nz-north --repo owner/app
kiwicd api GET repos/owner/app               # any other API call
```

Secrets and variables without `--repo` are the global ones. `--watch` exits non-zero unless the
run succeeds, and list and view commands take `--json`. `kiwicd help` lists every command.

API tokens act as whoever made them, as a `Bearer` token or a Basic password, for the API and
git over `/git/`. The admin's can last forever; those of people signed in with GitHub last at
most 90 days, since organization membership is only checked when they sign in. Changing the
admin password revokes the admin's tokens, as it does their sessions. Tokens are listed and
revoked under **Settings → API tokens**, or with `kiwicd token list` and `kiwicd token revoke`;
`kiwicd logout` revokes its own.

### MCP

`kiwicd mcp` is an MCP server on stdio, with the saved login, for agents such as Claude Code:

```sh
claude mcp add kiwicd -- kiwicd mcp              # or: kiwicd mcp --read-only
```

Its tools list repositories, workflows, runs and runners, get a run with its jobs and steps,
read the end of a job's log (or one step's), wait for a run to finish, start, re-run and cancel
runs, and list, set and delete secrets and variables. Secrets stay write-only: the agent can set
them but not read them back. `--read-only` leaves out the tools that change anything. Give it
its own token (`KIWICD_API_TOKEN`) to revoke it separately from yours.

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
  `-e`, `--memory` and `--tmpfs` work as usual.
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
| `KIWICD_ADMIN_PASSWORD` | Required. UI login (user `admin`), git push and API basic auth. People can also [sign in with GitHub](#sign-in-with-github) |
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

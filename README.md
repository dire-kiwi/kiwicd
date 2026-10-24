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
  `needs`, job `if`, `strategy.matrix` (include/exclude, fail-fast), job `outputs`, `env`,
  `defaults`, `timeout-minutes`, `secrets`, `vars`.
- **Steps** run by act: `run`, local and remote actions (`composite`, `node`, `docker`),
  `docker://` images, `GITHUB_OUTPUT`/`GITHUB_ENV`/`GITHUB_PATH`/`GITHUB_STEP_SUMMARY`, workflow
  commands, `::add-mask::`, `hashFiles()`. `actions/checkout` of the workflow's own repository is
  served from the source archive the agent downloads.
- **Repositories**: *hosted* (push to `https://SERVER/git/OWNER/NAME.git`, which triggers
  workflows immediately) or *mirror* (a remote URL fetched every minute or on a webhook,
  `POST /api/hooks/OWNER/NAME` with GitHub's `X-Hub-Signature-256`). A mirror's first fetch
  only records existing branches and tags; later changes trigger runs.
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
  (see [below](#deploy-a-public-repository-from-a-private-one)).
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
  jobs that haven't started yet from getting them.
- **Job tokens**: every job gets `secrets.KIWICD_TOKEN`, which can fetch its repository over
  `/git/` while the job runs (and, for `repository_push` and dispatched runs from a protected ref,
  the repositories its workflow listens to that list it as a reader). It can't push or use the API.
- **Agents** register with a one-hour registration token, then use a long-lived runner token.
  `runs-on` labels must all match the agent's labels; every agent also has `self-hosted`,
  the OS (`Linux`) and architecture (`X64`/`ARM64`) labels.
- **UI**: runs, live job logs per step, re-run and cancel, repositories and workflows, manual
  dispatch, runners, secrets and variables (global or per repository).

Not yet: reusable workflows (`jobs.<id>.uses`), job `container:`/`services:` on host labels,
artifacts and caches, `concurrency` groups, pull-request events.

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

## Layout

| Path | |
|---|---|
| `cmd/kiwicd-server`, `internal/server` | HTTP API, git hosting, scheduler, SQLite store |
| `cmd/kiwicd-agent`, `internal/agent` | Registration, polling, running jobs with act, log reporting |
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

### Server configuration

| Flag / variable | |
|---|---|
| `KIWICD_ADMIN_PASSWORD` | Required. UI login (user `admin`), git push and API basic auth |
| `--data` / `KIWICD_DATA` | Database and repositories |
| `--public-url` / `KIWICD_PUBLIC_URL` | URL used in clone URLs, `github.server_url` and links |
| `--retention-days` / `KIWICD_RETENTION_DAYS` | Days to keep completed runs and their logs (default 30, 0 keeps them forever); also passed to jobs as `GITHUB_RETENTION_DAYS` |
| `--client-ip-header` / `KIWICD_CLIENT_IP_HEADER` | Behind a reverse proxy: the header it puts the client's address in (`X-Forwarded-For`, `CF-Connecting-IP`), so wrong admin passwords are limited per client (10, then one a minute) rather than per proxy. Only set it if every request comes through that proxy, since clients can send the header themselves |
| `KIWICD_REGISTRATION_TOKEN` | Optional long-lived registration token, for agents deployed next to the server |
| `--reserve-label` / `KIWICD_RESERVED_LABELS` | Keep a runner label for some repositories and refs: `LABEL=OWNER/REPO[@REF][,...]`, repeatable (the variable holds them separated by spaces). A bare `OWNER/REPO` means its default branch; `REF` is a branch, a full ref (`refs/tags/v*`) or a pattern (`@*` for any branch). A runner with a reserved label takes only jobs of runs those allow, whatever labels the jobs ask for, and jobs elsewhere that ask for the label fail at once. For runners that hold deploy credentials, reach production or have the Docker socket |
| `--max-log-lines` / `KIWICD_MAX_LOG_LINES` | Most log lines stored per job (default 1000000, `-1` for no limit). Later output is dropped after a notice; the job keeps running |
| `--max-log-bytes` / `KIWICD_MAX_LOG_BYTES` | Most log bytes stored per job (default 104857600). Lines longer than 64 KiB are cut |
| `KIWICD_SECRET_KEY` | Key that encrypts secrets and SSH deploy keys in the database: 32 bytes as base64 or hex (`openssl rand -base64 32`). Default: `<data>/secret.key`, generated on first start |

#### The secret key

Secret values and mirror deploy keys are stored encrypted (AES-256-GCM). The key is
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

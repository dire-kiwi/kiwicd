# kiwicd

## Getting changes onto master

The source of truth is `dire-kiwi/kiwicd` on GitHub; a commit that only reaches
a local `master` is never built or deployed. Changes go through pull requests:

- **When a task's work is done:** push its branch to `origin` and open a PR
  against `master` with `gh pr create --repo dire-kiwi/kiwicd --base master`.
  If the change moves the `act` submodule, push that commit to the `kiwicd`
  branch of `dire-kiwi/act` first, so the commit can be cloned with its
  submodule.
- **At the merge stage:** merge the PR on GitHub rather than merging into the
  local `master`: `gh pr merge <number> --repo dire-kiwi/kiwicd --merge`. If
  `master` has moved on, merge `origin/master` into the branch, push, and merge
  the PR. Then bring the project's main checkout (not the task's worktree) up
  to date with `git -C <main checkout> pull --ff-only origin master`, but only
  if it is on `master` and clean.

Deployment lives outside this repository, which is public: don't commit host
names, addresses, users or other details of a particular deployment.

[![Go Report Card](https://goreportcard.com/badge/github.com/Luzifer/clean-github-branches)](https://goreportcard.com/report/github.com/Luzifer/clean-github-branches)
![](https://badges.fyi/github/license/Luzifer/clean-github-branches)
![](https://badges.fyi/github/downloads/Luzifer/clean-github-branches)
![](https://badges.fyi/github/latest-release/Luzifer/clean-github-branches)

# Luzifer / clean-github-branches

This tool is intended to clean up branches in repositories having many contributors with several pull-requests: Most of the time people tend to forget to delete the branch after merging the pull-request and branches pile up. Running `clean-github-branches` against the repo will find

- branches whose corresponding pull-requests are merged and delete them
- branches not being ahead of the base branch and delete them after a configurable duration
- branches being stale and optionally delete them after a configurable duration

## Usage

```console
$ clean-github-branches --help
Usage of clean-github-branches:
      --branch-staleness duration   When to see a branch as stale (default 90d) (default 2160h0m0s)
      --delete-stale                Delete branches after branch-staleness even if ahead of base
  -n, --dry-run                     Do a dry-run (take no destructive action) (default true)
      --even-timeout duration       When to delete a branch which is not ahead of base (default 24h0m0s)
      --listen string               Port/IP to listen on (default ":3000")
      --log-level string            Log level (debug, info, warn, error, fatal) (default "info")
      --process-limit int           How many repos to process concurrently (default 10)
  -r, --repo-regex string           Regular expression the full repo (user/reponame) must match against
      --token string                Token to access Github API
      --version                     Prints current version and exits

$ clean-github-branches -r '^[lL]uzifer(-docker|-ansible|)/'
WARN[0013] Stale branch found                            ahead=1 behind=1 branch=develop dry-run=true repo=luzifer-docker/etherpad-lite
INFO[0013] Done.
```

All parameters causing destructive actions are set to sane defaults: By default a `dry-run` is done which prevents any deletion. Also `delete-stale` is disabled as it might cause data loss as the branch is not merged and all commits in it will be lost.

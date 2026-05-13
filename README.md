# Introduction

Parallel Git mass-cloner written in Go with the help of goroutines.

# Installation

```
git clone https://github.com/sdobrau/goc
cd goc
go install
```

# Usage:

```
usage: goc -f FORGE -u USER|-o ORGANISATION|-F FILE
Flags:
  -F string
        File to fetch cloning configuration from
  -T string
        SourceHut OAuth2 token
  -d string
        Root directory to clone to (default "/tmp/")
  -f string
        Forge to clone from
  -i string
        Instance URL to clone from
  -o string
        Organisation/group to clone from.
        For GitLab please use the path embedded in the group page URL.
  -s uint
        Only clone repositories with stars larger than N
  -t uint
        Number of concurrent git processes at which to slow down spawning. (default 20)
  -u string
        User to clone from
  -x    Ignore forks
```

# Features

Features:

* [x] Clone users or groups, organisations' repositories
* [x] Concurrency: as many as N concurrent git processes running
* [x] Forges supported:
  * [x] GitHub
  * [x] GitLab (including instances)
  * [x] Gitea (including instances)
* [x] Tokens (taken from envvars `GITHUB|GITLAB|GITEA_TOKEN` or provided per-user/organisation)
* [x] YAML file for declaring what and how to clone:
  * [x] Ignore forks
  * [x] Ignore with stars lower than a specific amount

# TODOs

* [ ] Subgroups support and integration for GitLab
* [ ] Pull from all existing repos first, then check
* [ ] Persist pulled repos and download only new repos, provided new repositories come up first in the REST response
* [ ] Proper SIGINT handling: wait for goroutines to finish first
* [ ] Test SourceHut integration
* [ ] Consistent struct naming: Why don't getters work for private fields?
* [ ] Logging options
* [ ] More test coverage
* [ ] Order test cases
* [ ] Limit spawning based on CPU usage

# Contributing to rushbin

Thank you for helping. rushbin is made by Artide Innova SRL and published under the
[Functional Source License](LICENSE.md): the code is open to read, change and run for any purpose except a
competing commercial product or service, and each version becomes Apache 2.0 two years after it is published.

## Before you start

- **Bugs:** open an issue with the bug form. Security problems never go in an issue: see [SECURITY.md](SECURITY.md).
- **Changes bigger than a fix:** open an issue first and say what you want to change and why, so no work is wasted.
- **The contributor agreement:** your first pull request asks you to sign [the CLA](CLA.md), once, with a click.
  It lets Artide ship your change in every rushbin it distributes, including the hosted site; you keep the
  copyright of your work.

## Build and test

You need Go (the version in `go.mod`), FFmpeg and FFprobe on the `PATH`, and for the team site's tests a Postgres
with pgvector you may create databases in:

```sh
docker run -d --name rushbin-test-db -e POSTGRES_HOST_AUTH_METHOD=trust -p 5432:5432 pgvector/pgvector:0.8.7-pg17-trixie
export RUSHBIN_TEST_DB=postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable
gofmt -l . && go vet ./... && go test ./...
```

Without `RUSHBIN_TEST_DB` the site's tests are skipped; CI runs them all, on Linux and Windows. The Mac app builds
with `macos/build.sh` (see the README). Reading RED and Nikon raw needs RED's SDK, which is not in this repository and
cannot be: a build from source lists those clips as plain files.

## What a good pull request has

- One change, explained in its description: what it fixes or adds, and how you checked it.
- A test that fails without the change, for anything with a branch, a loop or a parser.
- `gofmt` and `go vet` clean.
- Words a person reads in every language: a new key in `web/i18n/en.js` goes into each file of `web/i18n/`
  (`i18n_test.go` checks it). Write English if you do not speak the others; say so, and we translate.
- No new dependency for what a few lines of Go do.
- Comments that say why, not what.

## How the code is laid out

See "For developers" in the [README](README.md). Questions: open an issue.

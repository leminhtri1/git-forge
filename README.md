# git-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/leminhtri1/git-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/leminhtri1/git-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A small GitHub-compatible git forge in one Node 22 process, written on the standard library alone (no
dependencies, no git binary). It serves git smart HTTP (upload-pack protocol v2 with ls-refs, shallow and
incremental fetch; receive-pack v0 with report-status, side-band-64k and delete-refs), the REST v3 API
under `/api/v3` (repositories, branches, commits, contents, labels, issues, comments, pull requests,
their files and squash/merge merges), GraphQL v4 at `/api/graphql` (the documents `gh` and
github-mcp-server send, with introspection and batching) and a Model Context Protocol endpoint at `/mcp`
(streamable HTTP, `server/discover` and the `initialize` handshake), all over one in-memory store.

Layout: `main.ts` (lifecycle: bind, register with `$MAKEFUN_ENV_URL`, wait for done), `app.ts` (the
router), `src/git/` (pkt-lines, objects, packs, smart HTTP), `src/rest/`, `src/graphql/`, `src/mcp/`,
`src/ops.ts` (the operations every surface shares) and `src/store.ts`.

## Run it

```sh
node main.ts
```

Every API request needs `Authorization: Bearer $MAKEFUN_SHOP_TOKEN`. Without `MAKEFUN_ENV_URL` the forge
runs standalone on `FORGE_PORT` (or an ephemeral port); `FORGE_LOGIN` sets the account it serves.

---

<sub>Entered on [make.fun](https://make.fun/?ref=eh1gzvjg&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>

# pgoverlay-connect

Resolve a ready Postgres connection string for a
[pgoverlay](https://github.com/abd-ulbasit/pgoverlay) branch at startup.
Zero dependencies; Node 18+ (global `fetch`).

With per-branch credential rotation on, every branch has its own password,
so an app cannot hold a fixed `PGPASSWORD`. This helper keeps the app's
config static: it holds the branchd API endpoint and a read-only (`viewer`)
token, and fetches the branch's current credentials when it starts.

```js
import { resolve } from "pgoverlay-connect";

const { proxyDsn } = await resolve({
  server: process.env.PGOVERLAY_API,     // https://branchd:7070
  token: process.env.PGOVERLAY_TOKEN,    // a viewer token is enough
  repo: process.env.GITHUB_REPOSITORY,   // "acme/widgets"
  pr: process.env.PR_NUMBER,             // 7 -> gh-<key>-pr-7
  proxyHost: "pg.example.com:6432",
});
const client = new pg.Client({ connectionString: proxyDsn });
```

## Which branch

- `branch`: an exact branch name.
- `repo` + `pr`: the branch the pgoverlay GitHub App (`pgoverlay-github`)
  creates for that pull request in its default `pr-number` mode,
  `gh-<key>-pr-<n>`. `<key>` is six hex characters of the SHA-256 of the
  lowercased `owner/name`, so pull request #7 of two repositories never
  shares a branch.
- `repo` + `ref`: the branch in `git-branch` mode, `gh-<key>-<sanitized
  ref>`, for preview platforms that know the git branch before they know the
  pull request (`GITHUB_HEAD_REF`, `VERCEL_GIT_COMMIT_REF`). Pass the branch
  name (`feat/login`), not a full ref (`refs/heads/feat/login`). A ref with no
  letters or digits falls back to `pr`. Pull requests from forks are always
  named by number, so pass `pr` for them.

`branchName(opts)`, `prBranchName(repo, pr)`, `refBranchName(repo, ref)` and
`repoKey(repo)` return the name without contacting the server. They match the
Go helper (`pgoverlayconnect`) and the GitHub App service exactly; all three
are tested against one shared table.

## Options

All optional, see `index.d.ts`: `{ server, token, branch, repo, pr, ref,
proxyHost, password }`. `server` and `token` default to `PGOVERLAY_API` and
`PGOVERLAY_TOKEN`. `proxyHost` is `host[:port]` of the pgoverlay router
(`[v6]:port` for IPv6) and defaults to the server host on port 6432. When the
server does not rotate credentials (inherit mode) it returns no password, and
`password` or `PGPASSWORD` is used.

The result has `dsn` (the branch's Postgres directly) and `proxyDsn`
(through the router, database `db@branch`), plus `branch`, `host`, `port`,
`user` and `database`.

## Develop

```sh
node --test test/*.test.mjs   # unit tests; names.test.mjs reads pgoverlayconnect/testdata/branch_names.json
npm pack --dry-run            # verify the publishable file list
```

## Publish (manual, not wired to CI)

```sh
cd sdk/js-connect
npm version <patch|minor>
npm publish --access public
```

## License

Apache-2.0, see [LICENSE](LICENSE).

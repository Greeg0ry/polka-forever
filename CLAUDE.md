# Polka

Self-hosted e-book library: a single Go binary (HTTP API + OPDS + kosync) with an
embedded React SPA. Module path: `github.com/vestigiumincaligne/polka`.
Go 1.26, Node 20+ (CI uses 22). No CGO anywhere.

## Commands

```sh
make build                      # web UI + bin/polka + bin/polka-desktop
make test                       # go test ./... (needs web/dist, see below)
go test ./internal/...          # Go tests without building the frontend
go test ./internal/server -run TestName
go vet ./... && gofmt -l .      # CI runs vet; keep the tree gofmt-clean

cd web && npm run dev           # Vite dev server
cd web && npm test              # vitest run (jsdom)
cd web && npx vitest run src/components/Shelf.test.jsx
```

Run locally: `./bin/polka serve --data-dir /tmp/polka --library-dir /path/to/books`
(first run logs a one-time random password for `admin`).

### Build gotchas

- **`web/dist` is git-ignored but embedded** (`web/embed.go`, `//go:embed all:dist`).
  On a fresh checkout `go build ./...`, `go vet ./...` and `go test ./...` fail with
  `pattern all:dist: no matching files found` until `make web` has run. Packages
  under `internal/` do not import `web` and test fine without it.
- **Always build with `-tags=nodynamic`** (the Makefile and CI export
  `GOFLAGS=-tags=nodynamic`; a bare `go build` does not). It selects the pure-Go
  wazero backend for JPEG XL decoding so cross-compilation stays CGO-free.
- Frontend dev against a running server: put `VITE_API_URL=http://localhost:12791/`
  in `web/.env.local`. There is no Vite proxy.

## Layout

```
cmd/polka/            server CLI: serve | import | passwd | collections | version
cmd/polka-desktop/    desktop app: embedded server on 127.0.0.1 + WebView2 / Chromium window
collections/          bundled curated lists (*.json, embedded; format in collections/README.md)
internal/
  config/             flags with POLKA_* env fallbacks
  store/              catalog DB (polka.db): schema, import sessions, queries, FTS5 search, recs
  auth/               users.db: users, sessions, progress, ratings, lists, kosync, sync state
  collections/        collections.db: curated lists matched against the catalog; sources/ = scrapers
  library/            book files on disk: zip/7z archives, FB2/EPUB/TXT parsing, covers, JXL→JPEG
  inpx/, importer/    .inpx catalog reader and the import pipeline into store
  server/             HTTP handlers, middleware, OPDS, kosync, reader, admin, sync-mode routes
  syncer/             desktop "connected" mode: proxy to a remote Polka, offline cache, LWW sync
  enrich/             external ratings / similar books (LiveLib, Google Books, Open Library…), disk cache
  genres/             FB2 genre code → ru/en names
  mailer/             SMTP send-to-e-reader
  convert/            FB2 → EPUB/MOBI/… by running the external fbc / fb2c tools (optional at runtime)
  sqlitedrv/          SQLite driver selection (modernc; mattn on Android)
web/                  React 19 + Vite SPA (plain JS/JSX, plain CSS, no TypeScript, no UI kit)
packaging/            nfpm (deb/rpm) and NSIS installer
```

## Architecture

**Three SQLite databases in the data dir**, deliberately separate so a catalog
re-import never touches user data:

- `polka.db` — catalog (`internal/store`). Disposable: `import --replace` deletes it.
- `users.db` — accounts and all per-user data (`internal/auth`).
- `collections.db` — curated lists; `book_id` matches are recomputed after every import.

Never add user-owned data to `polka.db`, and never store catalog book IDs as the
only key for something that must survive a re-import.

Catalog schema changes: bump `schemaVersion` in `internal/store/schema.go`, add a
`schemaVN` constant and a step in `Store.migrate()` (`PRAGMA user_version`). The
store uses a single connection (`SetMaxOpenConns(1)`) — don't hold a rows cursor
open while issuing another query.

**Routing** lives in one place: `server.New` in `internal/server/server.go`
(stdlib `http.ServeMux` with method patterns). Wrap handlers with `s.protected`
(logged-in, or anyone in public mode), `s.adminOnly`, or `s.opdsAuth` (HTTP Basic).
Handlers that mutate synced user data are also wrapped in `s.maybeSyncAfter`.
`internal/server/access_test.go` holds a route access matrix — add new routes to it.

**Run modes** (`cfg.Auth`) change which routes are registered:

- `required` (default) / `public` — normal server. The README's `users`/`none`
  values are out of date; `internal/config/config.go` is authoritative.
- `demo` — public showcase with ephemeral guests; admin and write routes are absent.
- `desktop` — set by `polka-desktop`; auto-login owner, no user management.
  With a `syncer` (`s.sync != nil`) catalog routes are proxied to the remote server
  or served from the offline cache (`internal/server/syncmode.go`). A new catalog
  endpoint usually needs a counterpart there, or it will 404 in the connected desktop app.

**Format conversion** (`internal/convert`) shells out to `fbc` (fb2cng) and/or
`fb2c` (fb2converter) — GPL-3.0 programs that must stay external processes, never
Go imports. The server works without them: `getBookForm` returns `convertFormats`
only for what the installed tools can produce, and the book page renders one
button per entry. Tests use shell-script stand-ins, not the real tools.

URL prefixes `/main/getBooks/*` and `/Images/*` are a legacy contract the frontend
and OPDS clients depend on; newer endpoints go under `/api/v1/`.

**Frontend**: `web/src/Root.jsx` is the router (`HashRouter`) and holds auth/config
state. `api/api.js` is the fetch wrapper; one module per domain in `api/`. Each
page/component has a sibling `.css`; colors and spacing come from the tokens in
`theme.css`.

## Conventions

- **i18n is mandatory, two languages.** UI strings go into both the `ru` and `en`
  dictionaries in `web/src/i18n.js` and are read via `t("key")`. Server-side
  user-facing strings go through `tr(reqLang(r), key)` in `internal/server/lang.go`
  (language comes from the `X-Polka-Lang` header). Genre names: `internal/genres`
  (ru and en tables must stay consistent; a test checks this).
- Code comments and log messages are in English. CLI usage text and some defaults
  (e.g. the collection name «Полка») are Russian.
- Go: stdlib-first (`net/http`, `log/slog`, `flag`); adding a dependency needs a
  real reason, and it must be pure Go. Errors to the client via `http.Error`,
  JSON via `writeJSON`.
- Server tests use `newTestServer(t)` (`internal/server/api_test.go`): a temp dir,
  a real SQLite store and a synthetic zip library behind `httptest`. Prefer that
  over mocks. Library fixtures live in `internal/library/testdata/*.7z`.
- Frontend tests: vitest + Testing Library, colocated as `*.test.js(x)`; setup in
  `web/src/test/setup.js`.
- Commit messages: one imperative English line describing the user-visible change,
  with the cause after a colon when it is a fix (see `git log`).
- Any path taken from a request or an archive must stay inside the library root
  (`internal/library/confine_test.go`, `internal/server/security_test.go`).

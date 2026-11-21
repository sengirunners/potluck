# kit/server: the daemon's seam (read before working here)

Owner: integrator.

## What this is

`server.New` opens the store and the registry, runs the binary's `Wiring` (`internal/wiring`: adding a module is
one line in `Full`; this package imports no module), mounts the routes on one guarded `core.Router`, and serves the Unix socket. Middleware checks the
protocol, authenticates (`core.Authenticate`), and hands team-project calls to the replica proxy.

## What may cross

- Modules come in only through `core.Module` (`Routes`, optional `Start`/`Close`) and what their `New` gives
  `core.Deps` when the wiring builds them: services (`Provide`), migrations (`AddMigrations`), self-check probe parts
  (`AddProbe`), health parts (`AddHealthPart`) and request gates (`AddRequestGate`: after the caller is resolved,
  before the team proxy, the router and the route's auth class, so a gate also sees unauthenticated and public
  requests). So a daemon has the tables and checks of the modules its wiring builds, not of every package linked
  in. Only the shell's own tables (kit/project, kit/events) register from `init()`; they run when the store opens,
  and every other migration (registered or added) runs in one name-order pass after the wiring has built
  (`store.Migrate`). Module logic does not live here.
- Only the Unix listener marks a request local (`markUnix` → `core.WithLocal`). Every other listener (tailnet,
  loopback web, a future one) is network: token or WhoIs login, else 401. In tests,
  `client.WithHandler(dm.SocketHandler())` is the machine owner, while `dm.Handler` is a network caller that gets
  401 without a token. Never hand `SocketHandler()` to a listener. One exception: `runners.listen` (loopback)
  serves only the runner broker's own public handlers, never `dm.Handler`.
- A home whose term does not serve (`kit/hometerm`) gets a parked daemon (`term.go`): no store, no Wiring, no writer
  lock, only the modules of `ParkedListeners` (their Start and Close, never their routes; `Deps.Store`, `Projects`
  and `Events` are nil, so a caller of `New` checks them). The term for anyone; health, stop and unfreeze for the
  socket's owner; `parked` for everything else. A route or loop added to the shell runs only on a serving daemon
  unless it is added there on purpose.
- `parkGate` is in front of `Handler` and of every module public host (`core.PublicIngress(d).Gate`): once a daemon
  parks, and until its first ask of a named successor has an answer, nothing but health and the term reaches a route.
  In-process callers of `d.Handler` are held too, and that ask runs after every module's `Start`: a `Start` must not
  wait on a call through `d.Handler`. `Serve` waits for its own loops (the fence loop, the hourly optimize) before it
  closes the modules and the store.
- A serving daemon holds `config.LockWriter` from `New` to `Close`; every daemon's process holds `LockHome`.
- Known drift: `server.go` reads the `events` table directly. Don't add more. That read becomes a `core`
  method.

## The proof (keep it green; never weaken an assertion)

Tests that need zm's full daemon (`wiring.Full`) live in `internal/wiring/fulltest`: this package's tests move
with it into the potluck module and import no orchestration (`internal/deps` TestLayers).

| test | checks |
| --- | --- |
| `fulltest/routes_test.go` | every registered route is in API.md, verbatim |
| `fulltest/leak_test.go` | every route called as a guest over three transports: hidden answers like missing |
| `fulltest/leak_writes_test.go` | hidden writes refused; an unmarked listener fails closed |
| `fulltest/leak_team_test.go` | the same walk through a team replica and its authority |
| `fulltest/leak_webhooks_test.go` | hidden webhooks answer the guest like missing ones, in lists, errors and bodies |
| `fulltest/leak_ambient_test.go` | a tailnet login without a token: own Host, not cross-site, `Zm-Protocol` on writes |
| `unix_test.go`, `machine_test.go` | socket cleanup, the home lock, only Unix marked local; the machine name persists |

## Adding a route

1. Register it with `core.Router` in the owning module: access `public|authed|local|device`, plus the fine check
   in words.
2. List it in API.md (guard table and example), or `fulltest/routes_test.go` fails.
3. Seed the leak fixture if the route needs a hidden object to probe; the walk then calls the route by itself.
4. A public route returns nothing scoped. A route that writes from the network needs `Zm-Protocol` or a token.

Run `go test -p 2 ./internal/server/ ./internal/wiring/fulltest/`; CI runs the rest.

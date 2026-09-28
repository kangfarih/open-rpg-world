# go-server (rpg-world-server)

Kaetram-compatible game server in Go: one binary that runs as an
all-in-one world (`ws://127.0.0.1:9001`), a standalone hub router
(`ROLE=router`), or a shard registering with that router (`ROLE=shard`).
The JSON wire contract is identical to `packages/server` (see
`docs/SPEC.md`); it needs no `packages/*` change.

## Run

```sh
cd go-server
go mod tidy
go run ./cmd/server     # canonical runner: TESTMAP showcase (default ON), ws://127.0.0.1:9001
go run .                 # same server via the root shim (identical boot)
TESTMAP=0 go run ./cmd/server  # pure 9 real regions, no overlays
CLEAN=1 go run ./cmd/server    # clean mode: pure terrain + one equipped adventurer
COMBAT=1 go run ./cmd/server   # combat party mode: 4 bots + BossDummy
```

The game boot lives in `internal/server` (driven by `internal/app`
`Run`); both entries above run the exact same steps. Run from this
directory: map/persist paths (`../packages/server/data`, `data.db`) are
resolved relative to it.

Flags also work: `--clean` / `--noclean`, `--combat`, `--testmap=false` /
`--notestmap`. `CLEAN=0` / `COMBAT=` turn modes off (default OFF).

## Identity & accounts

`internal/account` owns the identity store (username/email, PBKDF2-HMAC-SHA256
210k iterations, password-reset tokens) in the same SQLite file the rest of the
shard uses; `internal/server/login.go` ports the TS login decision tree
(`incoming.ts handleLogin`): the Login/Register/Guest opcodes, the
`invalidpassword`/`invalidinput`/`userexists`/`emailexists`/`invalidlogin`/
`loggedin`/`disabledregister` reasons, and the `SKIP_DATABASE` shortcut. Every
rejection closes the socket the way the stock client can read it — code 1010
plus the reason string (`internal/net/reject.go`); the old bare `"ban"` text
frame was invisible to the client.

Guests are named `Guest0`, `Guest1`, … and are never persisted. Unlike TS, a
guest session can be **upgraded in place**: `/register <username> <password>
[email]` in chat (or a second `Login.Register` frame on the live socket)
converts it into a real account, keeping position, inventory, skills and quest
progress instead of starting over.

New env knobs (all optional):

| var | default | meaning |
|---|---|---|
| `SKIP_DATABASE` | on | TS parity: accept names without touching the DB |
| `DISABLE_REGISTER` | off | refuse `Register` with `disabledregister` |
| `MAX_PLAYERS` | 200 | world capacity (client world list + `worldfull`) |
| `DEBUGGING` | off | TS `config.debugging`: skip the reconnect/per-IP gates |
| `SERVER_ID` | 1 | world id this shard claims on the hub and posts to `/isOnline` |
| `HUB_DB` | `DB_PATH` | DB the router reads for `/leaderboards` + password reset |

## Router / multi-shard

The router serves the hub HTTP surface the stock client's world-select and
forgot-password flows call: `GET /` (liveness), `GET /all` and `GET /server`
(`SerializedServer` worlds), `GET /leaderboards`, `POST /isOnline` (cross-shard
duplicate-login check, `HUB_TOKEN`-guarded) and `POST /api/v1/requestReset` +
`/api/v1/resetPassword`. `/` carries both the shard hub socket and that
liveness body, dispatched on the WebSocket upgrade — a route pattern there
would shadow the socket and every shard dial would fail with `bad handshake`.

```sh
ROLE=router HUB_LISTEN=127.0.0.1:9101 HUB_TOKEN=tok /tmp/server &
ROLE=shard PORT=9001 HUB_ADDR=ws://127.0.0.1:9101/ HUB_TOKEN=tok SERVER_ID=2 DB_PATH=data-shard2.db /tmp/server &
curl -s 127.0.0.1:9101/all     # -> [{"id":2,"name":"shard",...}]
```

## Ports

- Server: `ws://127.0.0.1:9001` (client server PORT, HUB disabled).
- Client: stock Kaetram client at `http://127.0.0.1:9000`
  (`yarn workspace @kaetram/client dev --port 9000 --host 127.0.0.1`).

## Checks

```sh
go run ./cmd/server &      # TESTMAP=1 default (or `go run .` — same boot)
go run ./e2e/testmap       # showcase check
COMBAT=1 go run ./cmd/server &  # restart server first
go run ./e2e/combat        # combat check
CLEAN=1 go run ./cmd/server &   # restart server first
go run ./e2e/clean         # clean check

go run ./e2e/login                        # identity layer (SKIP_DATABASE default on)
SKIP_DATABASE=0 go run ./e2e/login        # + credentials, reset-free full coverage
```

The login harness dials a running server and waits out the 5s reconnect gate
between sessions (`toofast`, TS parity); boot the server with `DEBUGGING=1` to
skip both the gate and that wait.

Milestone harnesses (each against a TESTMAP=1 server — the default; use a
fresh DB per run, e.g. `DB_PATH=/tmp/e2e.db`, since logins restore persisted
positions). The harnesses dial a running server, they don't spawn one, so the
debug damage accelerators are server-side env: start the server with
`M9_MOBDMG=10` (m9 death leg) and/or `M11_HERODMG=10` (m11 skeleton kill
leg). m11's drop leg greps the server log, so pass `M11_SERVER_LOG=<server
log file>` to the m11 harness as well. Every harness (and the server)
honors `PORT` — set it on both when 9001 is taken, e.g. a TS dev server
running side-by-side:

```sh
go run ./e2e/m6                 # stores/bank/NPC/persistence
go run ./e2e/m7                 # chat + rank-gated commands
go run ./e2e/m8                 # minigames lobby/queue/score
go run ./e2e/m9                 # mob AI aggro/death/leash/kill
go run ./e2e/m10                # areas music/overlay/pvp/camera + chest flow
go run ./e2e/m11                # quests/achievements + gated drops/persistence
go run ./e2e/m12                # trade + crafting + enchanting
go run ./e2e/m13                # full commands.ts port (admin/mod/quest)
```

## Docs

See `docs/`: `SPEC.md` (wire contract), `GO-PLAN.md` (current state +
rolling-update plan), `DEPLOY.md` (runbook), `REWRITE-V2.md`,
`GO-SERVER-PLAN.md` (superseded by GO-PLAN.md), `WORLD-RECREATION.md`,
`CLASS-DESIGN.md`, `CLASS-DESIGN-V2.md`, `COMBAT-SKILLS.md`, `CHAOS-R4.md`,
`CLIENT-ASSETS.md`, `NOTES.md`.

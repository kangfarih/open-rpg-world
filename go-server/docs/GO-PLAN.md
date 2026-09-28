# Go Plan — Server Recreation (Part I) + Rolling Updates (Part II)

Contract: JSON wire identical (`SPEC.md`: bulk `[[id,data]|[id,opcode,data]]` over `ws://host:port`). No `packages/*` edits. All game logic reimplemented in Go.

## Part I — Recreation (condensed from GO-SERVER-PLAN.md)

### 0. Stack

CURRENT (2026-09-22): Go 1.25 module `rpg-world-server`, `gorilla/websocket`, `modernc.org/sqlite` (pure Go, no cgo) `PRAGMA journal_mode=WAL; synchronous=NORMAL`, single-writer store (`internal/persist`, `SetMaxOpenConns(1)`), 23 `internal/*` packages (TS-mirror layout, see §2), `cmd/server` shim, 15+ black-box e2e harnesses (`go-server/e2e/*`) all green. Root `package main` retains the runtime wiring (transport, registry, dispatch, tick) with thin adapters over the packages.
TARGET (remaining): `go:embed` data + `meta.world_hash` boot gate; R4 chaos drill re-run against the identity layer. Landing status 2026-09-28: multi-server hub transport DONE (R1–R3, §12), identity/accounts layer DONE (A1 below). `gobwas/ws` swap is optional (gorilla serves current scale).

### 1. Inventory

Subsystems: bootstrap; conn + anti-spam (handshake/login/ready, IP throttle, chat bucket); regions/areas (48 tiles/region, sideLength 24, surroundingRegions); movement + anticheat (speed/collision/entity-grid verify, teleport-back); combat + projectiles (GCD, aggro, poison/burn/freeze/bleed); gathering (deplete→respawn); drops/loot (30–50s despawn); quests + achievements; stores/bank/trade/craft/enchant (stub: stores in-memory only); guilds/friends/hub; chat/commands ~1300 lines; abilities; minigames teamwar/coursing; NPC/pet/stats.
Data (verified 2026-09-15): `items.json` 525, `mobs.json` 148, `npcs/spawns/tables.json`, `trees/rocks/fishing/foraging.json`, `stores.json`, `crafting/` 7 files, `quests/` 21 + `quest_bases/` 28, `abilities.json`, `minigames.json`, `map/world.json` 1152×1008; `sprites.json` client-only, no port.
Packets: 61 (0–60 Connected…AdminSync; 59 Resource, 60 AdminSync) + opcodes (movement 0/1/2/3/4/5/7, equipment Batch0, store/guild/quest/ability/minigame sub-ops). Port all 53 `network/impl/*.ts` frames exactly.
State split — persistent DONE (SQLite, `internal/persist` + subsystem tables): players, equipment/inventory/bank, skills/XP, abilities, quests/achievements, guilds (`guilds`/`guild_members`), friends, m13 flags, accounts (`internal/account`); dirty-flush every 10s + on disconnect + SIGTERM barrier. Schema gating DONE: `meta.schema_version` (`internal/persist/schema.go`, v9) stamps fresh DBs, re-stamps older ones forward after the expand-only DDL converges them, and refuses boot against a newer-than-known schema. TODO: `meta.world_hash` boot gate. In-memory: entities, region buckets, combat/aggro/projectiles, loot/chests (30–50s), resource timers, store 20s refresh, minigame lobby/queue, sessions + spam buckets, map frame cache.

### 2. Package layout — ACTUAL (TS-mirror; extraction E0–E9 landed, see §4 ledger)

- `cmd/server` — thin shim over the root runner (canonical entry pending root dissolution).
- `internal/protocol` — 61 packet IDs + opcodes + `EntityData/PlayerData` + `pkt/pktOp/bulk` (canonical; root `packets.go` is an alias shim).
- `internal/net` — `Bus` interface + rate `Limiter` (16/IP, 300 msg/s, chat 3-burst); root implements Bus via `bus_impl.go`.
- `internal/world` — modes, movement/anticheat verify, tick `Engine` + cadence consts, `Store` seam (staged; registry maps still live in root).
- `internal/worldmap` — canonical world store (`LoadDefault`, `BuildTile`, `SurroundingRegions`, `IsBlocked`) + lights/signs orchestration.
- `internal/entity` — mob engine, areas system, pet registry/orchestration (live-conn structs stay in root adapters).
- `internal/player` — pure-data `Player` model + `chat/` pipeline + `quest/` engine (registry, lifecycle, frames, SQLite).
- `internal/controller` — trade/craft/enchant, stores/bank/equipment/NPC, warps/events orchestration, full commands tables.
- `internal/persist` — single-writer SQLite store (players/inventory/bank/equipment/skills), identical schema/pragmas/cadence.
- `internal/meta` — LevelExp table + damage/accuracy math (wired into m5 + combat).
- `internal/minigame` — coursing/teamwar rules + `Manager` lifecycle (transport via `Effects`).
- `internal/abilities` + `internal/status` — ability registry + DoT tracker (wired: quest rewards grant, 20Hz tick damages).
- `internal/pets`, `internal/friends`, `internal/guilds`, `internal/hub` — registries + relay router (all-in-one mode).
- `internal/warps`, `internal/events`, `internal/globals` — pure tables/scheduler/loaders.
- `internal/api`, `internal/console`, `internal/app`, `internal/sim` — REST, stdin admin, boot config, demo-scene math.

### 3. SQLite schema — ACTUAL (single-writer store, expand-only migrations via `meta.schema_version`, currently v9 = v8 + the `accounts` table/index fold-in)

```sql
PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;
CREATE TABLE players(instance TEXT PRIMARY KEY, name TEXT, x INT, y INT, level INT, hp INT, max_hp INT, mana INT, max_mana INT, orientation INT, data JSON);
CREATE TABLE inventory(player TEXT, slot INT, item TEXT, count INT, enchant JSON, PRIMARY KEY(player,slot));
CREATE TABLE equipment(player TEXT, slot TEXT, item TEXT, enchant JSON, PRIMARY KEY(player,slot));
CREATE TABLE bank(player TEXT, slot INT, item TEXT, count INT, PRIMARY KEY(player,slot));
CREATE TABLE skills(player TEXT, skill TEXT, level INT, xp BIGINT, PRIMARY KEY(player,skill));
CREATE TABLE stats(player TEXT PRIMARY KEY, points INT, data JSON);
CREATE TABLE abilities(player TEXT, ability TEXT, unlocked INT, PRIMARY KEY(player,ability));
CREATE TABLE quests(player TEXT, quest TEXT, stage INT, done INT, PRIMARY KEY(player,quest));
CREATE TABLE achievements(player TEXT, ach TEXT, progress INT, done INT, PRIMARY KEY(player,ach));
CREATE TABLE guilds(id TEXT PRIMARY KEY, name TEXT, owner TEXT, data JSON);
CREATE TABLE guild_members(guild TEXT, player TEXT, rank INT, PRIMARY KEY(guild,player));
CREATE TABLE friends(player TEXT, friend TEXT, PRIMARY KEY(player,friend));
CREATE TABLE meta(k TEXT PRIMARY KEY, v TEXT); -- world.json hash, schema version
```

In-memory only: live entities, region buckets, combat/aggro/projectiles, loot/chests (30–50s), resource timers, store cache (20s TTL), minigame lobby/queue, sessions + spam buckets, map frame cache.

### 4. Milestones — ALL DONE (parity complete; every item e2e-green, see `go-server/e2e/`)

- M1 boot+map+spawn — DONE: `Connected→Handshake→Login→Welcome+Map→Ready→Spawn*`, `[4,base64gzip,bufSize]` 9 regions. Modes: TESTMAP (default + overlays) / CLEAN=1 / COMBAT=1. TODO (D3): full-world streaming + `go:embed` + `meta.world_hash` gate.
- M2 movement+anticheat — DONE (`ed1b2a0` + E9b `internal/world` verify funcs): speed check, teleport-back, noclip bypass, `List.Positions`.
- M3 combat+projectiles+GCD — DONE (`955ca3e`): formulas (`internal/meta`), `Combat/Heal/Effect`, projectile flight, aggro/leash/respawn, DoT ticks (`internal/status`).
- M4 resources+gathering — DONE (`5494a20`): all tables, tool tiers, deplete→respawn, `Animation` + `Resource` sync.
- M5 drops/loot+XP/skills — DONE (`29e2cb5` + E9a `internal/persist`): drop tables → lootbag/chest, pickup, XP curves, SQLite persist. Plus audit batch: C→S Examine (mob/item descriptions + `NO_IDEA`), `internal/player/stats` milestone achievements (skill/examiner, persisted JSON blob, schema v2), LootBag Open/Take lifecycle (Step opens, Target opens + take-all, per-index Take).
- M6 stores/bank/NPC/equipment — DONE (`db20f81` + E6 `internal/controller`): buy/sell/select + 20s refresh, bank, containers, equip (`e2e/m6`).
- M7 chat/commands — DONE (`f1b4246` + E3 `internal/player/chat`): region/global/PM, rank gates (`e2e/m7`).
- M8 minigames — DONE (`9678f04` + E2 `internal/minigame` Manager): coursing/teamwar lobby/queue/score (`e2e/m8`).
- M9 mob AI — DONE (`054659e` + E5 `internal/entity/mob`): roam/chase/leash/attack, death/respawn (`e2e/m9`).
- M10 areas — DONE (`054659e` + E5 `internal/entity/areas`): music/overlay/pvp/camera/chest/dynamic (`e2e/m10`).
- M11 quests+achievements — DONE (`9356043` + E7 `internal/player/quest`): 21 quests, gated drops, SQLite (`e2e/m11`).
- M12 trade/craft/enchant — DONE (`c0d76ae` + E1 `internal/controller`): sessions, recipes, shards (`e2e/m12`).
- M13 commands — DONE (`a42748a` + E8 `internal/controller`, + admin-cheat batch `internal/controller/progression.go`: all 117 `commands.ts` cases incl. addability/addexp/setlevel/max/resetskills/setability/setquickslot/resetabilities/setpet/setrank/openbank/poison/poisonarea/attackrange/debug/resetregions/ipban) (`e2e/m13`).
- A1 identity + hub client surface — DONE (2026-09-28): `internal/account` (PBKDF2-HMAC-SHA256 210k, email + reset tokens, schema v9) and the `incoming.ts handleLogin` decision tree (Login/Register/Guest opcodes, `SKIP_DATABASE`/`DISABLE_REGISTER`/`MAX_PLAYERS` env, guest nameplates, worldfull gate, guest persistence guards, guest guild restrictions) with 1010 close reasons on every reject (`internal/net/reject.go`); hub client endpoints `GET /all`, `GET /server`, `GET /leaderboards`, `POST /isOnline`, `POST /api/v1/requestReset|resetPassword` (`internal/app/router.go`, `internal/api`); shard presence via `HUB_ADDR` (`internal/server/presence.go`); world ids pinned by `SERVER_ID` on the hub handshake so the router and the shard's own duplicate-login check agree; `/` dispatches hub socket vs liveness body. New in Go (TS has no equivalent): **guest → account upgrade in place** (`/register` in chat, or a `Login.Register` frame on the live guest socket), keeping position/inventory/skills/quests. E2E: `go-server/e2e/login` (14 checks).
- Explicit non-goals: admin web panel, NATS transport, background-asset streaming, cross-region atomic trades. Sentry opt-in, Discord hook, and profanity filter remain open optionals.
- P-A abilities+status — DONE (`internal/abilities`, `internal/status`, quest rewards grant; `e2e/abilities`).
- P-B pets — DONE (`internal/pets` + `internal/entity/pet`; `e2e/pets`).
- P-C friends/guilds/hub — DONE (`internal/friends`, `internal/guilds`, `internal/hub` all-in-one Router; `e2e/social`).
- P-D warps/events/globals — DONE (`internal/warps`, `internal/events`, `internal/globals` + live multipliers; `e2e/world`).
- P-E api/console/hardening — DONE (`internal/api`, `internal/console`, `internal/net` Limiter; soak 16-admit/4-reject).
- E0–E9 extraction — DONE (commits `e1beb2d`, `23a437a`, `382c472`, `fe3c12f`, `c80843e`, `c39bea6`, `a25e86a`): TS-mirror `internal/*` owns all domain logic; root `package main` retains ONLY runtime wiring (transport, registry maps, dispatch switch, tick, boot) behind documented seams. Physical dissolution of that residue is D2-followup (needs call-site unfreeze, one coordinated pass).

### 5. Perf notes — ACTUAL (all wired; soak-verified)

20 Hz tick (`FlushInterval` 50ms, `internal/world` consts); one bulk `[...]` write per conn per tick (20Hz outbox flush); region interest (surrounding regions); Map gzipped once at boot (cached frame); single-writer SQLite (`internal/persist`, max-open-conns=1), flush dirty every 10s + on disconnect + SIGTERM barrier; WAL + NORMAL; per-IP cap 16 + 300 msg/s + chat 3-burst (`internal/net` Limiter, enforced at accept/read/chat). Soak: 20 concurrent dials → 16 admitted / 4 rejected, server healthy, `testmap` green after.

### 6. Gap closures (audit 2026-09-15, all TARGET unless noted)

- G1 commands (`internal/social/commands.go`, rank-gated player<mod<admin): player/mod/admin/boss/teleport groups (~120 cases); unknown → Chat error; log admin use.
- G2 minigames: teamwar vs coursing sharing a lobby/queue/instance interface; `minigames.json` drives rules; Join/Leave/Queue/Start/Score/End.
- G3 areas (`internal/world/areas.go`): camera/music/pvp direct; chest≈loot, dynamic≈trigger, minigame≈instance, overlay≈safe; checked every Step + teleport.
- G4 globals: static light table on region enter; sign text on interact; read-only, no persistence.
- G5 mob AI: spawn-queue → roam → chase → leash → return → boss phase hooks.
- G6 pets: follow (teleport if >12 tiles), attack, tame rolls, hunger/expire timers; never block movement.
- G7 loot: blink (10s `Sync` warning) → destroy; chest refill from `tables.json`.
- G8 stores: Buy/Sell/Select; 20s snapshot TTL; `stores.json` prices authoritative.
- G9 social: friends + guilds (persist `guilds`/`guild_members`) + hub relay; Discord = local-broadcast hook only.
- G10 REST + hub auth: read-only `/players/:name /guilds /status` (env `API_PORT`, default off); hub bearer `HUB_TOKEN`.
- G11 Sentry opt-in (`SENTRY_DSN` empty = off, sample 0.1); admin web panel out-of-scope.
- G12 E2E (M8): Cypress parity + headless Go harness (`go-server/e2e/`); M8 gates on both green.
- G13 map-build: `tools map` export is a required pre-build step; `meta.world_hash` mismatch → refuse boot.
- G14 hub (`internal/social/hub.go`): server-list + player-location cache (TTL) + offline mailer; single goroutine, token-guarded.
- G15 filter + i18n: profanity wordlist (mask `***`, mod-bypass, mute counter); server strings English-only.
- G16 port checklist: controllers→`net`; combat/effects/projectiles/formulas→`combat`; abilities/XP→`skills`; equipment/containers→`entities`/`persist`; quests→`quests`; 53 frames→`net`; config→`cmd/server`.

## Part II — Rolling Update System (from REWRITE-V2.md + brainstorm)

### 7. CURRENT vs TARGET

CURRENT (2026-09-22): full-parity all-in-one binary (SQLite WAL per process, random per-conn player instances, `Handshake{gVer}` received but ignored — no gate, all-in-one hub `Router` only, no server-list/heartbeat). Client points directly at the server.
TARGET: one `go-server` binary as `router` / `shard` / `all-in-one` (flags); SQLite WAL per shard + shared account DB; stamped `buildID` (git SHA) + `gVer` with enforced gate; standalone router + hub server-list; RUNNING→DRAINING→SHUTDOWN lifecycle; expand-only migrations. Nothing below assumes all-in-one behavior.

### 8. Worlds-as-versions

A deploy = new world rows in the hub server-list tagged with the new version (`buildID`, `gVer`, state). Router sends all NEW logins to the newest healthy (RUNNING) version; the old version drains: no new sessions, existing players finish tasks naturally, state persists, then shutdown on empty or drain timeout. Rollback = router swap-back to the old version (kept warm) — no data migration, no redeploy.

### 9. gVer lockstep + client banner

Client build is pinned to server `gVer`: router rejects mismatched `Handshake{gVer}` with a hub redirect at the correct build. Mismatched clients stay playable on the old version until they refresh — no forced kick. Banner (via existing Chat/notice frame, no wire change): "new version available — refresh when ready". Refresh loads the new bundle and opens a new socket atomically (old socket closes only after the new session is up). Cross-build version swaps are always disconnect+reconnect via hub; `Teleport` stays same-socket only.

### 10. Lifecycle, canary, health

Per instance: `RUNNING → DRAINING → SHUTDOWN`. Deploy: start new build alongside old (new port, self-registers RUNNING) → canary 5% of new logins, health-gate → 100% swap (router swaps the live tag blue→green) → SIGTERM old → DRAINING (no new conns, sim continues) → flush + exit on empty or drain timeout (default 30 min, flag-tunable). Health endpoint (`/healthz` + state/load) with 5s heartbeats; 3 missed beats evicts the entry. Forced evacuation = notice + disconnect reason (never `Teleport`); client re-logs via hub.

### 11. Restartability, migrations, ownership

No RAM-only progression: every XP/item/quest grant enters the write queue before its packet goes out; flush every 30s dirty-set + on disconnect/handoff + mandatory pre-shutdown barrier. Ephemeral state (positions, aggro, timers, routing table) is rebuilt from DB/spawn tables on restart. Migrations are expand-only (nullable/default columns, additive tables; drops/renames two releases later); binary refuses boot on newer-than-known schema. One single-writer owner per DB file (`sql.DB` max-open-conns=1 + write mutex); old and new builds never dual-write — cross-build state transfers by owner-to-owner RPC (copy-on-migrate is fallback only).

### 12. Milestones R1–R4

Map: shards own region groups within one world version; worlds-as-versions = parallel blue/green world copies across a version swap; R1–R4 = V2-M1–M4 respectively.

- R1 router + gVer gate: `Register`/heartbeat table, newest-RUNNING routing, `Handshake{gVer}` reject→hub redirect, SIGTERM→DRAINING, drain timeout + flush barrier, `/healthz`. Done: `kill -TERM` mid-session keeps players on; new logins land on new proc. Relaxation allowed: ship router-only (no shard sim) and gate the criteria on routing + drain, not world sim.
- R2 multi-world versions: version-tagged world rows, new-login swap, old drains, cross-shard handoff RPC + router region→build lookup; same-build shard move = seamless, no reload (RPC/socket handoff); cross-build version swap = disconnect+reconnect via login/hub with reload, never bare Teleport across builds; refresh banner via Chat-19 (Notification-25 alt), no new opcode. Done: two versions live side by side, clients split by login time.
- R3 runbook + rollback: backup (Go-side snapshot via `VACUUM INTO '<timestamped>.db'` through database/sql on the owner connection, no sqlite3 CLI, timestamped archive) → migrate → start new → health-gate → canary 5% → 100% → TERM old → warm-hold 15 min → shutdown; rollback = router swap-back. Done: full deploy with players online, only gVer-mismatched redirects.
- R4 chaos drill: 100+ bots, `kill -TERM` mid-fight/mid-trade, `kill -9` (WAL recovery), skew soak. Pass: no item/XP/quest loss, p95 tick <50 ms, hub reconnect <10s.

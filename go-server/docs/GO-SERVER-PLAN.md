# Go Server — Full Recreation Plan (stock web client, custom world)

> Historical planning doc (2026-09-15): it describes the original stub. For the
> current state — full game parity, the router/shard split, the schema gate and
> the identity/accounts layer — see `GO-PLAN.md`. Kept for the inventory and
> port-source checklists below.

Contract: keep JSON wire identical (`SPEC.md`: bulk `[[id,data]|[id,opcode,data]]` over `ws://host:port`). No `packages/*` edits, no client change. All game logic reimplemented in Go.

## 0. Stack

CURRENT (stub as built): Go 1.21 module `rpg-world-server`, `github.com/gorilla/websocket` for WS, flat `main.go` + `packets.go`, in-memory state only — no DB, no SQLite, no persistence yet.
TARGET: Go 1.22 + `gobwas/ws` + `modernc.org/sqlite` WAL + single binary `kaetram-stub`. Choice: `gobwas/ws` for raw low-alloc WS + manual JSON bulk write per tick; `nhooyr.io/websocket` is the fallback if HTTP-mux integration or WASM-adjacent API matters more than alloc control. `modernc.org/sqlite` (pure Go, no cgo) with `PRAGMA journal_mode=WAL; synchronous=NORMAL; busy_timeout=5000`. Single writer goroutine + prepared statements. Config via env (`HOST/PORT/HUB_ENABLED`). Data: copy JSON from `packages/server/data/` at build or embed via `go:embed`.

## 1. Inventory (what to port)

Subsystems: bootstrap (config/env/loader/world init); connection + anti-spam (handshake/login/ready, per-IP throttle, chat-spam bucket); regions/areas (MAP_DIVISION_SIZE 48 tiles/region, sideLength 24, surroundingRegions, area triggers/music/pvp); movement + anticheat (request/step/stop/follow/speed, collision/entity-grid verify, teleport-back); combat + projectiles (melee/range/magic, poison/burn/freeze/bleed, GCD, aggro/target); resources/gathering (trees/rocks/fishing/foraging, shake→deplete→respawn); drops/loot (mob tables, chest/lootbag, 30–50s despawn); quests + achievements (talk/kill/gather/craft stages, rewards — impl handlers + data defs, see per-file counts below); stores/bank/trade/craft/enchant (buy/sell/select, bank stash, trade sessions, recipes/tables, enchant reroll — CURRENT stub: stores in-memory only); guilds/friends/hub (create/invite/rank, friend list, hub routing); chat/commands ~1300 lines (say/global/guild + admin commands); abilities (mana/cd/effects/projectiles); minigames teamwar/coursing (lobby/queue/team/score); NPC/pet/stats (shop/dialog nodes, pet follow/attack, level/XP/stat allocation).
Data files (per-file, verified 2026-09-15): `items.json` 525, `mobs.json` 148, `npcs.json`, `spawns.json`, `tables.json` (drop tables), `trees/rocks/fishing/foraging.json`, `stores.json`, `crafting/` 7 recipe JSON files (`alchemy/chiseling/cooking/crafting/fletching/smelting/smithing.json` + `index.ts` loader), `quests/` 21 JSON files + `quest_bases/` 28 JSON files, `abilities.json`, `minigames.json`, plus `map/world.json` (1152×1008, collisions/objects/cursors); `sprites.json` is client-generated/optional (not server-authoritative, no port).
Packets: 61 (`packets.ts` 0–60 Connected…AdminSync — 59 Resource, 60 AdminSync) + opcodes (`movement`: Request0/Started1/Step2/Stop3/Move4/Follow5/Speed7; `equipment` Batch0; `store` Buy/Sell/Select; `trade`, `guild`, `quest`, `ability`, `minigame` sub-ops). Port all 53 `network/impl/*.ts` send/recv shapes exactly (Handshake1/Welcome3/Map4/Spawn5/Equipment8/Sync10/Movement11/Teleport12/Despawn13/Animation16/Chat19/Combat/Heal/Effect/Resource59 etc).
State split — persistent (TARGET, TODO — no DB in stub yet): players, equipment/inventory/bank, quests/achievements, skills/XP, stats, abilities, guilds. `meta` table + schema-version/migration story is TODO with the SQLite work. In-memory only (CURRENT + carried forward): world/entities, grids/regions, combat instances, loot/chests (30–50s), resources (deplete/respawn timers), store cache (20s TTL), minigame lobby/queue.

## 2. Package layout — TARGET STRUCTURE (TODO M2+; CURRENT = flat `main.go` + `packets.go` in `go-server/` root)

- `cmd/server` — boot: env, load data, open SQLite, build world, start hub+WS, signal flush.
- `internal/net` — WS hub, bulk codec, 61 packet IDs + opcode consts, port of 53 `impl/*` frames; region-scoped `send/filter/broadcast`; rate limits (`MAX_CONNECTIONS` per IP, msg/s + chat buckets).
- `internal/world` — map load, grids (collision/entity), regions/areas, pathing helpers, surroundingRegions.
- `internal/entities` — player/mob/npc/pet/projectile/item/chest/lootbag/resource structs + (de)serialize matching `EntityData/PlayerData` (one json tag, omitempty optionals; never send EntityType 6).
- `internal/combat` — damage formula, GCD, aggro, poison/burn ticks, projectile flight+Hit.
- `internal/skills` — gathering rolls + crafting rolls, XP curves (19 skills incl Chiseling/Smelting pseudo).
- `internal/quests` — quest handlers over `quests/` 21 + `quest_bases/` 28 data defs + achievements engine.
- `internal/economy` — stores/bank/trade/craft/enchant sessions + 20s store cache.
- `internal/social` — guilds/friends/hub routing + chat/commands (~1300 lines port).
- `internal/persist` — SQLite schema + single-writer store, dirty-flush.
- `internal/minigame` — teamwar/coursing lobby/queue/score/instancing.

## 3. SQLite schema + in-memory list — TARGET (no DB in stub yet; CURRENT = all in-memory; `meta`/migration TODO with this work)

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

In-memory only (no tables): live `instance→*Entity` maps, region buckets + entity grids, combat/aggro/projectile state, lootbag/chest objects with 30–50s expiry timers, resource states/respawn timers, store snapshot cache (20s TTL), minigame lobby/queue/scores, connection sessions + spam buckets, map frame cache (`sync.Once` gzip+bufSize).

## 4. Milestones

- M1 boot+map+spawn — DONE in stub: `Connected→Handshake→Login→Welcome+Map→Ready→Spawn*`, `[4,base64gzip,bufSize]` 9 regions around spawn, entity-grid block + Stop/Teleport-back. Actual stub modes: TESTMAP (default ON — clone of 9 real regions + overlays: center pond, 5-oak row, 232-entity showcase grid stamped over base) / CLEAN (`CLEAN=1` — pure original terrain, zero overlays, exactly 2 players: Welcome hero + one fully-equipped adventurer Spawn + Equipment Batch) / COMBAT (`COMBAT=1` — pure terrain + combat scene: boss dummy + 4-bot party: warrior/archer/mage/support). TODO: full-world region streaming + embed all data files.
- M2 movement+anticheat — PARTIAL (stub single-resource block only): TODO full collision/entity-grid verify on every Request/Step, speed cheat check, Follow/Speed ops, `List.Positions` correction, region handoff `List.Spawns`.
- M3 combat+projectiles+GCD — TODO: melee/range/magic formulas, `Combat/Heal/Effect` packets, projectile `type:5` (owner+target+hit) flight, aggro/leash/respawn, poison/burn/freeze ticks, death→despawn.
- M4 resources+gathering — PARTIAL (Animation shake + Resource stump in stub path): TODO all trees/rocks/fishing/foraging tables, tool tiers, deplete→respawn timers, `Animation{resourceInstance}` + `Resource{state}` sync.
- M5 drops/loot+XPs/skills — TODO: drop tables → lootbag/chest Spawn (30–50s despawn), pickup, XP curves + level-up `Sync`, 19 skills.
- M6 stores/bank/trade/craft — DONE: store buy/sell/select + 20s cache, bank stash, NPC talk, containers/equip in stub (e2e/m6 green); quest/achievement half in M11 (DONE below); trade sessions (mutual request, offers, double-accept exchange, undroppable guard), `crafting/` recipes via `/open<skill>` commands, enchanting via the vendingmachine NPC (shard consume + persist, e2e/m12 green).
- M7 social+hub+chat/commands — PARTIAL: the full `controllers/commands.ts` port is DONE in stub (m13.go, e2e/m13 green): player guild subcommands (create/join/leave + pending-invite flow, m13flags persistence), moderator table (mute/unmute silencing the chat gate, ban + socket close with the TS two-step `ban 'ban'` chain, kick close, jail → spawn teleport), admin table (/spawn /takeitem /take /clear, /tp, /ms + /noclip with the movement anti-cheat relaxations, /toggle effect frames, /finishquest quest-admin via the m11 registry, /template, rank self-service up to admin). TODO: guilds/friends persistence + hub routing, chat channels, anti-spam + `MAX_CONNECTIONS` per-IP + msg/s limits.
- M8 minigames+hardening — TODO: teamwar/coursing lobby/queue/instance/score; hardening: region-scope all sends, batch bulks per tick, rate limits, store-cache TTL, load/soak test, `go vet/build/gofmt` green.
- M9 mob AI engine — DONE in stub (e2e/m9 green): data-driven profiles (mobs.json 148 + spawns.json overrides), 500ms engine tick (roam/chase/attack/leash/return-home), aggro + retaliation, hero HP/death/respawn flow, kill → loot + M10 chest hooks, profile respawn timers. TESTMAP debug dispatcher rides `[46 {m9test}]` (spawn/remove/tp/mobhp); `M9_MOBDMG` damage accelerator for the e2e death leg.
- M10 area system — DONE in stub (e2e/m10 green): world.json `areas` groups — music (song change `[30,null,song]`, exit `[30,null,<empty>]`), overlay (Set/Remove + freezing status effect), pvp (state flip + IN/NOT_IN_PVP_ZONE notify, flag feeds Spawn PlayerData), camera (lockX/lockY/player + FreeFlow exit; TESTMAP injects the band the real map lacks), chest areas (clear → reward chest Spawn at spawnX/spawnY, mob-respawn guard re-adopt removes it, open → item roll → persistent M5 loot pickup), dynamic (linked pairs parsed, per-tile remap documented divergence). Movement path runs the detectAreas parity hook on every position update; per-player change-detection state is dropped on disconnect.
- M11 quests+achievements engine — DONE in stub (e2e/m11 green): data-driven registry over data/quests/*.json (21) + achievements.json (quest_bases/ parsed for parity only — authoring drafts with no TS impl); per-player stage/subStage state over the quest.ts lifecycle (stage 0 → Start prompt unless noPrompts; talk stages progress at dialogue end with itemRequirement text swap + item consume on the final click; kill stages advance subStage per mob and stage at mobCountRequirement; resource stages count resource hits), skill/quest requirement gating; Quest [23] Batch/Progress/Start + Achievement [24] Batch/Progress flow with login batches, Pointer [36] Location/Remove + Notification Popup [25,3] side channels; quest-gated drops via m11DropGated + m5RollEntryGated (skeletonkingtalisman only while codersglitch is started; gate state persists across relogin); NPC talk order parity quest → achievement → store → banker (handleTalkToNPC; the m6 harness releases the foresting/ratinfestation claims before its forester-store leg); SQLite persistence (m11EnsureTables/m11PersistQuests/m11LoadQuests — restore at login, flush on disconnect); TESTMAP debug dispatcher `[46 {m11test}]` (setstage/setach/echo quest|ach|pending|drops); `M11_HERODMG` damage accelerator mirrors `M9_MOBDMG`.

## 5. Perf notes — TARGET (not yet in stub; CURRENT = per-send writes, no tick loop, no batching, no region-scoped broadcast yet)

20 Hz world tick, broadcast only deltas; batch each tick into one bulk `[...]` write per conn; region interest — only entities in client's surrounding regions (`List.Spawns/Positions`); compress Map once at startup (gzip + encodeURI bufSize, cached frame); single-writer DB goroutine, in-memory authoritative, flush dirty players every 5–15s + on disconnect; persist off hot path (never on tick); prepared statements + WAL readers; per-IP `MAX_CONNECTIONS`, per-conn msg/s + chat token buckets.

## 6. Gap closures (audit 2026-09-15, all TARGET unless noted)

- G1 commands (`internal/social/commands.go`, rank-gated): ~120 cases by group — player (/help /who /loc /ping /roll /time /pvpstatus /mgjoin); mod (/mute /unmute /kick /banlist /warn); admin (/spawn /give /level /teleport /goto /summon /heal /kill /set /reload /announce /shutdown); boss (/spawnboss `<key>` x y); teleports (/warp /town /return). Gate player<mod<admin; unknown → Chat error; log every admin invocation.
- G2 minigames (`internal/minigame/`, split): teamwar (lobby→queue→balanced teams→score→win) vs coursing (lobby→queue→bracket→heats→final) as separate types sharing a lobby/queue/instance interface; instance = isolated entity set + region scope; `minigame.json` drives sizes/rules; sub-ops Join/Leave/Queue/Start/Score/End.
- G3 areas (`internal/world/areas.go`): 7 impl files `camera/chest/dynamic/minigame/music/overlay/pvp.ts` — camera, music, pvp direct; chest≈event/loot, dynamic≈door/trigger, minigame≈event/instance, overlay≈safe (darkness/fog); spawn areas via spawn-queue hooks; each `{id, x, y, w, h, kind, data}` from world.json; enter/exit hooks fire music/pvp-flag/teleport/dialog; areas checked on every movement Step + teleport.
- G4 globals (`internal/world/globals.go`): lights + signs broadcast — static light table (x,y,radius,color) sent on region enter; sign text served on interact (object lookup → Chat/dialog frame); both read-only from map data, no persistence.
- G5 mob AI (`internal/combat/ai.go`): spawn-queue (staggered respawn timers from `spawns.json`) → roam (idle wander in leash radius) → chase (aggro range, target lock) → leash (max range → drop aggro) → return (walk home, full heal) → boss hooks (phase callbacks at HP thresholds, summon/adds, enrage).
- G6 pets (`internal/entities/pet.go`): follow (trail owner, teleport if >12 tiles), attack (mirror owner target, melee only), tame (item→chance roll→bind), timers (hunger tick, expire/dismiss, respawn cooldown); `PetData{owner}` required; never block movement.
- G7 loot (`internal/economy/loot.go`): 2-phase expiry — blink (visible warning last 10s via `Sync` flash) → destroy (despawn + free); chest respawn wiring (open→empty→timer→refill from `tables.json`, broadcast `Resource` state).
- G8 stores (`internal/economy/stores.go`): Buy (gold check→inventory add→snapshot decrement), Sell (item→gold credit), Select (tab switch); snapshot cache 20s TTL refreshed on first read after expiry; `stores.json` prices authoritative.
- G9 social (`internal/social/`): friends (add/remove/block/list, online notify) + guilds (create/invite/accept/kick/leave/rank promote-demote, persist `guilds`/`guild_members`) + relay frame (guild/global chat routed via hub when `HUB_ENABLED`, else local broadcast); Discord (`packages/common/api/discord.ts`): local-broadcast hook only (`onMessage`→global chat), full Discord bot bridge OOS.
- G10 REST + hub auth: REST port (env `API_PORT`, default off) serves `/players/:name /guilds /status` read-only JSON for tools; hub token auth (shared secret env `HUB_TOKEN`, bearer on hub socket, reject+log on mismatch, per-hub rate limit).
- G11 Sentry + admin scope: Sentry opt-in (env `SENTRY_DSN` empty = disabled; wrap tick/WS handlers, sample 0.1); admin web panel explicitly out-of-scope (no port, no UI; admin via in-game commands only).
- G12 E2E (M8): Cypress parity — boot stub, scripted WS client asserts Welcome/Map/Spawn/Ready/move/combat/chat flows mirror `packages/client` Cypress specs; Go harness (`go-server/e2e/`) runs same asserts headless in CI; M8 gates on both green.
- G13 map-build (`Tools` step): `tools map` export (`map_template.json`→`map.json`→`world.json`) is a required pre-build step (documented command, hash check); `meta.world_hash` stored at boot, compared on start, mismatch → refuse boot with explicit error.
- G14 hub (`internal/social/hub.go`): consolidates 11 TS hub files — server-list (register/heartbeat/deregister), cache (player-location routing table, TTL), mailer (offline messages persisted, delivered on login); single goroutine, token-guarded.
- G15 filter + i18n (`internal/social/filter.go`): profanity filter (wordlist from `packages/server/data/filter.json` if present, else builtin; mask `***`, mod-bypass, violation counter→mute); i18n English-only note (all server strings English; client localises; no translation tables ported).
- G16 inventory checklist rows (port sources): controllers (input→action dispatch per entity type) → `internal/net`; combat formulas (melee/range/magic + poison/burn/freeze/bleed ticks) → `internal/combat`; effects/projectiles tables → `internal/combat`; abilities (mana/cd/unlock) → `internal/skills`; equipment sets/bonuses → `internal/entities`; containers (inventory/bank slots) → `internal/persist`; skills XP curves (19 incl pseudo Chiseling/Smelting) → `internal/skills`; quests/achievements handlers → `internal/quests`; plugins (event hooks) → per-package `hooks.go`; formulas (shared math) → `internal/combat/formula.go`; network impl (53 frames) → `internal/net`; args/env parsing → `cmd/server`; common config + mongo legacy (read-only reference, SQLite is authority) → `internal/persist`.

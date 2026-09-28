# rpg-world-sim — Boot / Rendering / Network Evidence (2026-09-14)

## Environment
- `node -v` = v26.8.1; engines require `^18.14.1 || ^20.0.0`. No `nvm`/`fnm`/`volta`/`n` available — documented mismatch, proceeded best-effort.
- Created `.env` from `.env.defaults` (untracked, gitignored) with `ACCEPT_LICENSE=true`. Key values: `HOST=localhost PORT=9001 SKIP_DATABASE=true HUB_ENABLED=false CLIENT_REMOTE_HOST/PORT empty`.
- `yarn install` (yarn 4.0.0-rc.40): OK in ~1m38s with warnings (TS patch hunks YN0066, cache fetches). Link step built kaetram/cypress/esbuild/sharp/bufferutil/sentry.

## What booted
- **Client (Astro) BOOTED**: `yarn workspace @kaetram/client dev --port 9000 --host 127.0.0.1` → `astro v3.2.0 started in 881ms`, `Local http://127.0.0.1:9000/`, `lsof -i :9000` LISTEN (pid 75662). Log: `/tmp/kaetram-client.log`.
- **Server FAILED (expected uWS blocker)**: `yarn workspace @kaetram/server dev` → crash in `node_modules/uws/uws.js:3`: "This version of uWS.js supports only Node.js LTS versions 16, 18 and 20…" + `Cannot find module './uws_darwin_arm64_147.node'`. Nothing listening on :9001 (`lsof -i :9001` empty). Full log: `/tmp/kaetram-server.log`.
- **Tools map check (fallback)**: `yarn workspace @kaetram/tools map` runs via tsx, loads `.env`, then `ERROR File map/data/map.json could not be found.` Only `map_template.json` exists in `packages/tools/map/data/`. So map needs export/generation step; not attempted (would write files).

## Rendering (code evidence, no source edits)
- `packages/client/components/game.astro:1-12`: `#border` div wrapper > `#canvas` div > 8 canvases `#background #entities #entities-fore #foreground #entities-mask #cursor #overlay #text-canvas` (+ UI-only `#equipments-player-image-canvas:612`).
- `packages/client/src/renderer/renderer.ts:56-75`: queries all 8 canvas IDs, stores in `canvases[]`; contexts split tilemap vs entities (`entitiesContext`, `entitiesForeContext`, `entitiesMaskContext`, `overlayContext`, `textContext`, `cursorContext`); `camera.ts:12` queries `#border` div (not a canvas) for screen size.
- No screenshots possible headless; no renderer code modified.

## Network (code evidence)
- Transport: `packages/client/src/network/socket.ts:56-64`: `new WebSocket(ws://host:port)` (or wss if ssl), `message` → `receive(event.data)`; hub skipped when `!config.hub` (`getServer:33-34`); close code 1010 = server reject reason.
- Wire shape: `packages/common/network/packet.ts:25-33` `serialize()` → `[id, data]` or `[id, opcode, data]` (+ optional bufferSize). JSON string; client `receive` ignores non-`[` messages (`socket.ts:100`).
- IDs: `packets.ts` enum: Spawn=5, Movement=11, Animation=17 (0-indexed: Connected=0…). Opcodes `Movement`: Request/Started/Step/Stop/Move/Follow/Entity/Speed.
- `spawn.ts`: `[Packets.Spawn, EntityData]` where EntityData (`common/types/entity.d.ts:25-57`) = `{instance,type,key,name,x,y + movementSpeed?,hitPoints?,orientation?,count?,displayInfo?...}`; player/mob serialize variants.
- `movement.ts:7-16`: `[Packets.Movement, opcode, {instance,x?,y?,forced?,target?,orientation?,state?,movementSpeed?}]`.
- `animation.ts:7-11`: `[Packets.Animation, {instance, action: Modules.Actions, resourceInstance?}]`.

## Blockers / open risks
1. Node 26 vs engines 18/20 + uWS native has no `uws_darwin_arm64_147.node` → server cannot boot without Node 18/20 (via nvm/fnm/volta) or uWS rebuild. This is the hard blocker for live WS capture.
2. `map.json` absent → even with correct Node, server/world load may need `tools map` export first.
3. Client `build` (`astro check && tsc && astro build`) not run (slow); only `dev` boot verified. Typecheck evidence still open.
4. `.env` is local-only, untracked; `yarn install` mutated yarn cache/node_modules only. No commits made.

## Sprite+Animation harness (2026-09-15, read-only, no commits)
- Harness: `~/.config/opencode/.harness-memory/rpg-world-sim/viewer/` (`index.html`, `viewer.js`, `img/{player-base,crab,oak}.png` copies). No `packages/client/src` edits.
- PNGs (PIL+file): `player/base.png` 128x384 (32x32 cells -> 4 cols x 12 rows, matches 12-row default player layout); `mobs/crab.png` 256x320 (32x32 -> 8 cols x 10 rows, matches 10-row sprites.json override, NOT 9-row mob default); `trees/oak.png` 128x288 (64x96 -> 2 cols x 3 rows, matches idle/shake/exhausted).
- sprites.json: `player/base` {32,32,offsetX:-8}; `mobs/crab` {32,32,idleSpeed:300,off:-8,-8, 10 anims death r0(8)..idle_down r9(7)}; `trees/oak` {64,96,off:-24,-64}.
- Frame math (animation.ts:77-78, renderer.ts:716-726 + 871-872): `frame.x=index*width`, `frame.y=row*height`, `drawImage(img,fx,fy,w,h,offsetX,offsetY,w,h)`; loop resets at `index>=length-1`; speeds walk 120ms (character.ts:50), idle sprite.idleSpeed (250 default / crab 300, entity.ts:227); Left orientation -> `*_right` row + `translate+scale(-1,1)` flip (character.ts:247-252, renderer.ts:682-684).
- Verified: `node --check viewer.js` OK; node headless loop: player walk_down r3 cycles (0,0,96)->(96,96)->loop, crab walk_right r2 (0,64)->(160,64)->loop; `python3 -m http.server 8123` served index/viewer/3 PNGs all HTTP 200.
- Open viewer: `cd viewer && python3 -m http.server 8000`, open http://localhost:8000/index.html (or double-click index.html; file:// works). Shows player walk, flipped-Left, crab walk/idle, static oak + live frame log in console/page.
- Limits: no headless browser on this machine (no chromium/chrome) so no PNG screenshot; canvas visually verified only via manual open; harness duplicates (not imports) source math so future source drift needs re-check.

## Minimal server protocol SPEC (2026-09-15, read-only, no commits)
- Wrote `~/.config/opencode/.harness-memory/rpg-world-sim/SPEC.md`: wire shape (`[id,data]`/`[id,opcode,data]`,
  bulk arrays, `ws://host:port`), packet IDs (Handshake1/Welcome3/Map4/Spawn5/Equipment8/Sync10/Movement11/
  Teleport12/Despawn13/Animation16/Chat19/Resource59; Movement ops Move4/Follow5/Stop3/Speed7; Equip Batch0),
  boot `Connected→Handshake→Login→Welcome+Map→Spawn*→Ready`, EntityType/Orientation/Actions numbers,
  `key` prefix table (mobs/, player/, trees/, rocks/, bushes/, fishspots/, npcs/, items/, objects/, pets/),
  Movement→walk/idle, Animation→atk+shake, Equipment Batch→paperdoll, Resource Depleted→stump, + 3 copy-paste sends.
- Key files: `common/network/{packets,opcodes,modules,packet}.ts`, `impl/{spawn,movement,animation,equipment,
  despawn,teleport,sync,map,handshake,resource}.ts`, `types/entity.d.ts`, client `network/{socket,connection,
  messages}.ts`, `controllers/entities.ts`, `character.ts performAction/setAnimation`, `player.ts equip/load`.

## Reuse plan docs (2026-09-15, no source edits, no commits)
- Wrote `CLIENT-ASSETS.md` (612w: ~40-class table, type table, sprites 1308 entries/1316 files + 3 verified row-maths, 6 tilesheets, map 1152x1008 t16, 44 mp3s, lazy vs 16-preloaded, reusable vs Astro-UI split).
- Wrote `GO-SERVER-PLAN.md` (392w: Phase A stub Welcome/Map/Spawn + packet JSON + Go structs, Phase B movement/anim/equip, Phase C SQLite schema + WAL + periodic save, Phase D Tiled pipeline, perf notes).
- Wrote `WORLD-RECREATION.md` (264w: Tiled t16, region/MAP_DIVISION_SIZE, tools/map exporter to map.json + world.json, tilesheet GID constraints, new-sprite PNG+sprites.json+prefix rule).
- Verified: `git status --porcelain` clean (empty), `packages/*` untouched, all writes in harness-memory only.

## Reviewer Blocking fixes applied (2026-09-15, docs only, no commits)
- Fixed per review, all in ~/.config/opencode/.harness-memory/rpg-world-sim/ only; packages/* untouched.
- 1. MAP_DIVISION_SIZE=48: WORLD-RECREATION.md §2 rewritten (modules.ts:630, map.ts:75 sideLength, util.ts:280-281 getRegion); §1 + GO-SERVER-PLAN.md Phase D disambiguate tileSize 16px vs 48-tiles-per-region.
- 2. EntityType.Object=6 un-spawnable: SPEC.md key-modules note + prefix-table note, CLIENT-ASSETS.md entities row, GO-SERVER-PLAN.md Go block + Phase B, WORLD-RECREATION.md §5 (no case in entities.ts:78-163 → entities.ts:166 log.error).
- 3. Load model corrected: CLIENT-ASSETS.md §4 rewritten — SpritesController.load() (sprites.ts:21-40) instantiates all 1308 Sprite objects up front; PNG fetch lazy via Sprite.load() (sprite.ts:83-97, new Image + loaded gate, preload flag) + loadHurtSprite/loadSilhouetteSprite derivatives; §2 sprites row updated.
- 4. Map packet always [4,base64,bufSize]: SPEC.md §1 + table row + Welcome/Map paragraph, WORLD-RECREATION.md §3, GO-SERVER-PLAN.md Map stub (impl/map.ts:9-15 compress+getBufferSize, packet.ts:25-33 trailing push, connection.ts:264-280 atob→inflate→loadRegions).
- 5. Go structs fixed: GO-SERVER-PLAN.md Phase A rewritten — one json tag per field, omitempty on all optionals per types/entity.d.ts:25-57 (+ PetData owner required per pet.d.ts:3-5, ResourceEntityData.state per resource.d.ts:27-28, PlayerData per impl/player.ts:15-28).
- 6. Boot order corrected: SPEC.md §2 rewritten (S Connected → C Handshake{gVer} [connection.ts:186-191] → S Handshake{type:client} → C Login [connection.ts:199-241] → S Welcome+Map → C Ready [game.ts:202 in postLoad ← handleWelcome connection.ts:249-254] → S Spawn after Welcome); GO-SERVER-PLAN.md Phase A boot line matches.
- 7. Canvas IDs = 8 + border: CLIENT-ASSETS.md canvas row (§1) + §5 rewritten, NOTES.md rendering bullets above (game.astro:1-12, renderer.ts:56-75, camera.ts:12 #border div).
- Warnings (trivial): SPEC.md projectile (ownerInstance/targetInstance/hit, entities.ts:262-314) + pet (owner/movementSpeed, pet.d.ts) requirements; WeaponSkin→player/weapon/<key> (player.ts:239) in SPEC.md §3 + GO-SERVER-PLAN.md Phase B; Ready exact shape {regionsLoaded,userAgent} (game.ts:202) in SPEC.md table + GO-SERVER-PLAN.md Phases A/B.
- Verify: git status --porcelain in /Users/appfuxion/repo/rpg-world-sim = clean (empty, EXIT:0) on 2026-09-15; no commits made.

## Go WS stub — live interop check (2026-09-15, no repo edits, no commits)
- Stub: `~/.config/opencode/.harness-memory/rpg-world-sim/go-stub/` (`go.mod` go 1.21 + gorilla/websocket v1.5.3, `main.go`, `packets.go`). Listens `ws://127.0.0.1:9001`.
- `go mod tidy && go vet ./... && go build ./...` all green; `gofmt -l` clean. `git status --porcelain` in `/Users/appfuxion/repo/rpg-world-sim` = empty.
- Boot verified with scripted WS client: S `[[0,null]]` → C `[1,{gVer}]` → S `[[1,{type:client,instance:p1,serverTime}]]` → C `[2,{opcode:2}]` → S Welcome(PlayerData p1/base@100,100) + Map `[4,"eJyqrgUEAAD//wF1APk=",2]` (zlib-inflate → `{}`, bufSize 2 matches) + Spawns p2/player-base, m1/mobs-rat, t1/trees-oak(state 0). No type 6 sent. EntityData uses one json tag/field + omitempty on optionals.
- Manual test: `cd go-stub && go run .`, then client `.env` → `HOST=127.0.0.1 PORT=9001` (HUB empty/disabled), `yarn workspace @kaetram/client dev --port 9000 --host 127.0.0.1`, open http://127.0.0.1:9000, watch stub stdout RX/TX.

## Go WS stub — Reviewer Blocking + Warnings fixes (2026-09-15, no repo edits, no commits)
- Boot: `Login(2)` → Welcome + Map only; `Spawn*` moved to `Ready(9)` handler exclusively (`main.go`).
- `packets.go`: `EntityData` += `enchantments` (`Enchantments`, item.d.ts:5-7) + `hit` (`HitData`, info.d.ts:1-9); `PlayerData`: required `orientation int` (always emitted, shadows embedded optional) + `experience/nextExperience/prevExperience` omitempty (impl/player.ts:15-28).
- Named consts: full `packets.ts` enum `PacketConnected=0…PacketAdminSync=60` (`PacketLogin=2`, `PacketReady=9` etc); no magic numbers in `main.go`.
- Map: `compress/zlib` (zlib format, matches pako `inflate` which handles zlib/gzip) + `bufSize` via `bufferSize()` emulating `Utils.getBufferSize` encodeURI length (util/utils.ts:257-258); Handshake reply += `serverId:1`.
- Inbound: accepts bulk `[[id,data]]` as well as single `[id,data]` (probe outer[0], fan-out to frame loop).
- Verify: `go vet ./... && go build ./...` green, `gofmt -l .` empty; scripted WS check PASS — after Login ids `[0,1,3,4]` (no Spawn), Map gunzip → `{}` bufSize 2, Welcome has `orientation:1`, Handshake has `serverId:1`, bulk `[[9,…]]` Ready → 3 Spawns (no type 6). `git status --porcelain` in `/Users/appfuxion/repo/rpg-world-sim` = empty.

## Kaetram "Couldn't connect to localhost:9001" diagnosis (2026-09-15, no repo edits, no commits)
- Stub running: N (dead; old `/tmp/stub.pid`=50512 gone; `ps aux | grep go-stub` empty; `lsof -i :9001` empty). Port 9001 LISTEN: N. `curl -i http://127.0.0.1:9001/` → exit 7 connection refused (both 127.0.0.1 and localhost). Client :9000 OK (node pid 75662 LISTEN, curl 200).
- .env: `HOST='localhost' PORT=9001 SSL=false HUB_ENABLED=false CLIENT_REMOTE_HOST/PORT empty`. Client WS URL: `astro.config.ts:23-31` → host=localhost port=9001; `socket.ts:56` → `ws://localhost:9001`; error text `Couldn't connect to ${host}:${port}` (socket.ts:148, DEV only) matches screenshot.
- Host mismatch? No. `localhost` resolves to `::1` + `127.0.0.1`; stub binds `127.0.0.1:9001` (main.go:25, IPv4-only) but browsers fall back to 127.0.0.1 — fine once stub is up. Prior `/tmp/stub.log` proves full handshake worked (TX Connected/Handshake/Welcome+Map/Spawns, RX Handshake/Login/Ready).
- Root cause: stub down (not host mismatch, not wrong build) + real Node server can't substitute: `/tmp/kaetram-server.log` → uWS crash `Cannot find module './uws_darwin_arm64_147.node'` (Node 26 vs required 18/20), so :9001 stays closed.
- Fix applied: `cd ~/.config/opencode/.harness-memory/rpg-world-sim/go-stub && nohup go run . > /tmp/go-stub.log 2>&1 &` → parent PID 74892, listener PID 74916, `lsof -i :9001` LISTEN, `curl` → `HTTP/1.1 400 Bad Request` + `Sec-Websocket-Version: 13` (port open, WS upgrade required). Reload http://127.0.0.1:9000 to verify; watch `tail -f /tmp/go-stub.log` for RX handshake. If port ever busy: `lsof -ti :9001 | xargs kill` then restart. `packages/*` untouched, `git status` clean.

## Go WS stub — real map tiles around spawn (2026-09-15, no repo edits, no commits)
- Was: Map sent `{}` → client rendered black void. Now: stub loads `packages/server/data/map/world.json` (1152x1008, 1073999 tiles, 3232 collisions, 56 objects) once at startup and serves the 9 regions around spawn (100,100) = region 50 + neighbours 25,26,27,49,51,73,74,75 (MAP_DIVISION_SIZE=48, sideLength=24, `surroundingRegions` mirrors regions.ts:711-766).
- `getRegionData`/`buildTile` (main.go) mirror regions.ts:501-563 + map.ts:353-361: index=y*width+x, number|[n..] layered, skip 0/empty, keep data>=1, unflip Tiled flags (>0x20000000), flag c/o/cur from collisions/objects/cursors; walkable tiles emit `c:false`. Framing unchanged [4,base64gzip,bufSize], gzip + encodeURI byte length, frame cached via sync.Once. `RegionTile` struct added to packets.go.
- Spawns spread onto walkable tiles verified vs collisions array: hero (100,100)=[3907,9787], guest (102,98)=3394, rat (104,104)=3907, oak (106,100)=[3906,5478]. NOTE spec coords (102,100)=9975 and (104,102)=[3969,10070] collide, so guest/rat shifted to nearest walkable spread spots.
- Verify: `gofmt -l` clean, `go vet ./... && go build ./...` green; scripted WS check PASS — Map gunzip = 772680 bytes = bufSize, 9 regions (25:1776 26:1776 27:1776 49/50/51/73/74/75:2304), tile (100,100) in region 50 `{"x":100,"y":100,"data":[3907,9787],"c":false}`; Ready → 3 spread Spawns. Restarted: `lsof -ti :9001 | xargs kill; nohup go run . > /tmp/go-stub.log 2>&1 &` (`world loaded` + `map frame: 9 regions` in log). `git status --porcelain` in repo = empty.

## Go WS stub — walk-through-trees fix (2026-09-15, no repo edits, no commits)
- Collision semantics (read-only in repo): server `regions.ts:610-646 buildTile` sets `c:true` iff a layer (unflipped, `map.ts:145-147/353-361`) is in `objects` (also `o:true`) or `collisions`; walkable tiles OMIT `c`. `high` is never consulted for `c` (render layer only). Client `map.ts:106` defaults grid to 1 (colliding); `loadRegionTileData:168-172` sets 1 on `tile.c`, clears to 0 on `!tile.c` — so stub's explicit `c:false` (always-emitted `RegionTile.C`) equals server's omitted-`c`. Verified live: 19152 tiles, 9094 with `c:true`; spawns `c:false` (hero 100,100 / guest 102,98 / rat 104,104 / oak 105,100), each adjacent to a `c:true` tile (101,100 / 102,99 / 103,104 / 104,100); grass (99,100) `c:false`, known wall (102,100)=9975 `c:true`.
- Static vs entity trees: static map trees are colliding TILES (`c:true`, client `player/handler.ts:56` refuses them itself). Entity trees (oak t1) stand on a WALKABLE tile — real server blocks via entity grid (`map.ts:246-254` hasEntityAt+isResource) enforced by `setPosition/verifyCollision` (player.ts:1579-1580,693-716, teleport-back). Stub mirrored this: `resourceEntities` (t1 only — players/mobs never block, per server), `tileBlocked` (OOB/empty/collisions+objects), `blocked()`, per-conn `session` pos. `Movement Request` onto a blocked tile (untargeted) → `[11,3,{instance:p1}]` Stop + `[12,{instance,x,y}]` Teleport-back (client connection.ts:461-463,478-501); targeted resource Requests pass (chop-approach via ignores, handler.ts:83); `Movement Step` with blocked next tile → Stop+Teleport; grass silent. C→S movement is `[11,{opcode,…}]` (opcode inside data), S→C is `[11,opcode,{…}]` — new `clientMovement/serverMovement/teleportData` + `pktOp` in packets.go; opcodes Request0/Started1/Step2/Stop3 (opcodes.ts).
- Oak moved (106,100)→(105,100) (`[3906,5478]`→`[3520,9782,5477]`, both walkable) so all four spawns sit adjacent to a colliding tile; comment updated.
- Why canopy-over-head is CORRECT: `renderer.ts:751-801 drawTree` draws the canopy on `entitiesForeContext` (above player) and the stump on `entitiesContext` with `destination-over` (below player) — head-peeking under leaves is the intended split, not a bug. The bug was movement only.
- Verify: `gofmt -l` clean, `go vet ./... && go build ./...` green; restarted stub (`lsof -ti :9001 | xargs kill; nohup go run . >/tmp/go-stub.log 2>&1 &`, pid 7402 LISTEN); scripted Go WS check (`/tmp/wcheck/main.go`, own go.mod, cached gorilla) ALL PASS: 11 tile asserts, 3 spawns no-type-6, oak-Request→Stop+Teleport, targeted-Request silent, colliding-Step→Stop+Teleport, grass Request+Step silent. Quirk found: after one read-timeout, gorilla/Go1.22 reads on that conn always time out (stub log proves it replied) — checker uses one fresh conn per phase. `git status --porcelain` in `/Users/appfuxion/repo/rpg-world-sim` = empty.

## CLASS-DESIGN (2026-09-15, no repo edits, no commits)
- Wrote `CLASS-DESIGN.md` (992w <1200): 8 classes (Warrior, Knight-Paladin, Rogue-Assassin, Archer-Ranger, Mage-Elementalist, Necro-Curse capped, Woodsman-Gatherer, Crafter-Smith) with 3+ weapon examples + levels each, armor looks, 2+1 skill triples, signature effect+projectile, roles.
- Skill trees §2: shared tier table 1/5/10/15/20/25/30 (weapon tier, armor tier, spell/ability of 8, recipe of 94/7 tables); metal ladder L5 bronze→L25-30 amethyst+, staff ladder aqua L1→fire L25 (curse L35 excluded).
- Spells §3: fire/ice/poison/boulder/terror/drain/heal/buffs mapped to 31 effects + 32 projectiles, mana 10-40 / cd 3-20s.
- Mob curve §4: existing 24/148 ≤30 bands (1-10:4, 11-20:12, 21-30:8) per 5-level split + 6 downscaled elites reusing 31+ art (stat-nerf HP/DMG x0.35).
- Balance §5: magic range 9 vs archer 8, 9-arrow consumption loop, mana costs, curse-staff exclusion note.
- Verify: `wc -w CLASS-DESIGN.md` = 992; `git status --porcelain` in repo = empty.

## CLASS-DESIGN Blocking fixes applied (2026-09-15, docs only, no commits)
- Fixed per review, all in ~/.config/opencode/.harness-memory/rpg-world-sim/CLASS-DESIGN.md only; packages/* untouched, no commits.
- 1. Real items only (every pool key grepped `rg '"<key>":' packages/server/data/items.json`): spears bronze L5/iron L10/cobalt L15/gold L20; swords copper/tin starter (no level) -> bronze L5 -> iron L10 -> nisoc L15 -> gold L20 (+cinnabar L20 alt, icesword L17); staves aqua starter (no level, manaCost 2, purplebolt) / lightning L10 (4, yellowlightning) / nature L17 (6, greenbolt) / fire L25 (7, fireball), curse L35 ("Ice Staff", terror) excluded. Bows woodenbow (=boweggplant)/bowbamboo/bowpine +14 more, ALL level-less -> cosmetic, no ladder. Zero `*dagger*` keys (Rogue -> swords + adeptsrapier/royalrapier L25); no Bronze/Iron/Gold Bow tiers, no Copper/Bronze/Amethyst daggers, no Iron Rapier, no Bronze Hammer (Crafter -> real pickaxes level-less + smithshammer L25); doc spelling `magichamberge` unmatched, real near-miss `magichamberge` (L20, no projectile) noted off-ladder and excluded.
- 2. Spells rewritten to real `Effects` enum (Fireball/Iceball/Poisonball/Boulder/Terror+TerrorStatus/Healing/Stun/Burning/Freezing/Bleed + Accuracy/Strength/Defense/Magic/Archery Buff+SuperBuff) + real projectileNames (fireball/iceball/terror/yellowlightning/greenbolt/purplebolt/arrow/firearrow/poisonarrow/lightningarrow/icearrow/nisocarrow/cinnabararrow/pythararrow/iboarrow); real costs (staff manaCost 2/4/6/7, abilities mana 15-21/cd 60s); kit mapping marked NEW PROPOSAL.
- 3. Counts corrected: Skills 19 incl pseudo Chiseling/Smelting; Effects enum 30 incl None (29 usable) per grep -- SPEC SAID 31, VERIFIED 30, doc uses 30 (flagged); DamageStyles parenthetical fixed to real None/Crush/Slash/Stab/Magic/Archery; downscaled elites + ability triples labeled NEW PROPOSAL, not implemented.
- Verify: `wc -w CLASS-DESIGN.md`; `git status --porcelain` in /Users/appfuxion/repo/rpg-world-sim = clean (empty); no commits made.

## CLASS-DESIGN V2 (2026-09-15, no repo edits, no commits)
- Wrote `CLASS-DESIGN-V2.md` (858w <1400): 21 classes 1-line each (role / 2-3 real weapon keys+levels / 1 real armor set / 3 unique skills Lv1 core-Lv10 spec-Lv20 ult with real Effect enum + projectileName + mana 2-7/cd 8-60s). Cap 20: `firestaff` L25 + `cursestaff` L35 cut, Mage tops at `naturestaff` L17; bows cosmetic (17 keys gateless); no daggers (swords instead). Pet/avatar tricks 1 line each: snek/whitebear/wolf/spider reskins, skeletonhelm+skin, turret=stationary pet, clone=pet+Running/teleport, puddle=lava effectEntity. Shared beats table 1/5/10/15/20 (+17 off-tier spike). 16 real mobs <=20 in 4 bands; 21-30 cut with Lesser-downscale note (NEW PROPOSAL). Balance para: range 9 vs 8, arrows consumable, mana vs positioning.
- Verify: `wc -w CLASS-DESIGN-V2.md` = 858; `git status --porcelain` in repo = empty.

## COMBAT-SKILLS doc (2026-09-15, no repo edits, no commits)
- Wrote `~/.config/opencode/.harness-memory/rpg-world-sim/COMBAT-SKILLS.md` (1348w, <1500): 21 classes x 3 COMBAT skills = 63 skills, Lv1/10/20, mana 2-7 (cap 20), cd 8-20s (cap 20s). No professions.
- Each skill: name, level, mana/cd, real `Modules.Effects` enum + `projectiles/*` name + `mobs/*` pet / `lava` effectentity / buff via `drawEffects` (character.ts), 1-line anim (overlay / projectile / trick).
- Assets used only from allowed lists: Effects Fireball/Iceball/Poisonball/Boulder/Terror/TerrorStatus/Healing/Stun/Burning/Freezing/Bleed/Critical/Buffs/SuperBuffs/ThickSkin/DualistsMark/Running/Invincible/FirePotion; projs fireball/iceball/poisonball/boulder/terror/purplebolt/yellowlightning/greenbolt/arrow/firearrow/poisonarrow/lightningarrow/iboarrow/bloodball; pets snek/whitebear/wolf/spider/skeleton/ghost/vulture (mobs/ keys verified in sprites.json); zone lava (effectentities.json duration 5000).
- Necro Lv20 Summon Dead: TerrorStatus + terror + pet:skeleton, terror2 rise FX, skeleton pet 20s HP50 atk5 follows owner + attacks target.
- Verify: `wc -w` 1348, 63 bullets, 21 sections; `git status --porcelain` in repo = empty.

## COMBAT-SKILLS Blocking fixes applied (2026-09-15, docs only, no commits)
- Fixed per review, all in ~/.config/opencode/.harness-memory/rpg-world-sim/COMBAT-SKILLS.md only; packages/* untouched, no commits.
- 1. Invisible overlays replaced with substitutes verified in BOTH `packages/client/src/entity/character/character.ts:58-171` effects map AND `packages/client/data/sprites.json`: visible set = Critical, Stun, Burning (burn), Freezing (freeze), Healing (heal), Fireball/Iceball/Poisonball, Boulder, Strength/Defense/Magic/Archery Buffs (+Super), Terror (terror)/TerrorStatus (terror2). Invisible (map entry without sprite, or sprite without map entry): Bleed (sprite `effects/bleed` exists, no map entry → `drawEffects` skips via `getEffect` undefined), DualistsMark/ThickSkin/Invincible/Running/FirePotion/SnowPotion (enum-only, no map entry), AccuracyBuff/AccuracySuperBuff (map entries, no `effects/accuracy`/`effects/accuracysuper` sprite ids).
- 2. Substitutions (intent kept): Aegis Invincible→DefenseSuperBuff; Bloodlust Bleed→Burning; Feint/Jab AccuracyBuff→Critical; Deathmark/Assassinate DualistsMark→Critical+Terror; Impale Bleed→Critical; Sentinel/Barkskin/Deploy ThickSkin→DefenseBuff; Reap Bleed→Burning; Execute AccuracySuperBuff→StrengthSuperBuff; Thorn Bleed→Poisonball; Hiss AccuracyBuff→Terror; Venom Bleed→Poisonball; Immolate FirePotion→Burning; Track/Blink Running→movement-only, no overlay (speed is server state, Teleport/Movement packets).
- 3. GO-NEW-LOGIC (no client change, pets already render+move; Go authoritative, not bound by Node pet.ts cosmetic-only): Summon Dead skeleton follow+attack, turret Deploy/Overclock/Barrage spawn+fire, lava zones Tempest/Ashfall DoT — all via Spawn/Movement/Combat packets + Go timers (Heal/Combat packets); `lava` effectentity verified in `packages/server/data/effectentities.json`, pets verified as `mobs/skeleton|wolf|whitebear|spider|snek|ghost|vulture` in sprites.json.

## GO-SERVER-PLAN full-recreation gaps closed (2026-09-15, docs only)
- Appended §6 (G1–G16) to `go-server/docs/GO-SERVER-PLAN.md`: commands table, minigame split, 7 area types, globals, mob AI, pets, loot 2-phase, stores, social+relay, REST+hub auth, Sentry/admin scope, E2E+M8, map-build+world_hash, hub consolidation, filter+i18n, inventory checklist rows. 1111→1721 words (<2000).
- Mirrored identically to `~/.config/opencode/.harness-memory/rpg-world-sim/GO-SERVER-PLAN.md` (`cmp` clean). `packages/*` untouched. `go vet ./...` green in `go-server/`.
- Verify: `git status --porcelain` in /Users/appfuxion/repo/rpg-world-sim = clean (empty); no commits made.

## GO-SERVER-PLAN final coverage nits (2026-09-15, docs only)
- GAP-1 Discord: G9 extended — `packages/common/api/discord.ts` local-broadcast hook only (`onMessage`→global chat), full Discord bot bridge OOS.
- GAP-2 sprites.json: §1 inventory — marked client-generated/optional (not server-authoritative, no port); no `packages/server/data/sprites.json` exists.
- GAP-3 areas: G3 rewritten to 7 impl files `camera/chest/dynamic/minigame/music/overlay/pvp.ts` with aliases (chest≈event/loot, dynamic≈door/trigger, minigame≈event/instance, overlay≈safe, spawn via spawn-queue hooks).
- Mirrored identically to `~/.config/opencode/.harness-memory/rpg-world-sim/GO-SERVER-PLAN.md` (`cmp` clean). `packages/*` untouched. `go vet ./...` green in `go-server/` (docs only).

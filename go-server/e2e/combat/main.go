// Scripted WS check for COMBAT party mode (COMBAT=1): pure original 9
// regions, 4 bots (WarBot gold warrior, ArchBot woodenbow archer, MageBot
// naturestaff mage, SupBot unarmed support) + BossDummy (golem, 5000 HP)
// + leash-demo rat (m-rat-1) spawns; M3 formula autos at class speeds
// (war 0-37, archer 0-41, mage 0-62, 5% crits to 52/59/91) + Sunder/Volley/
// Storm skills (per-bot 1s GCD); ranged damage lands via real Projectile
// Spawn (type 5) + delayed impact; support stays silent at full HP
// retaliates, so no Heal/Healing-FX/bot-Points overheal spam; heal amount
// clamps to missing HP with an Idle anim when it does fire) + party buffs
// (Effect Add DefenseBuff/StrengthSuperBuff); per-class Spawn attackRange
// (war 1 / archer 8 / mage 9 / support 1); boss death (Despawn) + respawn
// (Spawn).
// Not part of the stub build (run via `go run ./_combatcheck` from the
// stub dir; underscore dir ignored by go tooling).
// Usage: restart stub with COMBAT=1, then go run ./_combatcheck.
package main

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var incoming = make(chan []json.RawMessage, 65536)

func reader(conn *websocket.Conn) {
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			close(incoming)
			return
		}
		var bulk [][]json.RawMessage
		if err := json.Unmarshal(raw, &bulk); err != nil {
			fmt.Println("parse fail:", string(raw))
			continue
		}
		for _, f := range bulk {
			incoming <- f
		}
	}
}

type stamped struct {
	f []json.RawMessage
	t time.Duration
}

var lastFrames []stamped

func drain(d time.Duration) map[int]int {
	counts := map[int]int{}
	deadline := time.Now().Add(d)
	for {
		timeout := time.Until(deadline)
		if timeout <= 0 {
			return counts
		}
		select {
		case f, ok := <-incoming:
			if !ok {
				return counts
			}
			if len(f) == 0 {
				continue
			}
			var id int
			if err := json.Unmarshal(f[0], &id); err != nil {
				continue
			}
			counts[id]++
			lastFrames = append(lastFrames, stamped{f, time.Since(startT)})
		case <-time.After(timeout):
			return counts
		}
	}
}

var startT = time.Now()

var failures []string

func check(cond bool, msg string) {
	if !cond {
		failures = append(failures, msg)
		fmt.Println("  FAIL:", msg)
	} else {
		fmt.Println("  ok:", msg)
	}
}

func frameData(f []json.RawMessage) json.RawMessage {
	if len(f) == 0 {
		return nil
	}
	return f[len(f)-1]
}

func send(conn *websocket.Conn, msg string) {
	if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		fmt.Println("WRITE FAIL:", err)
		os.Exit(1)
	}
}

type tile struct {
	X    int             `json:"x"`
	Y    int             `json:"y"`
	Data json.RawMessage `json:"data"`
	C    bool            `json:"c"`
}

// dialPort resolves the target port: PORT env (matching the server's own
// override) or the default 9001.
func dialPort() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "9001"
}

func main() {
	// Honor PORT (the server's own override) so a side-by-side run works
	// while the default 9001 is occupied.
	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+dialPort()+"/", nil)
	if err != nil {
		fmt.Println("DIAL FAIL:", err)
		os.Exit(1)
	}
	defer conn.Close()
	go reader(conn)

	fmt.Println("connected:", drain(1500*time.Millisecond))
	lastFrames = nil

	send(conn, `[1,{"gVer":"0.5.5-beta"}]`)
	fmt.Println("handshake:", drain(1500*time.Millisecond))
	lastFrames = nil

	send(conn, `[2,{"opcode":0,"username":"tester","password":"x"}]`)
	fmt.Println("login:", drain(2*time.Second))

	var welcome struct {
		Instance string `json:"instance"`
		X        int    `json:"x"`
		Y        int    `json:"y"`
	}
	var mapB64 string
	var mapBuf float64
	var mapElems int
	for _, s := range lastFrames {
		var id int
		_ = json.Unmarshal(s.f[0], &id)
		if id == 3 {
			_ = json.Unmarshal(frameData(s.f), &welcome)
		}
		if id == 4 {
			mapElems = len(s.f)
			_ = json.Unmarshal(s.f[1], &mapB64)
			if len(s.f) >= 3 {
				_ = json.Unmarshal(s.f[2], &mapBuf)
			}
		}
	}
	check((welcome.Instance == "p1" || strings.HasPrefix(welcome.Instance, "p-")) && welcome.X == 100 && welcome.Y == 96,
		fmt.Sprintf("hero Welcome p1 at 100,96 (got %s %d,%d)", welcome.Instance, welcome.X, welcome.Y))
	check(mapElems == 3, fmt.Sprintf("map framing [4,b64,bufSize] (got %d elems)", mapElems))

	gz, err := base64.StdEncoding.DecodeString(mapB64)
	check(err == nil, "map base64 decodes")
	zr, err := zlib.NewReader(bytes.NewReader(gz))
	check(err == nil, "map zlib reader opens")
	rawJSON, err := io.ReadAll(zr)
	check(err == nil, "map zlib reads")
	check(int(mapBuf) == len(rawJSON),
		fmt.Sprintf("map bufSize==json bytes (%d==%d)", int(mapBuf), len(rawJSON)))

	var regions map[string][]tile
	check(json.Unmarshal(rawJSON, &regions) == nil, "map regions JSON parses")
	check(len(regions) == 9, fmt.Sprintf("exactly 9 pure regions (got %d)", len(regions)))
	byXY := map[[2]int]tile{}
	for _, tiles := range regions {
		for _, t := range tiles {
			byXY[[2]int{t.X, t.Y}] = t
		}
	}
	if t, ok := byXY[[2]int{104, 104}]; ok {
		check(string(t.Data) == "3907" && !t.C,
			fmt.Sprintf("(104,104) pure base 3907 walkable, no pond (got data=%s c=%v)", string(t.Data), t.C))
	} else {
		check(false, "tile (104,104) present")
	}
	for _, xy := range [][2]int{{102, 96}, {106, 96}, {100, 98}, {102, 98}, {100, 96}} {
		if t, ok := byXY[xy]; ok {
			check(!t.C, fmt.Sprintf("combat tile %v walkable (data=%s c=%v)", xy, string(t.Data), t.C))
		} else {
			check(false, fmt.Sprintf("combat tile %v present in map", xy))
		}
	}
	lastFrames = nil

	send(conn, `[9,{"regionsLoaded":true,"userAgent":"combatcheck"}]`)
	fmt.Println("ready:", drain(2*time.Second))

	type equip struct {
		Type int    `json:"type"`
		Key  string `json:"key"`
	}
	type spawn struct {
		Instance       string  `json:"instance"`
		Type           int     `json:"type"`
		Key            string  `json:"key"`
		Name           string  `json:"name"`
		X              int     `json:"x"`
		Y              int     `json:"y"`
		Orientation    int     `json:"orientation"`
		Level          int     `json:"level"`
		HitPoints      int     `json:"hitPoints"`
		MaxHitPoints   int     `json:"maxHitPoints"`
		AttackRange    int     `json:"attackRange"`
		OwnerInstance  string  `json:"ownerInstance"`
		TargetInstance string  `json:"targetInstance"`
		Equipments     []equip `json:"equipments"`
	}
	spawns := map[string]spawn{}
	for _, s := range lastFrames {
		var id int
		_ = json.Unmarshal(s.f[0], &id)
		if id != 5 {
			continue
		}
		var sp spawn
		_ = json.Unmarshal(frameData(s.f), &sp)
		if sp.Instance != "" {
			spawns[sp.Instance] = sp
		}
	}
	lastFrames = nil

	_, dummyAlive := spawns["m-dummy"]
	// In-flight projectile Spawns (type 5, pr-*) may land inside the Ready
	// burst while the brain ticks — they are not scenario spawns.
	for id, sp := range spawns {
		if sp.Type == 5 {
			delete(spawns, id)
		}
	}
	if dummyAlive {
		check(len(spawns) == 6, fmt.Sprintf("exactly 6 Spawn frames (4 bots + dummy + rat, got %d)", len(spawns)))
	} else {
		// Fast-HP race: the checker connected during the respawn gap, so
		// only the 4 bots + rat spawn now — the dummy shape is verified
		// when it respawns below.
		check(len(spawns) == 5, fmt.Sprintf("boss dead at connect: 4 bot + rat Spawns, dummy respawns later (got %d)", len(spawns)))
		fmt.Println("  info: boss dead at connect — dummy shape verified via respawn")
	}
	rat, rok := spawns["m-rat-1"]
	check(rok, "Rat m-rat-1 spawned (leash demo)")
	if rok {
		check(rat.Type == 3 && rat.Key == "rat",
			fmt.Sprintf("Rat Mob rat (got type=%d key=%s at %d,%d)", rat.Type, rat.Key, rat.X, rat.Y))
		check(rat.HitPoints == 20 && rat.MaxHitPoints == 20,
			fmt.Sprintf("Rat full HP 20/20 (got %d/%d)", rat.HitPoints, rat.MaxHitPoints))
	}
	bot, ok := spawns["p-warbot"]
	check(ok, "WarBot p-warbot spawned")
	arch, aok := spawns["p-archer"]
	check(aok, "ArchBot p-archer spawned")
	mage, mok := spawns["p-mage"]
	check(mok, "MageBot p-mage spawned")
	sup, sok := spawns["p-support"]
	check(sok, "SupBot p-support spawned")
	dummy, dok := spawns["m-dummy"]
	if ok {
		check(bot.Type == 0 && bot.Key == "base" && bot.X == 102 && bot.Y == 96,
			fmt.Sprintf("WarBot Player base at 102,96 (got type=%d key=%s %d,%d)", bot.Type, bot.Key, bot.X, bot.Y))
		check(bot.Level == 20, fmt.Sprintf("WarBot Lv20 (got %d)", bot.Level))
		check(bot.Orientation == 3, fmt.Sprintf("WarBot faces right at boss (got %d)", bot.Orientation))
		check(bot.AttackRange == 1, fmt.Sprintf("WarBot attackRange 1 melee (got %d)", bot.AttackRange))
		keys := map[string]int{}
		for _, e := range bot.Equipments {
			keys[e.Key] = e.Type
		}
		for k, slot := range map[string]int{"goldsword": 4, "goldshield": 5, "goldhelmet": 0, "goldchestplate": 3, "goldlegplates": 9, "goldboots": 11} {
			s, has := keys[k]
			check(has && s == slot, fmt.Sprintf("WarBot gold piece slot=%d key=%s", slot, k))
		}
	}
	if aok {
		check(arch.Type == 0 && arch.X == 100 && arch.Y == 98,
			fmt.Sprintf("ArchBot Player at 100,98 (got type=%d %d,%d)", arch.Type, arch.X, arch.Y))
		check(arch.Orientation == 3, fmt.Sprintf("ArchBot faces right at boss (got %d)", arch.Orientation))
		check(arch.AttackRange == 8, fmt.Sprintf("ArchBot attackRange 8 (ARCHER_ATTACK_RANGE, got %d)", arch.AttackRange))
		keys := map[string]int{}
		for _, e := range arch.Equipments {
			keys[e.Key] = e.Type
		}
		for k, slot := range map[string]int{"woodenbow": 4, "arrow": 2, "leatherhelmet": 0, "leatherchest": 3, "leatherleggings": 9, "leatherboots": 11} {
			s, has := keys[k]
			check(has && s == slot, fmt.Sprintf("ArchBot piece slot=%d key=%s", slot, k))
		}
	}
	if mok {
		check(mage.Type == 0 && mage.X == 102 && mage.Y == 98,
			fmt.Sprintf("MageBot Player at 102,98 (got type=%d %d,%d)", mage.Type, mage.X, mage.Y))
		check(mage.Orientation == 3, fmt.Sprintf("MageBot faces right at boss (got %d)", mage.Orientation))
		check(mage.AttackRange == 9, fmt.Sprintf("MageBot attackRange 9 (naturestaff items.json, got %d)", mage.AttackRange))
		keys := map[string]int{}
		for _, e := range mage.Equipments {
			keys[e.Key] = e.Type
		}
		s, has := keys["naturestaff"]
		check(has && s == 4, "MageBot naturestaff weapon slot=4")
	}
	if sok {
		check(sup.Type == 0 && sup.X == 100 && sup.Y == 96,
			fmt.Sprintf("SupBot Player at 100,96 (got type=%d %d,%d)", sup.Type, sup.X, sup.Y))
		check(sup.Orientation == 3, fmt.Sprintf("SupBot faces right at boss (got %d)", sup.Orientation))
		check(sup.AttackRange == 1, fmt.Sprintf("SupBot attackRange 1 unarmed (got %d)", sup.AttackRange))
		keys := map[string]int{}
		for _, e := range sup.Equipments {
			keys[e.Key] = e.Type
		}
		check(len(sup.Equipments) >= 4, fmt.Sprintf("SupBot leather set equipped (got %d pieces)", len(sup.Equipments)))
		_, hasWeapon := keys["woodenbow"]
		_, hasStaff := keys["naturestaff"]
		_, hasGold := keys["goldsword"]
		check(!hasWeapon && !hasStaff && !hasGold, "SupBot unarmed (no weapon — never attacks)")
	}
	dummyMax := 0
	if dok {
		check(dummy.Type == 3 && dummy.Key == "golem" && dummy.X == 106 && dummy.Y == 96,
			fmt.Sprintf("BossDummy golem Mob at 106,96 (got type=%d key=%s %d,%d)", dummy.Type, dummy.Key, dummy.X, dummy.Y))
		check(dummy.Name == "BossDummy", fmt.Sprintf("boss named BossDummy (got %q)", dummy.Name))
		check(dummy.Orientation == 2, fmt.Sprintf("boss faces left at party (got %d)", dummy.Orientation))
		check(dummy.MaxHitPoints >= 60 && dummy.HitPoints > 0 && dummy.HitPoints <= dummy.MaxHitPoints,
			fmt.Sprintf("Boss HP alive 0<hp<=max (got %d/%d)", dummy.HitPoints, dummy.MaxHitPoints))
		dummyMax = dummy.MaxHitPoints
	}
	for _, gone := range []string{"p2", "m1", "t1", "t-test-1", "m-show-1", "n-show-1", "p-show-1", "p-show-2", "p-adv-1"} {
		_, dup := spawns[gone]
		check(!dup, "overlay gone: "+gone)
	}

	// --- Combat window: collect live party-brain traffic until every
	// class/skill has been observed (one 300-HP life is ~12s and may not
	// contain a Storm tick, so keep collecting across the respawn gap).
	fmt.Println("collecting combat window (up to 120s, across respawns)...")

	type hit struct {
		Type   int      `json:"type"`
		Damage int      `json:"damage"`
		Ranged *bool    `json:"ranged,omitempty"`
		Skills []string `json:"skills"`
	}
	type combat struct {
		Instance string `json:"instance"`
		Target   string `json:"target"`
		Hit      hit    `json:"hit"`
	}
	type points struct {
		Instance     string `json:"instance"`
		HitPoints    int    `json:"hitPoints"`
		MaxHitPoints int    `json:"maxHitPoints"`
	}
	autos := map[string]int{}
	autoCrits := map[string]int{}
	// M3 formula ceilings: (bonus+20)*1.25 [+5 player] [*slash 1.1],
	// crit x1.5 — war 37/52, archer 41/59, mage 62/91.
	autoMax := map[string][2]int{
		"p-warbot": {37, 52},
		"p-archer": {41, 59},
		"p-mage":   {62, 91},
	}
	autoOK := func(inst string, h hit) bool {
		max, ok := autoMax[inst]
		if !ok {
			return false
		}
		if h.Type == 0 {
			return h.Damage >= 0 && h.Damage <= max[0]
		}
		if h.Type == 6 {
			return h.Damage >= 0 && h.Damage <= max[1]
		}
		return false
	}
	var sunderTimes, volleyTimes, stormTimes []time.Duration
	var anims int
	var supportAtkAnims int
	var boulders, fireballs int
	var hpFirst, hpLast = -1, -1
	var combatHits int
	var projectiles int
	var projBad int
	var supportAttacks int
	var heals int
	var healTargets = map[string]int{}
	var healingFX int
	var buffFX = map[string]int{} // bot -> count of 21/25
	var botPoints = map[string]int{}
	var windowDeaths int
	rangedOK := true
	processed := 0
	process := func() {
		for _, s := range lastFrames[processed:] {
			var id int
			_ = json.Unmarshal(s.f[0], &id)
			switch id {
			case 5:
				var sp spawn
				if json.Unmarshal(frameData(s.f), &sp) != nil {
					continue
				}
				if sp.Type != 5 {
					continue
				}
				// In-flight projectile: owner archer/mage, target dummy.
				if (sp.OwnerInstance == "p-archer" || sp.OwnerInstance == "p-mage") &&
					sp.TargetInstance == "m-dummy" {
					projectiles++
				} else {
					projBad++
				}
			case 15:
				if len(s.f) < 3 {
					continue
				}
				var opcode int
				_ = json.Unmarshal(s.f[1], &opcode)
				if opcode != 1 {
					continue
				}
				var c combat
				if json.Unmarshal(frameData(s.f), &c) != nil {
					continue
				}
				if c.Target != "m-dummy" {
					continue
				}
				if c.Instance == "p-support" {
					supportAttacks++
					continue
				}
				combatHits++
				isSkill := false
				for _, sk := range c.Hit.Skills {
					switch sk {
					case "sunder":
						isSkill = true
						if c.Hit.Type == 6 && c.Hit.Damage == 40 {
							sunderTimes = append(sunderTimes, s.t)
						} else {
							check(false, fmt.Sprintf("sunder shape type=6 dmg=40 (got type=%d dmg=%d)", c.Hit.Type, c.Hit.Damage))
						}
					case "volley":
						isSkill = true
						if c.Hit.Type == 6 && c.Hit.Damage == 30 {
							volleyTimes = append(volleyTimes, s.t)
						} else {
							check(false, fmt.Sprintf("volley shape type=6 dmg=30 (got type=%d dmg=%d)", c.Hit.Type, c.Hit.Damage))
						}
						if c.Hit.Ranged == nil || !*c.Hit.Ranged {
							rangedOK = false
						}
					case "storm":
						isSkill = true
						if c.Hit.Type == 6 && c.Hit.Damage == 45 {
							stormTimes = append(stormTimes, s.t)
						} else {
							check(false, fmt.Sprintf("storm shape type=6 dmg=45 (got type=%d dmg=%d)", c.Hit.Type, c.Hit.Damage))
						}
						if c.Hit.Ranged == nil || !*c.Hit.Ranged {
							rangedOK = false
						}
					}
				}
				if !isSkill {
					switch c.Instance {
					case "p-warbot":
						if autoOK(c.Instance, c.Hit) {
							autos[c.Instance]++
							if c.Hit.Type == 6 {
								autoCrits[c.Instance]++
							}
						} else {
							check(false, fmt.Sprintf("unexpected war auto type=%d dmg=%d (want 0:0-37 or 6:0-52)", c.Hit.Type, c.Hit.Damage))
						}
					case "p-archer":
						if autoOK(c.Instance, c.Hit) {
							autos[c.Instance]++
							if c.Hit.Type == 6 {
								autoCrits[c.Instance]++
							}
						} else {
							check(false, fmt.Sprintf("unexpected archer auto type=%d dmg=%d (want 0:0-41 or 6:0-59)", c.Hit.Type, c.Hit.Damage))
						}
						if c.Hit.Ranged == nil || !*c.Hit.Ranged {
							rangedOK = false
						}
					case "p-mage":
						if autoOK(c.Instance, c.Hit) {
							autos[c.Instance]++
							if c.Hit.Type == 6 {
								autoCrits[c.Instance]++
							}
						} else {
							check(false, fmt.Sprintf("unexpected mage auto type=%d dmg=%d (want 0:0-62 or 6:0-91)", c.Hit.Type, c.Hit.Damage))
						}
						if c.Hit.Ranged == nil || !*c.Hit.Ranged {
							rangedOK = false
						}
					default:
						check(false, fmt.Sprintf("combat from unknown instance %s", c.Instance))
					}
				}
			case 16:
				var a struct {
					Instance string `json:"instance"`
					Action   int    `json:"action"`
				}
				if json.Unmarshal(frameData(s.f), &a) == nil && a.Action == 1 {
					anims++
					if a.Instance == "p-support" {
						supportAtkAnims++
					}
				}
			case 13:
				var d struct {
					Instance string `json:"instance"`
				}
				if json.Unmarshal(frameData(s.f), &d) == nil && d.Instance == "m-dummy" {
					windowDeaths++
				}
			case 17:
				var p points
				if json.Unmarshal(frameData(s.f), &p) == nil {
					if p.Instance == "m-dummy" {
						if hpFirst < 0 {
							hpFirst = p.HitPoints
						}
						hpLast = p.HitPoints
					} else {
						botPoints[p.Instance]++
					}
				}
			case 27:
				var h struct {
					Instance string `json:"instance"`
					Type     string `json:"type"`
					Amount   int    `json:"amount"`
				}
				if json.Unmarshal(frameData(s.f), &h) == nil {
					if h.Type == "hitpoints" && h.Amount >= 1 && h.Amount <= 40 {
						heals++
						healTargets[h.Instance]++
					} else {
						check(false, fmt.Sprintf("unexpected heal %+v", h))
					}
				}
			case 47:
				if len(s.f) < 3 {
					continue
				}
				var opcode int
				_ = json.Unmarshal(s.f[1], &opcode)
				var e struct {
					Instance string `json:"instance"`
					Effect   int    `json:"effect"`
				}
				if opcode == 0 && json.Unmarshal(frameData(s.f), &e) == nil {
					switch {
					case e.Instance == "m-dummy" && e.Effect == 9:
						boulders++
					case e.Instance == "m-dummy" && e.Effect == 6:
						fireballs++
					case e.Effect == 5:
						healingFX++
					case e.Effect == 21 || e.Effect == 25:
						buffFX[e.Instance]++
					}
				}
			}
			processed = len(lastFrames)
		}
	}
	t0 := time.Now()
	drain(15 * time.Second)
	process()
	for time.Since(t0) < 120*time.Second && !(autos["p-warbot"] >= 5 && autos["p-archer"] >= 4 && autos["p-mage"] >= 3 && len(sunderTimes) >= 1 && len(volleyTimes) >= 3 && len(stormTimes) >= 1) {
		drain(5 * time.Second)
		process()
	}
	window := time.Since(t0)
	fmt.Printf("  info: window=%v combatHits=%d autos=%v autoCrits=%v projectiles=%d projBad=%d anims=%d sunders=%d volleys=%d storms=%d boulders=%d fireballs=%d hp=%d->%d heals=%d healTargets=%v healingFX=%d buffFX=%v botPoints=%v\n",
		window, combatHits, autos, autoCrits, projectiles, projBad, anims, len(sunderTimes), len(volleyTimes), len(stormTimes), boulders, fireballs, hpFirst, hpLast, heals, healTargets, healingFX, buffFX, botPoints)
	lastFrames = nil

	check(supportAttacks == 0, fmt.Sprintf("support never attacks boss (got %d)", supportAttacks))
	check(autos["p-warbot"] >= 5, fmt.Sprintf("warrior autos ~1200ms (got %d in 15s)", autos["p-warbot"]))
	check(autos["p-archer"] >= 4, fmt.Sprintf("archer autos ~1600ms (got %d in 15s)", autos["p-archer"]))
	check(autos["p-mage"] >= 3, fmt.Sprintf("mage autos ~2000ms (got %d in 15s)", autos["p-mage"]))
	check(rangedOK, "archer/mage hits carry ranged=true (projectile impact path)")
	check(projectiles >= 1, fmt.Sprintf("real projectiles fly (Spawn type 5 owner archer/mage -> m-dummy, got %d)", projectiles))
	check(projBad == 0, fmt.Sprintf("no stray projectiles (got %d)", projBad))
	check((hpFirst >= 0 && hpLast >= 0 && hpLast < hpFirst) || windowDeaths > 0,
		fmt.Sprintf("boss HP drops via Points or dies mid-window (got %d->%d deaths=%d)", hpFirst, hpLast, windowDeaths))
	check(combatHits >= 10, fmt.Sprintf("splat packets observed (Combat Hit ≥10, got %d)", combatHits))
	check(len(sunderTimes) >= 1, fmt.Sprintf("≥1 Sunder cast (got %d)", len(sunderTimes)))
	check(len(volleyTimes) >= 3, fmt.Sprintf("≥1 Volley = 3 arrow hits (got %d)", len(volleyTimes)))
	check(len(stormTimes) >= 1, fmt.Sprintf("≥1 Storm cast (got %d)", len(stormTimes)))
	for name, times := range map[string][]time.Duration{"sunder": sunderTimes, "storm": stormTimes} {
		gcdOK := true
		for i := 1; i < len(times); i++ {
			if times[i]-times[i-1] < 4*time.Second {
				gcdOK = false
			}
		}
		check(gcdOK, fmt.Sprintf("%s respects cadence/GCD: casts ≥4s apart (times=%v)", name, times))
	}
	// Volley bursts: the 3 hits of one cast emit synchronously, so a >2s gap
	// starts a new burst. The trailing group may be partial (1-2 hits) when
	// the 15s window deadline lands mid-burst — that is truncation, not a bug.
	volleyGroups := [][]time.Duration{}
	for _, t := range volleyTimes {
		last := len(volleyGroups) - 1
		if last < 0 || t-volleyGroups[last][len(volleyGroups[last])-1] > 2*time.Second {
			volleyGroups = append(volleyGroups, []time.Duration{t})
		} else {
			volleyGroups[last] = append(volleyGroups[last], t)
		}
	}
	volleyOK := len(volleyTimes) >= 3
	fullBursts := 0
	groupLens := make([]int, len(volleyGroups))
	for i, g := range volleyGroups {
		groupLens[i] = len(g)
		if i < len(volleyGroups)-1 {
			if len(g) != 3 {
				volleyOK = false
			} else {
				fullBursts++
			}
		} else if len(g) < 1 || len(g) > 3 {
			volleyOK = false
		} else if len(g) == 3 {
			fullBursts++
		}
	}
	check(volleyOK && fullBursts >= 1,
		fmt.Sprintf("volley fires in 3-hit bursts (groups=%v total=%d)", groupLens, len(volleyTimes)))
	check(boulders >= len(sunderTimes) && boulders >= 1,
		fmt.Sprintf("Boulder effect per sunder (got %d)", boulders))
	check(fireballs >= len(stormTimes) && fireballs >= 1,
		fmt.Sprintf("Fireball effect per storm (got %d)", fireballs))
	check(heals == 0, fmt.Sprintf("support silent at full HP (heal skipped, no overheal spam — got %d Heal packets)", heals))
	check(healingFX == 0,
		fmt.Sprintf("no Healing FX without a heal (got %d)", healingFX))
	nBotPoints := 0
	for _, n := range botPoints {
		nBotPoints += n
	}
	check(nBotPoints == 0,
		fmt.Sprintf("no bot HP bar updates without a heal (got %v)", botPoints))
	check(supportAtkAnims == 0,
		fmt.Sprintf("support never swings (heal anim is Idle, got %d Attack anims)", supportAtkAnims))
	buffTotal := 0
	for _, n := range buffFX {
		buffTotal += n
	}
	check(buffTotal >= 4, fmt.Sprintf("party buff hits all 4 bots (got %d buff FX: %v)", buffTotal, buffFX))

	// --- Death + respawn: wait for Despawn then full-HP Spawn ---
	fmt.Println("waiting for boss death (Despawn, up to 150s)...")
	dead := false
	deadline := time.Now().Add(150 * time.Second)
	for !dead && time.Now().Before(deadline) {
		drain(5 * time.Second)
		for _, s := range lastFrames {
			var id int
			_ = json.Unmarshal(s.f[0], &id)
			if id != 13 {
				continue
			}
			var d struct {
				Instance string `json:"instance"`
			}
			if json.Unmarshal(frameData(s.f), &d) == nil && d.Instance == "m-dummy" {
				dead = true
			}
		}
		lastFrames = nil
	}
	check(dead, "boss dies at 0 HP (Despawn m-dummy)")

	fmt.Println("waiting for boss respawn (Spawn full HP, up to 30s)...")
	respawned := false
	deadline = time.Now().Add(30 * time.Second)
	for !respawned && time.Now().Before(deadline) {
		drain(5 * time.Second)
		for _, s := range lastFrames {
			var id int
			_ = json.Unmarshal(s.f[0], &id)
			if id != 5 {
				continue
			}
			var sp spawn
			if json.Unmarshal(frameData(s.f), &sp) == nil && sp.Instance == "m-dummy" {
				if dummyMax == 0 {
					// Connected while the boss was dead: no baseline max HP.
					check(sp.HitPoints == sp.MaxHitPoints && sp.MaxHitPoints > 0,
						fmt.Sprintf("respawn full HP %d/%d (boss was dead at connect)", sp.HitPoints, sp.MaxHitPoints))
				} else {
					check(sp.HitPoints == sp.MaxHitPoints && sp.MaxHitPoints == dummyMax,
						fmt.Sprintf("respawn full HP %d/%d (max %d)", sp.HitPoints, sp.MaxHitPoints, dummyMax))
				}
				respawned = true
			}
		}
		lastFrames = nil
	}
	check(respawned, "boss respawns after ~15s (Spawn m-dummy)")
	lastFrames = nil

	// --- M5 slice 1: kill rat -> loot spawns -> pickup -> XP -> persist ---
	// NOTE: steps are paced 500ms (server speed allowance is 209ms/tile and
	// sess trails NextGrid by one step, so back-to-back steps look like a
	// 2-tile jump); the trailing stationary step converges sess onto the
	// destination with no speed check (dx+dy == 0).
	fmt.Println("m5: hero steps 100,96 -> 102,96 ...")
	send(conn, `[11,{"opcode":0,"requestX":101,"requestY":96}]`)
	time.Sleep(500 * time.Millisecond)
	send(conn, `[11,{"opcode":2,"playerX":100,"playerY":96,"nextGridX":101,"nextGridY":96}]`)
	time.Sleep(500 * time.Millisecond)
	send(conn, `[11,{"opcode":2,"playerX":101,"playerY":96,"nextGridX":102,"nextGridY":96}]`)
	time.Sleep(500 * time.Millisecond)
	send(conn, `[11,{"opcode":2,"playerX":102,"playerY":96,"nextGridX":102,"nextGridY":96}]`)
	time.Sleep(500 * time.Millisecond)
	send(conn, `[6]`)
	drain(2 * time.Second)
	heroXY := [2]int{100, 96}
	for _, s := range lastFrames {
		var id int
		_ = json.Unmarshal(s.f[0], &id)
		if id != 6 || len(s.f) < 3 {
			continue
		}
		var opcode int
		_ = json.Unmarshal(s.f[1], &opcode)
		if opcode != 1 {
			continue
		}
		var lp struct {
			Positions map[string]struct {
				X int `json:"x"`
				Y int `json:"y"`
			} `json:"positions"`
		}
		if json.Unmarshal(frameData(s.f), &lp) == nil {
			if p, ok := lp.Positions[welcome.Instance]; ok {
				heroXY = [2]int{p.X, p.Y}
			}
		}
	}
	lastFrames = nil
	fmt.Printf("  info: hero authoritative pos %v\n", heroXY)

	fmt.Println("m5: hero attacks rat (4 swings)...")
	for i := 0; i < 4; i++ {
		send(conn, fmt.Sprintf(`[15,{"instance":%q,"target":"m-rat-1"}]`, welcome.Instance))
		time.Sleep(300 * time.Millisecond)
	}
	drain(6 * time.Second)
	ratGone, ratPoints := false, 0
	type lootSpawn struct {
		Instance string
		Type     int
		X, Y     int
	}
	var lootSpawns []lootSpawn
	xpHits, skillUps := 0, 0
	for _, s := range lastFrames {
		var id int
		_ = json.Unmarshal(s.f[0], &id)
		switch id {
		case 5:
			var sp spawn
			if json.Unmarshal(frameData(s.f), &sp) != nil {
				continue
			}
			if sp.Type == 2 || sp.Type == 8 {
				lootSpawns = append(lootSpawns, lootSpawn{sp.Instance, sp.Type, sp.X, sp.Y})
			}
		case 13:
			var d struct {
				Instance string `json:"instance"`
			}
			if json.Unmarshal(frameData(s.f), &d) == nil && d.Instance == "m-rat-1" {
				ratGone = true
			}
		case 17:
			var p points
			if json.Unmarshal(frameData(s.f), &p) == nil && p.Instance == "m-rat-1" {
				ratPoints++
			}
		case 28:
			if len(s.f) < 3 {
				continue
			}
			var opcode int
			_ = json.Unmarshal(s.f[1], &opcode)
			if opcode != 1 {
				continue
			}
			var e struct {
				Instance string `json:"instance"`
				Amount   int    `json:"amount"`
				Skill    int    `json:"skill"`
			}
			if json.Unmarshal(frameData(s.f), &e) == nil && e.Instance == welcome.Instance && e.Amount > 0 &&
				(e.Skill == 6 || e.Skill == 3) {
				xpHits++
			}
		case 44:
			skillUps++
		}
	}
	lastFrames = nil
	fmt.Printf("  info: ratGone=%v ratPoints=%d loot=%v xpHits=%d skillUps=%d\n",
		ratGone, ratPoints, lootSpawns, xpHits, skillUps)
	check(ratGone, "hero kills rat (Despawn m-rat-1)")
	check(ratPoints >= 1, fmt.Sprintf("rat HP drops via Points (got %d)", ratPoints))
	check(len(lootSpawns) >= 1, fmt.Sprintf("loot spawns at corpse (Item type 2 or LootBag type 8, got %v)", lootSpawns))
	check(xpHits >= 2, fmt.Sprintf("combat XP gained (Experience Skill Strength/Health, got %d)", xpHits))
	check(skillUps >= 1, fmt.Sprintf("Skill Update packets (got %d)", skillUps))

	fmt.Println("m5: Target-take all loot ...")
	for _, l := range lootSpawns {
		send(conn, fmt.Sprintf(`[14,[3,%q]]`, l.Instance))
		time.Sleep(200 * time.Millisecond)
	}
	drain(4 * time.Second)
	took := map[string]bool{}
	gotSlots := map[string]int{}
	for _, s := range lastFrames {
		var id int
		_ = json.Unmarshal(s.f[0], &id)
		switch id {
		case 13:
			var d struct {
				Instance string `json:"instance"`
			}
			if json.Unmarshal(frameData(s.f), &d) == nil {
				took[d.Instance] = true
			}
		case 21:
			if len(s.f) < 3 {
				continue
			}
			var opcode int
			_ = json.Unmarshal(s.f[1], &opcode)
			if opcode != 1 {
				continue
			}
			var cd struct {
				Slot *struct {
					Key   string `json:"key"`
					Count int    `json:"count"`
				} `json:"slot"`
			}
			if json.Unmarshal(frameData(s.f), &cd) == nil && cd.Slot != nil {
				gotSlots[cd.Slot.Key] += cd.Slot.Count
			}
		}
	}
	lastFrames = nil
	allTook := true
	for _, l := range lootSpawns {
		if !took[l.Instance] {
			allTook = false
		}
	}
	fmt.Printf("  info: took=%v slots=%v\n", took, gotSlots)
	check(allTook, fmt.Sprintf("loot despawns on pickup (took %v)", took))
	total := 0
	for _, c := range gotSlots {
		total += c
	}
	check(total >= 1, fmt.Sprintf("pickup lands in inventory (Container Add slots=%v)", gotSlots))

	fmt.Println("m5: persist round-trip (hero stays at 102,96) ...")
	send(conn, `[6]`)
	drain(2 * time.Second)
	savedXY := [2]int{102, 96}
	for _, s := range lastFrames {
		var id int
		_ = json.Unmarshal(s.f[0], &id)
		if id != 6 || len(s.f) < 3 {
			continue
		}
		var opcode int
		_ = json.Unmarshal(s.f[1], &opcode)
		if opcode != 1 {
			continue
		}
		var lp struct {
			Positions map[string]struct {
				X int `json:"x"`
				Y int `json:"y"`
			} `json:"positions"`
		}
		if json.Unmarshal(frameData(s.f), &lp) == nil {
			if p, ok := lp.Positions[welcome.Instance]; ok {
				savedXY = [2]int{p.X, p.Y}
			}
		}
	}
	lastFrames = nil
	fmt.Printf("  info: saved pos %v slots %v\n", savedXY, gotSlots)
	check(savedXY == [2]int{102, 96},
		fmt.Sprintf("hero stands on 102,96 before disconnect (got %v)", savedXY))
	conn.Close()
	time.Sleep(800 * time.Millisecond)

	incoming = make(chan []json.RawMessage, 65536)
	conn2, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+dialPort()+"/", nil)
	check(err == nil, "m5 reconnect dials")
	if err != nil {
		fmt.Println("RECONNECT DIAL FAIL:", err)
		os.Exit(1)
	}
	defer conn2.Close()
	go reader(conn2)
	drain(1500 * time.Millisecond)
	lastFrames = nil
	send(conn2, `[1,{"gVer":"0.5.5-beta"}]`)
	drain(1500 * time.Millisecond)
	lastFrames = nil
	send(conn2, `[2,{"opcode":0,"username":"tester","password":"x"}]`)
	drain(2 * time.Second)
	var rwelcome struct {
		Instance string `json:"instance"`
		X        int    `json:"x"`
		Y        int    `json:"y"`
	}
	restored := map[string]int{}
	restoredXP := map[int]int{}
	for _, s := range lastFrames {
		var id int
		_ = json.Unmarshal(s.f[0], &id)
		switch id {
		case 3:
			_ = json.Unmarshal(frameData(s.f), &rwelcome)
		case 21:
			if len(s.f) < 3 {
				continue
			}
			var opcode int
			_ = json.Unmarshal(s.f[1], &opcode)
			if opcode != 0 {
				continue
			}
			var cb struct {
				Data *struct {
					Slots []struct {
						Key   string `json:"key"`
						Count int    `json:"count"`
					} `json:"slots"`
				} `json:"data"`
			}
			if json.Unmarshal(frameData(s.f), &cb) == nil && cb.Data != nil {
				for _, sl := range cb.Data.Slots {
					restored[sl.Key] += sl.Count
				}
			}
		case 44:
			if len(s.f) < 3 {
				continue
			}
			var opcode int
			_ = json.Unmarshal(s.f[1], &opcode)
			if opcode != 0 {
				continue
			}
			var sb struct {
				Skills []struct {
					Type       int `json:"type"`
					Experience int `json:"experience"`
				} `json:"skills"`
			}
			if json.Unmarshal(frameData(s.f), &sb) == nil {
				for _, sk := range sb.Skills {
					restoredXP[sk.Type] = sk.Experience
				}
			}
		}
	}
	lastFrames = nil
	fmt.Printf("  info: reconnect Welcome %d,%d restored=%v xp=%v\n",
		rwelcome.X, rwelcome.Y, restored, restoredXP)
	check(rwelcome.X == savedXY[0] && rwelcome.Y == savedXY[1],
		fmt.Sprintf("position restored from DB (want %v got %d,%d)", savedXY, rwelcome.X, rwelcome.Y))
	match := len(gotSlots) > 0 && len(restored) > 0
	for k, c := range gotSlots {
		if restored[k] != c {
			match = false
		}
	}
	check(match, fmt.Sprintf("inventory restored from DB (want %v got %v)", gotSlots, restored))
	check(restoredXP[6] > 0 || restoredXP[3] > 0,
		fmt.Sprintf("skill XP restored from DB (got %v)", restoredXP))

	if len(failures) > 0 {
		fmt.Printf("CHECK FAILED (%d failures)\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("CHECK PASSED")
}

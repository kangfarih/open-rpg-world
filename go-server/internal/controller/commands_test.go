package controller

import (
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fakes for the command seams (embed the trade_test fakes where they fit).
// ---------------------------------------------------------------------------

type cmdConn struct {
	*fakeConn
	rank   int
	mspeed int
}

func newCmdConn(instance, username string, rank int) *cmdConn {
	return &cmdConn{fakeConn: &fakeConn{instance: instance, username: username}, rank: rank}
}

func (c *cmdConn) Rank() int              { return c.rank }
func (c *cmdConn) MovementSpeed() int     { return c.mspeed }
func (c *cmdConn) SetMovementSpeed(v int) { c.mspeed = v }

type cmdBus struct {
	*fakeBus
	broadcasts int
	bans       []string
	closes     []string
	sourced    []string
}

func newCmdBus() *cmdBus { return &cmdBus{fakeBus: newFakeBus()} }

func (b *cmdBus) Broadcast(frames ...[]any) { b.broadcasts += len(frames) }
func (b *cmdBus) SendBan(instance string)   { b.bans = append(b.bans, instance) }
func (b *cmdBus) Close(instance string)     { b.closes = append(b.closes, instance) }
func (b *cmdBus) NotifySource(instance, message, colour, source string) {
	b.sourced = append(b.sourced, instance+"|"+message+"|"+colour+"|"+source)
}

func (b *cmdBus) closed(instance string) bool {
	for _, c := range b.closes {
		if c == instance {
			return true
		}
	}
	return false
}

type cmdPeers struct {
	*fakePeers
}

func newCmdPeers(conns ...Conn) *cmdPeers { return &cmdPeers{fakePeers: newFakePeers(conns...)} }

// ByUsername is case-insensitive (world.getPlayerByName parity), shadowing
// the exact-match fakePeers lookup.
func (p *cmdPeers) ByUsername(username string) (CommandConn, bool) {
	for name, c := range p.byName {
		if strings.EqualFold(name, username) {
			if cc, ok := c.(CommandConn); ok {
				return cc, true
			}
			return nil, false
		}
	}
	return nil, false
}

// ByInstance shadows fakePeers with the narrower CommandConn result.
func (p *cmdPeers) ByInstance(instance string) (CommandConn, bool) {
	c, ok := p.byInst[instance]
	if !ok {
		return nil, false
	}
	if cc, ok := c.(CommandConn); ok {
		return cc, true
	}
	return nil, false
}

func (p *cmdPeers) Usernames() []string {
	out := make([]string, 0, len(p.byName))
	for name := range p.byName {
		out = append(out, name)
	}
	return out
}

type cmdFlags struct{ m map[string]CommandFlags }

func newCmdFlags() *cmdFlags { return &cmdFlags{m: map[string]CommandFlags{}} }

func (f *cmdFlags) Load(username string) CommandFlags { return f.m[username] }
func (f *cmdFlags) Save(username string, v CommandFlags) {
	f.m[username] = v
}

type cmdGuilds struct {
	members map[string]bool
	kicks   []string
}

func (g *cmdGuilds) InGuild(username string) bool        { return g.members[username] }
func (g *cmdGuilds) Invite(c CommandConn, target string) {}
func (g *cmdGuilds) Kick(c CommandConn, username string, viaCommand bool) {
	g.kicks = append(g.kicks, username)
}
func (g *cmdGuilds) RankCommand(c CommandConn, rankStr, username string) {}

type cmdWorld struct {
	teleports [][3]any
	damaged   map[string]int
	hp        map[string]int
	pvp       []string
}

func newCmdWorld() *cmdWorld { return &cmdWorld{damaged: map[string]int{}, hp: map[string]int{}} }

func (w *cmdWorld) Teleport(c CommandConn, x, y int) {
	w.teleports = append(w.teleports, [3]any{c.InstanceID(), x, y})
}
func (w *cmdWorld) DamagePlayer(c CommandConn, dmg int) { w.damaged[c.InstanceID()] = dmg }
func (w *cmdWorld) PlayerHP(c CommandConn) int {
	if hp, ok := w.hp[c.InstanceID()]; ok {
		return hp
	}
	return 100
}
func (w *cmdWorld) SetPVP(c CommandConn)                   { w.pvp = append(w.pvp, c.InstanceID()) }
func (w *cmdWorld) Countdown(c CommandConn, t int)         {}
func (w *cmdWorld) SameRegion(ax, ay, bx, by int) bool     { return ax == bx && ay == by }
func (w *cmdWorld) RegionOf(x, y int) int                  { return 0 }
func (w *cmdWorld) TileBlocked(x, y int) bool              { return false }
func (w *cmdWorld) WorldWidth() int                        { return 100 }
func (w *cmdWorld) SetEntityPos(instance string, x, y int) {}
func (w *cmdWorld) SpawnFrame(instance string) []any       { return []any{5, map[string]any{}} }
func (w *cmdWorld) LiveNPCPos(npcKey string) (int, int, bool) {
	return 0, 0, false
}

type cmdMob struct {
	instance string
	key      string
	x, y     int
	hp       int
	target   string
}

type cmdMobs struct{ m map[string]*cmdMob }

func newCmdMobs() *cmdMobs { return &cmdMobs{m: map[string]*cmdMob{}} }

func (m *cmdMobs) MobFor(instance string) (MobHandle, bool) {
	mm, ok := m.m[instance]
	if !ok {
		return nil, false
	}
	return mm, true
}
func (m *cmdMobs) MobInstance(h MobHandle) string { return h.(*cmdMob).instance }
func (m *cmdMobs) MobKey(h MobHandle) string      { return h.(*cmdMob).key }
func (m *cmdMobs) MobHP(h MobHandle) int          { return h.(*cmdMob).hp }
func (m *cmdMobs) MobPos(h MobHandle) (int, int)  { return h.(*cmdMob).x, h.(*cmdMob).y }
func (m *cmdMobs) SetMobPos(h MobHandle, x, y int) {
	mm := h.(*cmdMob)
	mm.x, mm.y = x, y
}
func (m *cmdMobs) Attack(a, b MobHandle) { a.(*cmdMob).target = b.(*cmdMob).instance }
func (m *cmdMobs) AttackTarget(h MobHandle, target string) {
	h.(*cmdMob).target = target
}
func (m *cmdMobs) ClearTarget(h MobHandle)     { h.(*cmdMob).target = "" }
func (m *cmdMobs) HitMob(h MobHandle, dmg int) { h.(*cmdMob).hp -= dmg }
func (m *cmdMobs) Instances() []string {
	out := make([]string, 0, len(m.m))
	for inst := range m.m {
		out = append(out, inst)
	}
	return out
}
func (m *cmdMobs) SpawnMob(instance, key string, x, y int) bool {
	if key == "ghost" {
		return false
	}
	m.m[instance] = &cmdMob{instance: instance, key: key, x: x, y: y, hp: 50}
	return true
}
func (m *cmdMobs) MobTalk(instance, message string) {}

type cmdQuests struct {
	questDefs  map[string]int
	questStage map[string]int
	achDefs    map[string]int
	achStage   map[string]int
	dirtied    []string
	popups     []string
	progress   []string
}

func newCmdQuests() *cmdQuests {
	return &cmdQuests{
		questDefs: map[string]int{}, questStage: map[string]int{},
		achDefs: map[string]int{}, achStage: map[string]int{},
	}
}

func (q *cmdQuests) MarkDirty(username string) { q.dirtied = append(q.dirtied, username) }
func (q *cmdQuests) QuestDef(key string) (int, bool) {
	n, ok := q.questDefs[key]
	return n, ok
}
func (q *cmdQuests) QuestStage(username, key string) (int, int) {
	return q.questStage[username+"|"+key], 0
}
func (q *cmdQuests) SetQuestStage(c CommandConn, username, key string, stage, subStage int) {
	q.questStage[username+"|"+key] = stage
}
func (q *cmdQuests) QuestKeys() []string {
	out := make([]string, 0, len(q.questDefs))
	for k := range q.questDefs {
		out = append(out, k)
	}
	return out
}
func (q *cmdQuests) AchDef(key string) (int, bool) {
	n, ok := q.achDefs[key]
	return n, ok
}
func (q *cmdQuests) AchStage(username, key string) int { return q.achStage[username+"|"+key] }
func (q *cmdQuests) SetAchStage(username, key string, stage int) {
	q.achStage[username+"|"+key] = stage
}
func (q *cmdQuests) AchProgress(c CommandConn, username, key string) {
	q.achStage[username+"|"+key]++
}
func (q *cmdQuests) AchKeys(username string) []string {
	var out []string
	for k := range q.achDefs {
		out = append(out, k)
	}
	return out
}
func (q *cmdQuests) AchDefs() map[string]int { return q.achDefs }
func (q *cmdQuests) SendAchProgress(c CommandConn, key string, stage int) {
	q.progress = append(q.progress, key)
}
func (q *cmdQuests) SendPopup(c CommandConn, title, message, colour string) {
	q.popups = append(q.popups, title)
}

type cmdInv struct {
	items map[string]bool
	inv   map[string][]CommandSlot
	bank  map[string][]CommandSlot
}

func newCmdInv() *cmdInv {
	return &cmdInv{items: map[string]bool{}, inv: map[string][]CommandSlot{}, bank: map[string][]CommandSlot{}}
}

func (s *cmdInv) MarkDirty(username string)  {}
func (s *cmdInv) ItemExists(key string) bool { return s.items[key] }
func (s *cmdInv) AddItem(username, key string, count int) int {
	s.inv[username] = append(s.inv[username], CommandSlot{Key: key, Count: count})
	return len(s.inv[username]) - 1
}
func (s *cmdInv) SlotAt(username, container string, index int) (CommandSlot, bool) {
	slots := s.inv[username]
	if container != "inventory" {
		slots = s.bank[username]
	}
	if index < 0 || index >= len(slots) {
		return CommandSlot{}, false
	}
	return slots[index], true
}
func (s *cmdInv) RemoveAt(username, container string, index, count int) (string, int, bool) {
	slots := s.inv[username]
	if container != "inventory" {
		slots = s.bank[username]
	}
	if index < 0 || index >= len(slots) {
		return "", 0, false
	}
	key := slots[index].Key
	slots[index].Count -= count
	left := slots[index].Count
	if left <= 0 {
		slots = append(slots[:index], slots[index+1:]...)
		if container != "inventory" {
			s.bank[username] = slots
		} else {
			s.inv[username] = slots
		}
		return "", 0, true
	}
	if container != "inventory" {
		s.bank[username] = slots
	} else {
		s.inv[username] = slots
	}
	// Return the surviving key/count (empty key when cleared).
	return key, left, true
}

func (s *cmdInv) RemoveKey(username, container, key string, count int) int {
	slots := s.inv[username]
	if container != "inventory" {
		slots = s.bank[username]
	}
	removed := 0
	var out []CommandSlot
	for _, sl := range slots {
		if sl.Key == key && removed < count {
			take := sl.Count
			if take > count-removed {
				take = count - removed
			}
			sl.Count -= take
			removed += take
		}
		if sl.Count > 0 {
			out = append(out, sl)
		}
	}
	if container != "inventory" {
		s.bank[username] = out
	} else {
		s.inv[username] = out
	}
	return removed
}
func (s *cmdInv) EmptyContainer(username, container string) {
	if container != "inventory" {
		s.bank[username] = nil
	} else {
		s.inv[username] = nil
	}
}
func (s *cmdInv) CopyContainer(src, dst string, bank bool) {
	if bank {
		s.bank[dst] = append([]CommandSlot(nil), s.bank[src]...)
	} else {
		s.inv[dst] = append([]CommandSlot(nil), s.inv[src]...)
	}
}
func (s *cmdInv) BankCount(username, key string) int {
	n := 0
	for _, sl := range s.bank[username] {
		if sl.Key == key {
			n += sl.Count
		}
	}
	return n
}
func (s *cmdInv) InvCount(username, key string) int {
	n := 0
	for _, sl := range s.inv[username] {
		if sl.Key == key {
			n += sl.Count
		}
	}
	return n
}
func (s *cmdInv) AppendBank(username, key string, count int) {
	s.bank[username] = append(s.bank[username], CommandSlot{Key: key, Count: count})
}
func (s *cmdInv) BankSlots(username string) []CommandSlot {
	return append([]CommandSlot(nil), s.bank[username]...)
}

type cmdLoot struct {
	at  []string
	bag []string
}

func (l *cmdLoot) SpawnLootAt(owner, key string, count, x, y int) {
	l.at = append(l.at, key)
}
func (l *cmdLoot) SpawnLootBag(owner string, x, y int, items []Drop) {
	l.bag = append(l.bag, owner)
}

func cmdDeps(bus *cmdBus, peers *cmdPeers, flags *cmdFlags) CommandDeps {
	return CommandDeps{
		Flags:  flags,
		Guilds: &cmdGuilds{members: map[string]bool{}},
		World:  newCmdWorld(),
		Mobs:   newCmdMobs(),
		Quests: newCmdQuests(),
		Inv:    newCmdInv(),
		Loot:   &cmdLoot{},
		Bus:    bus,
		Peers:  peers,
		Skills: newProgSkills(), Abilities: newProgAbilities(), Ranks: &progRanks{},
		Pets: &progPets{}, Poison: newProgPoison(), Misc: newProgMisc(),
	}
}

// ---------------------------------------------------------------------------
// Rank gates.
// ---------------------------------------------------------------------------

func TestModeratorGateBlocksPlayer(t *testing.T) {
	admin := newCmdConn("a1", "admin", 0)
	victim := newCmdConn("v1", "victim", 0)
	bus := newCmdBus()
	flags := newCmdFlags()
	d := cmdDeps(bus, newCmdPeers(admin, victim), flags)

	ModeratorCommands(admin, "mute", []string{"1", "victim"}, d)
	ModeratorCommands(admin, "ban", []string{"1", "victim"}, d)
	ModeratorCommands(admin, "kick", []string{"victim"}, d)
	ModeratorCommands(admin, "jail", []string{"1", "victim"}, d)
	ModeratorCommands(admin, "unjail", []string{"victim"}, d)
	ModeratorCommands(admin, "unmute", []string{"victim"}, d)

	if len(bus.notifs["a1"]) != 0 {
		t.Fatalf("rank-0 moderator commands notified: %v", bus.notifs["a1"])
	}
	if len(flags.m) != 0 {
		t.Fatalf("rank-0 moderator commands wrote flags: %v", flags.m)
	}
	if len(bus.closes) != 0 || len(bus.bans) != 0 {
		t.Fatalf("rank-0 moderator commands closed/banned: %v %v", bus.closes, bus.bans)
	}
	if w := d.World.(*cmdWorld); len(w.teleports) != 0 {
		t.Fatalf("rank-0 jail teleported: %v", w.teleports)
	}
}

func TestAdminGateBlocksModerator(t *testing.T) {
	mod := newCmdConn("m1", "mod", CmdRankModerator)
	bus := newCmdBus()
	flags := newCmdFlags()
	d := cmdDeps(bus, newCmdPeers(mod), flags)

	AdminCommands(mod, "noclip", nil, d)
	AdminCommands(mod, "ms", []string{"500"}, d)
	AdminCommands(mod, "getregion", nil, d)
	AdminCommands(mod, "spawn", []string{"sword", "1"}, d)
	AdminCommands(mod, "kill", []string{"mod"}, d)

	if len(bus.notifs["m1"]) != 0 {
		t.Fatalf("moderator admin commands notified: %v", bus.notifs["m1"])
	}
	if len(flags.m) != 0 {
		t.Fatalf("moderator admin commands wrote flags: %v", flags.m)
	}
	if mod.mspeed != 0 {
		t.Fatalf("moderator /ms applied speed %d", mod.mspeed)
	}
}

func TestModeratorMuteCap(t *testing.T) {
	mod := newCmdConn("m1", "mod", CmdRankModerator)
	victim := newCmdConn("v1", "victim", 0)
	bus := newCmdBus()
	flags := newCmdFlags()
	d := cmdDeps(bus, newCmdPeers(mod, victim), flags)

	before := time.Now().UnixMilli()
	ModeratorCommands(mod, "mute", []string{"500", "victim"}, d)
	f := flags.m["victim"]
	want := before + int64(ModMuteCapHours)*3600_000
	if f.Mute < want-10_000 || f.Mute > want+10_000 {
		t.Fatalf("mod mute deadline = %d, want ~%d (168h cap)", f.Mute, want)
	}
	if !bus.hasNotif("m1", "victim has been muted for 168 hours.") {
		t.Fatalf("mod mute notify missing: %v", bus.notifs["m1"])
	}
}

func TestAdminBypassesMuteCap(t *testing.T) {
	admin := newCmdConn("a1", "admin", CmdRankAdmin)
	victim := newCmdConn("v1", "victim", 0)
	bus := newCmdBus()
	flags := newCmdFlags()
	d := cmdDeps(bus, newCmdPeers(admin, victim), flags)

	before := time.Now().UnixMilli()
	ModeratorCommands(admin, "mute", []string{"500", "victim"}, d)
	f := flags.m["victim"]
	want := before + 500*3600_000
	if f.Mute < want-10_000 || f.Mute > want+10_000 {
		t.Fatalf("admin mute deadline = %d, want ~%d (no cap)", f.Mute, want)
	}
	if !bus.hasNotif("a1", "victim has been muted for 500 hours.") {
		t.Fatalf("admin mute notify missing: %v", bus.notifs["a1"])
	}
}

func TestBanCloseSemantics(t *testing.T) {
	admin := newCmdConn("a1", "admin", CmdRankAdmin)
	victim := newCmdConn("v1", "victim", 0)
	bus := newCmdBus()
	flags := newCmdFlags()
	d := cmdDeps(bus, newCmdPeers(admin, victim), flags)

	ModeratorCommands(admin, "ban", []string{"2", "victim"}, d)

	if len(bus.bans) != 1 || bus.bans[0] != "v1" {
		t.Fatalf("ban text frame not sent to victim: %v", bus.bans)
	}
	if !bus.closed("v1") {
		t.Fatalf("victim socket not closed: %v", bus.closes)
	}
	if !bus.hasNotif("a1", "victim has been banned for 2 hours.") {
		t.Fatalf("ban notify missing: %v", bus.notifs["a1"])
	}
	if f := flags.m["victim"]; !f.Banned(time.Now().UnixMilli()) {
		t.Fatalf("victim ban flag not persisted: %+v", f)
	}
}

func TestJailTeleportsAndUnjailClears(t *testing.T) {
	mod := newCmdConn("m1", "mod", CmdRankModerator)
	victim := newCmdConn("v1", "victim", 0)
	bus := newCmdBus()
	flags := newCmdFlags()
	d := cmdDeps(bus, newCmdPeers(mod, victim), flags)
	w := d.World.(*cmdWorld)

	ModeratorCommands(mod, "jail", []string{"3", "victim"}, d)
	if f := flags.m["victim"]; !f.Jailed(time.Now().UnixMilli()) {
		t.Fatalf("victim jail flag not persisted: %+v", f)
	}
	if len(w.teleports) != 1 || w.teleports[0][1] != 100 || w.teleports[0][2] != 96 {
		t.Fatalf("jail teleport missing: %v", w.teleports)
	}
	if len(bus.sourced) != 1 || !strings.Contains(bus.sourced[0], "jailed for 3 hours.") {
		t.Fatalf("jail sourced notify missing: %v", bus.sourced)
	}

	ModeratorCommands(mod, "unjail", []string{"victim"}, d)
	if f := flags.m["victim"]; f.Jailed(time.Now().UnixMilli()) {
		t.Fatalf("unjail did not clear: %+v", f)
	}
	if !bus.hasNotif("v1", "You have been unjailed.") {
		t.Fatalf("unjail victim notify missing: %v", bus.notifs["v1"])
	}
}

func TestGuildGateNotifies(t *testing.T) {
	player := newCmdConn("p1", "player", 0)
	bus := newCmdBus()
	d := cmdDeps(bus, newCmdPeers(player), newCmdFlags())

	ParseCommand(player, "guild", []string{"kick", "bob"}, d)
	if !bus.hasNotif("p1", "You are not in a guild.") {
		t.Fatalf("guild gate notify missing: %v", bus.notifs["p1"])
	}
	// Unknown commands stay silent.
	ParseCommand(player, "dance", nil, d)
	if len(bus.notifs["p1"]) != 1 {
		t.Fatalf("unknown command notified: %v", bus.notifs["p1"])
	}
}

// ---------------------------------------------------------------------------
// Pure clamp helpers.
// ---------------------------------------------------------------------------

func TestClampMoveSpeed(t *testing.T) {
	if got := ClampMoveSpeed(20000); got != 2000 {
		t.Fatalf("overspeed clamp = %d, want 2000", got)
	}
	if got := ClampMoveSpeed(10); got != 75 {
		t.Fatalf("underspeed clamp = %d, want 75", got)
	}
	if got := ClampMoveSpeed(500); got != 500 {
		t.Fatalf("valid speed changed = %d", got)
	}
}

func TestCommandFlagMethods(t *testing.T) {
	now := time.Now().UnixMilli()
	f := CommandFlags{Mute: now + 1000, Ban: now - 1, Jail: now + 1}
	if !f.Muted(now) || f.Banned(now) || !f.Jailed(now) {
		t.Fatalf("flag deadline methods wrong: %+v", f)
	}
}

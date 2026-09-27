// Package friends is an additive, transport-free friends-list engine for
// the Go server stub. The caller owns persistence, presence lookups, and
// all packet I/O: this package only tracks per-owner friend sets, block
// sets, and online-status flags. There are no goroutines, DB calls,
// timers, or network sends here.
//
// TS sources mirrored here:
//   - packages/server/src/game/entity/character/player/friends.ts —
//     Friends class: list keyed by lowercase username with {online,
//     serverId}, add (length/self/duplicate guards + database.exists +
//     world.isOnline), remove, sync of inactive friends, getFriendsList,
//     getInactiveFriends, hasFriend, setStatus, setActiveFriends,
//     serialize.
//   - packages/common/network/impl/friends.ts — Friend/FriendInfo
//     ({online, serverId} per username) and FriendsPacketData
//     ({list, username, status, serverId}).
//   - packages/common/network/opcodes.ts — Opcodes.Friends:
//     List 0, Add 1, Remove 2, Status 3, Sync 4.
//
// Packet ID conventions (for the caller, not emitted here): S->C Friends
// is packet 48 (internal/protocol PacketFriends); opcodes are List 0,
// Add 1, Remove 2, Status 3, Sync 4 as above.
package friends

import (
	"sort"
	"strings"
)

// MaxUsernameLen mirrors the TS add() guard: usernames longer than 32
// characters are rejected (TS notifies 'misc:FRIENDS_USERNAME_TOO_LONG').
const MaxUsernameLen = 32

// OfflineServerID mirrors the TS convention: friends not currently online
// carry serverId -1 (friends.ts load/add/setStatus).
const OfflineServerID = -1

// FriendInfo mirrors FriendInfo in common/network/impl/friends.ts:
// per-friend online flag plus the world/server the friend is on
// (-1 when offline).
type FriendInfo struct {
	Online   bool
	ServerID int
}

// OnlineNotify is the fan-out event produced when a friend comes online.
// The caller delivers it (Status packet to the list owner); this package
// only decides whether the event fires. Owner is the list owner to
// notify, Username the friend who came online, ServerID the world they
// joined.
type OnlineNotify struct {
	Owner    string
	Username string
	ServerID int
}

// List tracks one owner's friends plus a block set. Names are stored
// normalized (lowercase, trimmed); Owner is stored normalized too. The
// zero value is usable via lazy map init — build via New for a normalized
// owner, or use a bare List and let Add/Block/Load allocate the maps.
type List struct {
	Owner   string
	friends map[string]FriendInfo
	blocked map[string]struct{}
}

// ensureMaps lazily allocates the friend/block maps so zero-value Lists
// (and Lists decoded without maps) never panic on write.
func (l *List) ensureMaps() {
	if l.friends == nil {
		l.friends = map[string]FriendInfo{}
	}
	if l.blocked == nil {
		l.blocked = map[string]struct{}{}
	}
}

// New returns an empty List for the given owner name.
func New(owner string) *List {
	return &List{
		Owner:   normalize(owner),
		friends: map[string]FriendInfo{},
		blocked: map[string]struct{}{},
	}
}

// normalize lowercases a username for map keys (TS add/remove/hasFriend
// parity: (typeof player === 'string' ? player : player.username)
// .toLowerCase()). Leading/trailing whitespace is also trimmed — see
// divergences.
func normalize(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// Add inserts name into the friend set as offline (serverId -1). The
// caller resolves presence afterwards via SetStatus and persists via
// serialize. It reports false (leaving the list unchanged) when the name
// is empty, longer than MaxUsernameLen, the owner themself, already a
// friend, or blocked. TS notify-key mapping, the database.exists check,
// and the world.isOnline lookup are caller-owned — see divergences.
func (l *List) Add(name string) bool {
	if l == nil {
		return false
	}
	key := normalize(name)
	if key == "" || len(key) > MaxUsernameLen {
		return false
	}
	if key == l.Owner {
		return false
	}
	l.ensureMaps()
	if _, dup := l.friends[key]; dup {
		return false
	}
	if _, blocked := l.blocked[key]; blocked {
		return false
	}
	l.friends[key] = FriendInfo{Online: false, ServerID: OfflineServerID}
	return true
}

// Remove deletes name from the friend set, reporting whether it was
// present (TS remove() notifies 'misc:FRIENDS_NOT_IN_LIST' when absent;
// the caller maps a false return to that notice).
func (l *List) Remove(name string) bool {
	if l == nil {
		return false
	}
	key := normalize(name)
	if _, ok := l.friends[key]; !ok {
		return false
	}
	delete(l.friends, key)
	return true
}

// Block adds name to the block set and drops them from the friend set
// when present. Blocking the owner themself or an empty name is a no-op
// reporting false. There is no TS source — see divergences.
func (l *List) Block(name string) bool {
	if l == nil {
		return false
	}
	key := normalize(name)
	if key == "" || key == l.Owner {
		return false
	}
	l.ensureMaps()
	delete(l.friends, key)
	if _, dup := l.blocked[key]; dup {
		return false
	}
	l.blocked[key] = struct{}{}
	return true
}

// Unblock removes name from the block set, reporting whether it was
// present. The name is NOT re-added as a friend — the caller must Add
// them again. There is no TS source — see divergences.
func (l *List) Unblock(name string) bool {
	if l == nil {
		return false
	}
	key := normalize(name)
	if _, ok := l.blocked[key]; !ok {
		return false
	}
	delete(l.blocked, key)
	return true
}

// IsFriend reports whether name is in the friend set (TS hasFriend
// parity: `username in this.list`). The lookup normalizes case.
func (l *List) IsFriend(name string) bool {
	if l == nil {
		return false
	}
	_, ok := l.friends[normalize(name)]
	return ok
}

// IsBlocked reports whether name is in the block set. There is no TS
// source — see divergences.
func (l *List) IsBlocked(name string) bool {
	if l == nil {
		return false
	}
	_, ok := l.blocked[normalize(name)]
	return ok
}

// Members returns the sorted friend usernames (TS serialize() parity:
// Object.keys(this.list), sorted here for determinism). Nil-safe: a nil
// or zero List yields an empty (non-nil) slice.
func (l *List) Members() []string {
	if l == nil {
		return []string{}
	}
	out := make([]string, 0, len(l.friends))
	for name := range l.friends {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Serialize is Members under the TS name (friends.ts serialize()).
func (l *List) Serialize() []string {
	return l.Members()
}

// BlockedList returns the sorted blocked usernames. There is no TS
// source — see divergences.
func (l *List) BlockedList() []string {
	if l == nil {
		return []string{}
	}
	out := make([]string, 0, len(l.blocked))
	for name := range l.blocked {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Load replaces the friend set with names, all marked offline
// (serverId -1). It mirrors friends.ts load() minus the presence lookup:
// TS marks each entry online via world.isOnline; here the caller applies
// SetStatus afterwards. Invalid entries (empty, too long, self,
// duplicates) are skipped. The block set is untouched. Nil-safe and
// lazy-inits the map so zero-value Lists can Load.
func (l *List) Load(names []string) {
	if l == nil {
		return
	}
	l.ensureMaps()
	l.friends = make(map[string]FriendInfo, len(names))
	for _, name := range names {
		key := normalize(name)
		if key == "" || len(key) > MaxUsernameLen || key == l.Owner {
			continue
		}
		if _, dup := l.friends[key]; dup {
			continue
		}
		l.friends[key] = FriendInfo{Online: false, ServerID: OfflineServerID}
	}
}

// Info returns a copy of the stored FriendInfo for name, or false when
// name is not a friend.
func (l *List) Info(name string) (FriendInfo, bool) {
	if l == nil {
		return FriendInfo{}, false
	}
	info, ok := l.friends[normalize(name)]
	return info, ok
}

// SetStatus updates a friend's online flag and server ID (TS setStatus
// parity: serverId is the given ID when online, -1 when offline). It
// reports false and changes nothing when name is not a friend. Status
// callbacks to the client are caller-owned.
func (l *List) SetStatus(name string, online bool, serverID int) bool {
	if l == nil {
		return false
	}
	key := normalize(name)
	info, ok := l.friends[key]
	if !ok {
		return false
	}
	info.Online = online
	if online {
		info.ServerID = serverID
	} else {
		info.ServerID = OfflineServerID
	}
	l.friends[key] = info
	return true
}

// Inactive returns the sorted names of offline friends (TS
// getInactiveFriends parity: keys where !online). The caller passes this
// to the hub sync when needed. Nil-safe: nil yields nil.
func (l *List) Inactive() []string {
	if l == nil {
		return nil
	}
	var out []string
	for name, info := range l.friends {
		if !info.Online {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ShouldNotify reports whether an online-status change for name should be
// fanned out to the list owner: name must be a friend and must not be
// blocked. There is no TS source for the blocked half — see divergences.
func (l *List) ShouldNotify(name string) bool {
	if l == nil {
		return false
	}
	key := normalize(name)
	if _, ok := l.friends[key]; !ok {
		return false
	}
	if _, blocked := l.blocked[key]; blocked {
		return false
	}
	return true
}

// OnlineEvent marks a friend online and returns the fan-out event for the
// caller to deliver. It returns ok=false (changing nothing) when name is
// not a friend or is blocked, so blocked friends never produce login
// notices. Offline transitions use SetStatus directly (no event); the
// caller decides whether to emit logout notices.
func (l *List) OnlineEvent(name string, serverID int) (ev OnlineNotify, ok bool) {
	if l == nil {
		return OnlineNotify{}, false
	}
	key := normalize(name)
	if !l.ShouldNotify(key) {
		return OnlineNotify{}, false
	}
	info := l.friends[key]
	info.Online = true
	info.ServerID = serverID
	l.friends[key] = info
	return OnlineNotify{Owner: l.Owner, Username: key, ServerID: serverID}, true
}

// Divergences from TS (documented; new-in-Go behaviour):
//   - Block/Unblock/IsBlocked/BlockedList and the blocked half of
//     ShouldNotify/OnlineEvent/Add have no TS source: upstream Kaetram
//     friends.ts has no block or ignore list (verified: no block/ignore
//     handling in packages/server friends.ts, the common friends packet,
//     or the client friends menu — only List/Add/Remove/Status/Sync).
//     Blocked names cannot be re-added until unblocked, and online events
//     never fire for blocked names.
//   - Block drops the name from the friend set (block-then-unblock does
//     not restore the friendship; the caller must Add again). Unblock
//     never re-adds.
//   - normalize trims surrounding whitespace in addition to lowercasing;
//     TS only lowercases and relies on the DB exists check to reject junk.
//   - Add/Load mark new entries offline with serverId -1; TS resolves
//     presence inline via world.isOnline (plus config.serverId). The Go
//     caller applies SetStatus/OnlineEvent after Add/Load instead.
//   - The database.exists check, hub Sync sends, and all client callbacks
//     (onLoad/onAdd/onRemove/onStatus) are caller-owned: Add returns false
//     for every rejection (empty/too-long/self/duplicate/blocked) and the
//     caller maps that to the misc:FRIENDS_* notice or packet.
//   - Members/Serialize/Inactive/BlockedList sort their output; TS returns
//     Object.keys insertion order.

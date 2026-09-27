// Scripted WS check for the social wiring (TESTMAP=1 default server):
// two clients — friend add + online notify across disconnect/reconnect,
// guild create/invite(via /guild)/accept, owner rank set, guild chat fanout
// to members only (third observer gets nothing), disconnect cleanup
// (friends Status offline + guild Update serverId -1), friend remove, and
// the guildless /guild stub string.
// Not part of the stub build (e2e dirs are run via `go run`).
// Usage: PORT=9135 go run ./e2e/social (server on the same PORT).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Packet ids (mirrors go-server/packets.go).
const (
	pktWelcome = 3
	pktChat    = 19
	pktNotify  = 25
	pktGuild   = 35
	pktMinig   = 46
	pktFriends = 48
)

// Opcodes.
const (
	friendList   = 0
	friendAdd    = 1
	friendRemove = 2
	friendStatus = 3

	guildLogin  = 1
	guildJoin   = 3
	guildLeave  = 4
	guildRank   = 5
	guildUpdate = 6
	guildChat   = 11
)

var failures []string

func check(cond bool, msg string) {
	if !cond {
		failures = append(failures, msg)
		fmt.Println("  FAIL:", msg)
	} else {
		fmt.Println("  ok:", msg)
	}
}

func dialPort() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "9001"
}

type client struct {
	conn *websocket.Conn
	name string
	inst string
	ch   chan []json.RawMessage
}

func (c *client) reader() {
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var bulk [][]json.RawMessage
		if err := json.Unmarshal(raw, &bulk); err != nil {
			continue
		}
		for _, f := range bulk {
			c.ch <- f
		}
	}
}

func frameID(f []json.RawMessage) int {
	var id int
	if len(f) > 0 {
		_ = json.Unmarshal(f[0], &id)
	}
	return id
}

func frameOpcode(f []json.RawMessage) int {
	if len(f) >= 3 {
		var op int
		_ = json.Unmarshal(f[1], &op)
		return op
	}
	return -1
}

func frameData(f []json.RawMessage) json.RawMessage {
	if len(f) == 0 {
		return nil
	}
	return f[len(f)-1]
}

// drain collects frames for d, returning everything seen.
func (c *client) drain(d time.Duration) [][]json.RawMessage {
	var out [][]json.RawMessage
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case f := <-c.ch:
			if len(f) == 0 {
				continue
			}
			out = append(out, f)
		case <-timer.C:
			return out
		}
	}
}

// flush discards any pending frames (post-handshake noise).
func (c *client) flush() {
	c.drain(300 * time.Millisecond)
}

func (c *client) send(msg string) {
	if err := c.conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		fmt.Println("WRITE FAIL:", err)
		os.Exit(1)
	}
}

type friendData struct {
	List map[string]struct {
		Online   bool `json:"online"`
		ServerID int  `json:"serverId"`
	} `json:"list"`
	Username string `json:"username"`
	Status   *bool  `json:"status"`
	ServerID *int   `json:"serverId"`
}

func friendsFrames(fs [][]json.RawMessage, opcode int) []friendData {
	var out []friendData
	for _, f := range fs {
		if frameID(f) != pktFriends || frameOpcode(f) != opcode {
			continue
		}
		var d friendData
		if json.Unmarshal(frameData(f), &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

type guildData struct {
	Name       string `json:"name"`
	Owner      string `json:"owner"`
	Identifier string `json:"identifier"`
	Username   string `json:"username"`
	Message    string `json:"message"`
	ServerID   *int   `json:"serverId"`
	Rank       *int   `json:"rank"`
	Experience *int   `json:"experience"`
	Members    []struct {
		Username string `json:"username"`
		Rank     *int   `json:"rank"`
		ServerID *int   `json:"serverId"`
	} `json:"members"`
}

func guildFrames(fs [][]json.RawMessage, opcode int) []guildData {
	var out []guildData
	for _, f := range fs {
		if frameID(f) != pktGuild || frameOpcode(f) != opcode {
			continue
		}
		var d guildData
		if json.Unmarshal(frameData(f), &d) == nil {
			out = append(out, d)
		}
	}
	return out
}

func notifyHas(fs [][]json.RawMessage, substr string) (string, bool) {
	for _, f := range fs {
		if frameID(f) != pktNotify {
			continue
		}
		var n struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(frameData(f), &n)
		if strings.Contains(n.Message, substr) {
			return n.Message, true
		}
	}
	return "", false
}

func login(name string) *client {
	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+dialPort()+"/", nil)
	if err != nil {
		fmt.Println("DIAL FAIL:", err)
		os.Exit(1)
	}
	c := &client{conn: conn, name: name, ch: make(chan []json.RawMessage, 4096)}
	go c.reader()
	c.drain(1200 * time.Millisecond) // Connected
	c.send(`[1,{"gVer":"0.5.5-beta"}]`)
	c.drain(1200 * time.Millisecond) // Handshake
	c.send(fmt.Sprintf(`[2,{"opcode":0,"username":%q,"password":"x","seedRank":2}]`, name))
	frames := c.drain(2500 * time.Millisecond) // Welcome bulk
	for _, f := range frames {
		if frameID(f) == pktWelcome {
			var w struct {
				Instance string `json:"instance"`
			}
			_ = json.Unmarshal(frameData(f), &w)
			c.inst = w.Instance
		}
	}
	check(c.inst != "", fmt.Sprintf("%s welcome carries instance", name))
	c.send(`[9,{"regionsLoaded":true,"userAgent":"check"}]`)
	c.drain(2 * time.Second) // Spawns
	c.flush()
	return c
}

func main() {
	stamp := time.Now().UnixNano() % 1000000
	aName := fmt.Sprintf("soc-a-%d", stamp)
	bName := fmt.Sprintf("soc-b-%d", stamp)
	cName := fmt.Sprintf("soc-c-%d", stamp)
	gName := fmt.Sprintf("socguild%d", stamp)

	fmt.Println("== login A ==")
	a := login(aName)
	fmt.Println("== login B ==")
	b := login(bName)

	// --- Friend add (both online): A adds B. ---
	fmt.Println("== friend add ==")
	a.send(fmt.Sprintf(`[48,{"opcode":1,"username":%q}]`, bName))
	frames := a.drain(1500 * time.Millisecond)
	adds := friendsFrames(frames, friendAdd)
	found := false
	for _, d := range adds {
		if d.Username == bName && d.Status != nil && *d.Status && d.ServerID != nil && *d.ServerID == 1 {
			found = true
		}
	}
	check(found, fmt.Sprintf("A add B -> [48,1] status=true serverId=1 (got %d adds)", len(adds)))

	a.send(`[46,{"socialtest":"friends"}]`)
	frames = a.drain(1500 * time.Millisecond)
	if msg, ok := notifyHas(frames, "social:friends"); ok {
		check(strings.Contains(msg, bName), fmt.Sprintf("socialtest friends names B (%q)", msg))
	} else {
		check(false, "socialtest friends echo")
	}

	// --- B adds A back. ---
	b.send(fmt.Sprintf(`[48,{"opcode":1,"username":%q}]`, aName))
	frames = b.drain(1500 * time.Millisecond)
	adds = friendsFrames(frames, friendAdd)
	found = false
	for _, d := range adds {
		if d.Username == aName && d.Status != nil && *d.Status {
			found = true
		}
	}
	check(found, "B add A -> [48,1] status=true")

	// --- Online notify across disconnect: B drops, A must see offline. ---
	fmt.Println("== disconnect notify ==")
	_ = b.conn.Close()
	frames = a.drain(2 * time.Second)
	sts := friendsFrames(frames, friendStatus)
	off := false
	for _, d := range sts {
		if d.Username == bName && d.Status != nil && !*d.Status && d.ServerID != nil && *d.ServerID == -1 {
			off = true
		}
	}
	check(off, "B disconnect -> A [48,3] status=false serverId=-1")

	// --- Reconnect: A must see online again, B's List must show A online. ---
	fmt.Println("== reconnect notify ==")
	b = login(bName)
	frames = a.drain(2 * time.Second)
	sts = friendsFrames(frames, friendStatus)
	on := false
	for _, d := range sts {
		if d.Username == bName && d.Status != nil && *d.Status && d.ServerID != nil && *d.ServerID == 1 {
			on = true
		}
	}
	check(on, "B relog -> A [48,3] status=true serverId=1")

	// --- Guild create by A. ---
	fmt.Println("== guild create/invite/accept ==")
	a.send(fmt.Sprintf(`[35,{"opcode":0,"name":%q}]`, gName))
	frames = a.drain(1500 * time.Millisecond)
	logins := guildFrames(frames, guildLogin)
	created := false
	for _, d := range logins {
		if d.Name == gName && d.Owner == aName && len(d.Members) == 1 &&
			d.Members[0].Username == aName && d.Members[0].Rank != nil && *d.Members[0].Rank == 7 {
			created = true
		}
	}
	check(created, fmt.Sprintf("A create -> [35,1] owner rank=7 (got %d logins)", len(logins)))

	// --- Invite over /guild, accept via Join. ---
	a.send(fmt.Sprintf(`[19,["/guild invite %s"]]`, bName))
	frames = a.drain(1500 * time.Millisecond)
	_, okA := notifyHas(frames, "invited")
	framesB := b.drain(1500 * time.Millisecond)
	msgB, okB := notifyHas(framesB, "invited to guild")
	check(okA, "A /guild invite -> inviter confirm notify")
	check(okB, fmt.Sprintf("invite -> B notify (%q)", msgB))

	b.send(fmt.Sprintf(`[35,{"opcode":3,"identifier":%q}]`, strings.ToLower(gName)))
	frames = b.drain(1500 * time.Millisecond)
	blogins := guildFrames(frames, guildLogin)
	joined := false
	for _, d := range blogins {
		if d.Name == gName {
			for _, m := range d.Members {
				if m.Username == bName && m.Rank != nil && *m.Rank == 0 {
					joined = true
				}
			}
		}
	}
	check(joined, "B join -> [35,1] with B at rank 0")
	bupds := guildFrames(frames, guildUpdate)
	check(len(bupds) > 0, "B join -> [35,6] online roster")
	frames = a.drain(1500 * time.Millisecond)
	joins := guildFrames(frames, guildJoin)
	sawJoin := false
	for _, d := range joins {
		if d.Username == bName && d.ServerID != nil && *d.ServerID == 1 {
			sawJoin = true
		}
	}
	check(sawJoin, "B join -> A [35,3] username=B serverId=1")

	// --- Owner rank set over /guild. ---
	a.send(fmt.Sprintf(`[19,["/guild rank 3 %s"]]`, bName))
	frames = a.drain(1500 * time.Millisecond)
	ranks := guildFrames(frames, guildRank)
	sawRank := false
	for _, d := range ranks {
		if d.Username == bName && d.Rank != nil && *d.Rank == 3 {
			sawRank = true
		}
	}
	check(sawRank, "A /guild rank 3 B -> [35,5] rank=3")
	if msg, ok := notifyHas(frames, "rank to 3"); ok {
		check(strings.Contains(msg, bName), fmt.Sprintf("rank ack names B (%q)", msg))
	} else {
		check(false, "rank ack notify")
	}
	frames = b.drain(1500 * time.Millisecond)
	branks := guildFrames(frames, guildRank)
	bsaw := false
	for _, d := range branks {
		if d.Username == bName && d.Rank != nil && *d.Rank == 3 {
			bsaw = true
		}
	}
	check(bsaw, "rank fanout -> B [35,5] rank=3")

	// --- Guild chat fanout to members only (observer C gets nothing). ---
	fmt.Println("== guild chat ==")
	c := login(cName)
	chatMsg := fmt.Sprintf("hello-guild-%d", stamp)
	b.send(fmt.Sprintf(`[35,{"opcode":11,"message":%q}]`, chatMsg))
	framesA := a.drain(1500 * time.Millisecond)
	framesB2 := b.drain(1500 * time.Millisecond)
	framesC := c.drain(1500 * time.Millisecond)
	gotA := false
	for _, d := range guildFrames(framesA, guildChat) {
		if d.Username == bName && strings.Contains(d.Message, chatMsg) {
			gotA = true
		}
	}
	check(gotA, "guild chat -> A [35,11] from B")
	gotB := false
	for _, d := range guildFrames(framesB2, guildChat) {
		if d.Username == bName && strings.Contains(d.Message, chatMsg) {
			gotB = true
		}
	}
	check(gotB, "guild chat -> sender B [35,11] echo")
	leaked := false
	for _, d := range guildFrames(framesC, guildChat) {
		if strings.Contains(d.Message, chatMsg) {
			leaked = true
		}
	}
	check(!leaked, "guild chat -> observer C receives nothing")

	// --- Guildless /guild stub string still works (C). ---
	c.send(`[19,["/guild kick somebody"]]`)
	frames = c.drain(1500 * time.Millisecond)
	if msg, ok := notifyHas(frames, "You are not in a guild."); ok {
		check(true, fmt.Sprintf("guildless /guild -> stub string (%q)", msg))
	} else {
		check(false, "guildless /guild -> stub string")
	}

	// --- Disconnect cleanup: B drops -> A gets Status offline + Update -1. ---
	fmt.Println("== guild/friend disconnect cleanup ==")
	_ = b.conn.Close()
	frames = a.drain(2 * time.Second)
	sts = friendsFrames(frames, friendStatus)
	off = false
	for _, d := range sts {
		if d.Username == bName && d.Status != nil && !*d.Status {
			off = true
		}
	}
	check(off, "B disconnect -> A friends Status offline")
	upds := guildFrames(frames, guildUpdate)
	updOff := false
	for _, d := range upds {
		for _, m := range d.Members {
			if m.Username == bName && m.ServerID != nil && *m.ServerID == -1 {
				updOff = true
			}
		}
	}
	check(updOff, "B disconnect -> A guild Update serverId=-1")

	// --- Friend remove. ---
	a.send(fmt.Sprintf(`[48,{"opcode":2,"username":%q}]`, bName))
	frames = a.drain(1500 * time.Millisecond)
	rems := friendsFrames(frames, friendRemove)
	sawRem := false
	for _, d := range rems {
		if d.Username == bName {
			sawRem = true
		}
	}
	check(sawRem, "A remove B -> [48,2]")

	a.send(`[46,{"socialtest":"guild"}]`)
	frames = a.drain(1500 * time.Millisecond)
	if msg, ok := notifyHas(frames, "social:guild"); ok {
		check(strings.Contains(msg, aName) && strings.Contains(msg, bName),
			fmt.Sprintf("socialtest guild lists both members (%q)", msg))
	} else {
		check(false, "socialtest guild echo")
	}

	fmt.Println()
	if len(failures) > 0 {
		fmt.Printf("FAILED: %d checks\n", len(failures))
		for _, f := range failures {
			fmt.Println(" -", f)
		}
		os.Exit(1)
	}
	fmt.Println("CHECK PASSED (social: friends + presence + guild + chat + cleanup)")
}

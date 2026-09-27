// Probe: bank deposit/withdraw/swap frame behavior at rank 0 vs rank 2.
// Usage: PORT=xxxx PROBE_RANK=0|2 go run ./e2e/probe_swap
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

var incoming = make(chan []json.RawMessage, 4096)

func reader(conn *websocket.Conn) {
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var bulk [][]json.RawMessage
		if err := json.Unmarshal(raw, &bulk); err != nil {
			continue
		}
		for _, f := range bulk {
			incoming <- f
		}
	}
}

func drain(d time.Duration) map[int]int {
	counts := map[int]int{}
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case f := <-incoming:
			var id int
			_ = json.Unmarshal(f[0], &id)
			counts[id]++
			fmt.Printf("  frame id=%d data=%s\n", id, string(f[1]))
		case <-timer.C:
			return counts
		}
	}
}

func send(conn *websocket.Conn, s string) {
	_ = conn.WriteMessage(websocket.TextMessage, []byte(s))
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9001"
	}
	rank := os.Getenv("PROBE_RANK")
	conn, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+port+"/", nil)
	if err != nil {
		fmt.Println("DIAL FAIL:", err)
		os.Exit(1)
	}
	defer conn.Close()
	go reader(conn)
	drain(800 * time.Millisecond)
	send(conn, `[1,{"gVer":"0.5.5-beta"}]`)
	drain(800 * time.Millisecond)
	login := `[2,{"opcode":0,"username":"probe","password":"x","seedGold":2000}]`
	if rank == "2" {
		login = `[2,{"opcode":0,"username":"probe","password":"x","seedGold":2000,"seedRank":2}]`
	}
	send(conn, login)
	fmt.Println("login drain:", drain(2*time.Second))
	// walk to banker n-show-48 is complex; instead directly deposit/withdraw
	// need bank open: talk to banker requires position. Use seedPos near banker?
	// Banker tile unknown here; just do the swap on fresh inventory (2 stacks?
	// only gold). Swap fromIndex 0 value 1 with single stack: still silent?
	fmt.Println("--- swap on fresh login ---")
	send(conn, `[21,{"opcode":4,"type":1,"fromIndex":0,"value":1}]`)
	fmt.Println("swap drain:", drain(600*time.Millisecond))
}

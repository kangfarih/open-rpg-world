// Cross-shard presence — the router half of world.isOnline.
//
// TS (packages/server/src/network/api.ts isPlayerOnline) posts
// {hubAccessToken, serverId, username} to the hub's /isOnline endpoint and
// expects {online: bool}; the hub answers by scanning its server roster for the
// name on any OTHER shard (packages/hub/src/controllers/api.ts:225). With
// HUB_ENABLED=false TS short-circuits to false.
//
// Go mirrors that contract against the router (internal/app/router.go), whose
// address the shard already knows as HUB_ADDR. Socket URLs are accepted
// (ws:// -> http://) because that is the form shards dial.
//
// One documented divergence: TS's axios .catch() logs and never invokes the
// callback, so a router outage leaves the login promise unresolved forever.
// Here a failure resolves to false (fail-open: the local check still ran), so a
// router outage degrades duplicate-login detection instead of hanging logins.
package server

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"rpg-world-server/internal/hub"
)

// The router-side endpoint this client calls lives with the router
// (internal/app RouterHandler: POST /isOnline over hub.Server.FindPlayer),
// because package app cannot import this one.

// crossShardTimeout bounds the router round-trip so a slow router cannot stall
// a login (TS has no timeout at all).
const crossShardTimeout = time.Second

var crossShardClient = &http.Client{Timeout: crossShardTimeout}

// crossShardBaseURL resolves the router HTTP base URL, or "" when the process
// is running standalone (no hub configured) — the TS config.hubEnabled=false
// short-circuit.
func crossShardBaseURL() string {
	addr := strings.TrimSpace(os.Getenv("HUB_ADDR"))
	if addr == "" {
		addr = strings.TrimSpace(os.Getenv("HUB_LISTEN"))
	}
	if addr == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(addr, "ws://"):
		addr = "http://" + strings.TrimPrefix(addr, "ws://")
	case strings.HasPrefix(addr, "wss://"):
		addr = "https://" + strings.TrimPrefix(addr, "wss://")
	case strings.HasPrefix(addr, "http://"), strings.HasPrefix(addr, "https://"):
		// already an HTTP base
	default:
		addr = "http://" + addr
	}
	return strings.TrimRight(addr, "/")
}

// isOnlineRequest is the POST body TS sends.
type isOnlineRequest struct {
	HubAccessToken string `json:"hubAccessToken"`
	ServerID       int    `json:"serverId"`
	Username       string `json:"username"`
}

// isOnlineResponse is the TS hub shape ({status, online}).
type isOnlineResponse struct {
	Status string `json:"status"`
	Online bool   `json:"online"`
}

// crossShardOnline asks the router whether username is online on another shard.
// Standalone processes (no hub configured) answer false without a request.
func crossShardOnline(username string) bool {
	base := crossShardBaseURL()
	if base == "" || username == "" {
		return false
	}
	body, err := json.Marshal(isOnlineRequest{
		HubAccessToken: os.Getenv("HUB_TOKEN"),
		ServerID:       serverIDFromEnv(),
		Username:       username,
	})
	if err != nil {
		return false
	}
	res, err := crossShardClient.Post(base+"/isOnline", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("isOnline: router unreachable (%v), treating %q as offline", err, username)
		return false
	}
	defer func() { _ = res.Body.Close() }()
	var out isOnlineResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return false
	}
	return out.Online
}

// serverIDFromEnv mirrors config.serverId (SERVER_ID, default 1). The hub
// transport pins the same value on its handshake (hub.ServerIDFromEnv), so the
// id reported here is the id the router knows this shard by.
func serverIDFromEnv() int { return hub.ServerIDFromEnv() }

// atoiSafe is a tiny helper so the env parse never panics on junk.
func atoiSafe(v string) int {
	n := 0
	for _, r := range v {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
		if n > 1<<20 {
			return 0
		}
	}
	return n
}

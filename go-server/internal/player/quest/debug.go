package quest

import (
	"encoding/json"
	"fmt"
)

// ---------------------------------------------------------------------------
// TESTMAP debug dispatcher (m9test precedent, rides [46 {m11test}]).
// The testMode gate stays in the root adapter (it reads a root global);
// this body is the moved remainder.
// ---------------------------------------------------------------------------

// HandleTest processes TESTMAP debug ops: stage/substage injection,
// achievement stage injection, and state echoes for the e2e harness.
func HandleTest(c Conn, d Deps, data []byte) {
	var pkt struct {
		M11Test string `json:"m11test"`
		Key     string `json:"key"`
		Stage   int    `json:"stage"`
		Sub     int    `json:"sub"`
		Echo    string `json:"echo"`
	}
	if err := json.Unmarshal(data, &pkt); err != nil {
		return
	}
	if pkt.M11Test == "" || c == nil {
		return
	}
	Load()
	st := StateFor(c.PlayerName())
	switch pkt.M11Test {
	case "setstage": // inject quest stage (progress semantics: full setStage)
		if Quests[pkt.Key] == nil {
			return
		}
		st.PendingStart[pkt.Key] = false
		SetStage(c, d, st, pkt.Key, pkt.Stage, pkt.Sub, true)
	case "setach":
		if Achs[pkt.Key] == nil {
			return
		}
		for st.Achs[pkt.Key] < pkt.Stage {
			AchProgress(c, d, st, pkt.Key)
		}
	case "echo": // reply "m11:<key>=<stage>/<sub>" via notify (harness probe)
		var reply string
		switch pkt.Echo {
		case "quest":
			q := st.Quest(pkt.Key)
			def := Quests[pkt.Key]
			fin := def != nil && q.Stage >= def.StageCount
			reply = fmt.Sprintf("m11:%s=%d/%d fin=%v", pkt.Key, q.Stage, q.SubStage, fin)
		case "ach":
			// Nil-guard: unknown achievement keys previously panicked on
			// Achs[key].StageCount dereference.
			def := Achs[pkt.Key]
			if def == nil {
				reply = fmt.Sprintf("m11:ach:%s=?/?", pkt.Key)
				break
			}
			reply = fmt.Sprintf("m11:ach:%s=%d/%d", pkt.Key, st.Achs[pkt.Key], def.StageCount)
		case "pending":
			reply = fmt.Sprintf("m11:pending:%s=%v", pkt.Key, st.PendingStart[pkt.Key])
		case "drops": // gate probe: how many codersglitch drops available
			reply = fmt.Sprintf("m11:gate:%s=%v", pkt.Key, DropGated(c.PlayerName(), pkt.Key, "", "started"))
		}
		if reply != "" {
			d.Bus.Notify(c.InstanceID(), reply)
		}
	}
}

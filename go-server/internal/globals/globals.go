// Package globals is an additive, transport-free extraction of the
// lights/signs globals in packages/server/src/game/globals/:
// globals.ts (Globals holder), lights.ts (assign each map light to its
// region), signs.ts (index signs by "x-y", skip empty text), plus
// impl/light.ts (Light defaults + serialize) and impl/sign.ts (Sign pages
// + talk via BubblePacket).
//
// Sources (read-only, do NOT import the TS packages):
//   - packages/server/src/game/globals/globals.ts
//   - packages/server/src/game/globals/lights.ts
//   - packages/server/src/game/globals/signs.ts
//   - packages/server/src/game/globals/impl/light.ts
//   - packages/server/src/game/globals/impl/sign.ts
//   - packages/server/src/game/map/map.ts:38-39 (lights/signs come from
//     world.json areas.lights / areas.signs as ProcessedArea)
//   - packages/common/types/map.d.ts:39-108 (ProcessedArea light/sign fields)
//   - packages/common/network/impl/overlay.ts (SerializedLight + Lamp packet)
//   - packages/server/src/game/entity/character/player/handler.ts
//     (handleLights fans out OverlayPacket Lamp for surrounding regions)
//   - packages/server/src/game/entity/character/player/player.ts
//     (sign lookup by instance then sign.talk)
//   - packages/server/src/game/map/regions.ts (buildRegions/getRegion,
//     MAP_DIVISION_SIZE=48 in packages/common/network/modules.ts:630)
//   - packages/server/data/map/world.json (areas.lights / areas.signs shape)
//
// Divergence notes vs TS:
//   - Defaults preserved: missing colour falls back to
//     'rgba(0, 0, 0, 0.2)', missing distance to 100, diffuse to 0.2,
//     flickerSpeed to 300, and flickerIntensity to 1, matching the Light
//     constructor defaults in impl/light.ts.
//   - Sign keeps the raw text string. TS splits text on ',' into pages and
//     advances them via player.talkIndex, sending BubblePacket Position
//     frames (impl/sign.ts talk(), player.ts interaction). No talk
//     progression is modelled here.
//   - Region ids mirror regions.ts: SideLen = Width / 48, region =
//     (y/48)*SideLen + x/48, -1 when out of bounds (TS getRegion returns
//     findIndex or -1). TS stores lights on Region objects and handleLights
//     unions the entered region plus surrounding regions; LightsFor returns
//     a single region's lights and the caller unions surrounding regions
//     (see internal/worldmap SurroundingRegions).
//   - Out-of-bounds lights: TS drops them at construction
//     (lights.ts `if (!region) continue`). They are retained in Lights here
//     but are unreachable via LightsFor, so per-region queries match.
//   - Signs with empty/missing text are skipped, mirroring signs.ts
//     (which logs a warning and continues).
//   - No globals, no logging, no sync.Once: Load does one uncached read per
//     call and the caller owns caching. All lookups are pure.
package globals

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// MapDivisionSize mirrors Modules.Constants.MAP_DIVISION_SIZE
// (packages/common/network/modules.ts:630) used by regions.ts buildRegions.
const MapDivisionSize = 48

// DefaultLightColour mirrors the Light constructor default in
// packages/server/src/game/globals/impl/light.ts.
const DefaultLightColour = "rgba(0, 0, 0, 0.2)"

// DefaultLightRadius mirrors the Light constructor default distance (100)
// in packages/server/src/game/globals/impl/light.ts.
const DefaultLightRadius = 100

// DefaultLightDiffuse mirrors the Light constructor default diffuse (0.2).
const DefaultLightDiffuse = 0.2

// DefaultLightFlickerSpeed mirrors the Light constructor default flickerSpeed
// (300). Use -1 to disable flickering.
const DefaultLightFlickerSpeed = 300

// DefaultLightFlickerIntensity mirrors the Light constructor default
// flickerIntensity (1).
const DefaultLightFlickerIntensity = 1

// Light is a transport-free light: grid position plus reach (Radius, from
// ProcessedArea distance), emanated colour, and animation properties
// (Diffuse, FlickerSpeed, FlickerIntensity) matching impl/light.ts.
type Light struct {
	ID               int
	X                int
	Y                int
	Radius           int
	Colour           string
	Diffuse          float64
	FlickerSpeed     int
	FlickerIntensity int
}

// Sign is a transport-free sign: grid position plus raw display text.
type Sign struct {
	X    int
	Y    int
	Text string
}

// Globals is the loaded lights/signs set plus derived lookup state.
// Width/Height/SideLen mirror the world.json dimensions; SideLen is
// Width / MapDivisionSize (mirrors regions.ts buildRegions).
type Globals struct {
	Lights  []Light
	Signs   []Sign
	Width   int
	Height  int
	SideLen int

	signs map[string]Sign
}

// lightArea mirrors the ProcessedArea light fields used here
// (packages/common/types/map.d.ts:65-71). Pointers distinguish "absent"
// (apply TS constructor default) from explicit zero values.
type lightArea struct {
	ID               int      `json:"id"`
	X                int      `json:"x"`
	Y                int      `json:"y"`
	Colour           *string  `json:"colour"`
	Distance         *int     `json:"distance"`
	Diffuse          *float64 `json:"diffuse"`
	FlickerSpeed     *int     `json:"flickerSpeed"`
	FlickerIntensity *int     `json:"flickerIntensity"`
}

// signArea mirrors the ProcessedArea sign fields used here
// (packages/common/types/map.d.ts:106-107).
type signArea struct {
	X    int    `json:"x"`
	Y    int    `json:"y"`
	Text string `json:"text"`
}

// worldFile mirrors the subset of world.json read here: dimensions plus
// the areas.lights / areas.signs groups (map.ts:38-39).
type worldFile struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	Areas  struct {
		Lights []lightArea `json:"lights"`
		Signs  []signArea  `json:"signs"`
	} `json:"areas"`
}

// Load reads and parses the world JSON at path, applying the TS constructor
// defaults for missing light colour/distance and skipping signs with empty
// text. One call = one read; the caller owns caching (no globals).
// Returns an error on missing or invalid files.
func Load(path string) (*Globals, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read world.json: %w", err)
	}
	var w worldFile
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("parse world.json: %w", err)
	}
	g := &Globals{
		Width:   w.Width,
		Height:  w.Height,
		SideLen: 0,
		signs:   make(map[string]Sign, len(w.Areas.Signs)),
	}
	if w.Width > 0 {
		g.SideLen = w.Width / MapDivisionSize
	}
	for _, l := range w.Areas.Lights {
		colour := DefaultLightColour
		if l.Colour != nil {
			colour = *l.Colour
		}
		radius := DefaultLightRadius
		if l.Distance != nil {
			radius = *l.Distance
		}
		diffuse := DefaultLightDiffuse
		if l.Diffuse != nil {
			diffuse = *l.Diffuse
		}
		flickerSpeed := DefaultLightFlickerSpeed
		if l.FlickerSpeed != nil {
			flickerSpeed = *l.FlickerSpeed
		}
		flickerIntensity := DefaultLightFlickerIntensity
		if l.FlickerIntensity != nil {
			flickerIntensity = *l.FlickerIntensity
		}
		g.Lights = append(g.Lights, Light{
			ID:               l.ID,
			X:                l.X,
			Y:                l.Y,
			Radius:           radius,
			Colour:           colour,
			Diffuse:          diffuse,
			FlickerSpeed:     flickerSpeed,
			FlickerIntensity: flickerIntensity,
		})
	}
	for _, s := range w.Areas.Signs {
		// Mirrors signs.ts: skip signs with no text.
		if s.Text == "" {
			continue
		}
		sign := Sign{X: s.X, Y: s.Y, Text: s.Text}
		g.Signs = append(g.Signs, sign)
		g.signs[signKey(s.X, s.Y)] = sign
	}
	return g, nil
}

// RegionOf returns the region id containing grid position (x, y), mirroring
// regions.ts getRegion/inRegion: (y/48)*SideLen + x/48, or -1 when out of
// bounds (map.ts:230, lights.ts:11 use the same call before region.addLight).
func (g *Globals) RegionOf(x, y int) int {
	if g == nil || g.SideLen <= 0 {
		return -1
	}
	if x < 0 || y < 0 || x >= g.Width || y >= g.Height {
		return -1
	}
	return (y/MapDivisionSize)*g.SideLen + (x / MapDivisionSize)
}

// LightsFor returns the lights in region id, mirroring the per-region
// light lists built by lights.ts (region.addLight). Pure lookup; the
// caller unions surrounding regions to mirror handleLights. Returns nil
// for negative ids (handler.ts handleLights returns early on region < 0).
func (g *Globals) LightsFor(region int) []Light {
	if g == nil || region < 0 {
		return nil
	}
	var out []Light
	for _, l := range g.Lights {
		if g.RegionOf(l.X, l.Y) == region {
			out = append(out, l)
		}
	}
	return out
}

// SignAt returns the sign at grid position (x, y), mirroring signs.ts
// get(coordinate) with the "x-y" key (player.ts looks signs up by the
// "x-y" instance). The second return value reports a hit.
func (g *Globals) SignAt(x, y int) (Sign, bool) {
	if g == nil {
		return Sign{}, false
	}
	s, ok := g.signs[signKey(x, y)]
	return s, ok
}

// signKey mirrors the `${x}-${y}` coordinate key in signs.ts and the Sign
// instance in impl/sign.ts.
func signKey(x, y int) string {
	return strconv.Itoa(x) + "-" + strconv.Itoa(y)
}

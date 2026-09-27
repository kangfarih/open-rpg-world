package globals

import (
	"os"
	"path/filepath"
	"testing"
)

// sampleWorld mirrors the world.json subset read by Load: dimensions plus
// areas.lights / areas.signs as ProcessedArea entries (map.ts:38-39).
// Width 96 gives SideLen 2 (96/48), so region = (y/48)*2 + x/48:
// (10,10)->0, (50,10)->1, (10,50)->2.
const sampleWorld = `{
	"width": 96,
	"height": 96,
	"areas": {
		"lights": [
			{"id": 1096, "x": 10, "y": 10, "width": 1, "height": 1, "colour": "rgba(255, 0, 0, 0.5)", "diffuse": 0.3, "distance": 160},
			{"id": 1097, "x": 50, "y": 10, "width": 1, "height": 1, "diffuse": 0.4, "distance": 100},
			{"id": 1098, "x": 10, "y": 50, "width": 1, "height": 1}
		],
		"signs": [
			{"id": 352, "x": 20, "y": 20, "width": 1, "height": 1, "text": "Tread carefully of what lies ahead."},
			{"id": 368, "x": 30, "y": 30, "width": 1, "height": 1, "text": "In memory of Azaria,my dearest friend."},
			{"id": 999, "x": 40, "y": 40, "width": 1, "height": 1, "text": ""}
		]
	}
}`

func loadSample(t *testing.T) *Globals {
	t.Helper()
	path := filepath.Join(t.TempDir(), "world.json")
	if err := os.WriteFile(path, []byte(sampleWorld), 0o644); err != nil {
		t.Fatalf("write sample world.json: %v", err)
	}
	g, err := Load(path)
	if err != nil {
		t.Fatalf("Load(sample) error = %v", err)
	}
	return g
}

func TestLoadParsesLightsWithDefaults(t *testing.T) {
	g := loadSample(t)

	if len(g.Lights) != 3 {
		t.Fatalf("len(Lights) = %d, want 3", len(g.Lights))
	}

	// Fully-specified entry keeps its colour, distance (Radius), diffuse,
	// and id; flickerSpeed/flickerIntensity fall back to defaults.
	if got := g.Lights[0]; got != (Light{ID: 1096, X: 10, Y: 10, Radius: 160, Colour: "rgba(255, 0, 0, 0.5)", Diffuse: 0.3, FlickerSpeed: 300, FlickerIntensity: 1}) {
		t.Fatalf("Lights[0] = %+v, want explicit colour/radius/diffuse", got)
	}

	// Missing colour falls back to the impl/light.ts constructor default.
	if got := g.Lights[1].Colour; got != DefaultLightColour {
		t.Fatalf("Lights[1].Colour = %q, want default %q", got, DefaultLightColour)
	}
	if got := g.Lights[1].Radius; got != 100 {
		t.Fatalf("Lights[1].Radius = %d, want 100", got)
	}
	if got := g.Lights[1].Diffuse; got != 0.4 {
		t.Fatalf("Lights[1].Diffuse = %f, want 0.4", got)
	}

	// Bare entry gets all defaults.
	if got := g.Lights[2]; got != (Light{ID: 1098, X: 10, Y: 50, Radius: DefaultLightRadius, Colour: DefaultLightColour, Diffuse: DefaultLightDiffuse, FlickerSpeed: DefaultLightFlickerSpeed, FlickerIntensity: DefaultLightFlickerIntensity}) {
		t.Fatalf("Lights[2] = %+v, want defaults", got)
	}
}

func TestLoadSkipsEmptySignText(t *testing.T) {
	g := loadSample(t)

	// Mirrors signs.ts: the empty-text sign is skipped.
	if len(g.Signs) != 2 {
		t.Fatalf("len(Signs) = %d, want 2 (empty text skipped)", len(g.Signs))
	}
}

func TestSignAtHitAndMiss(t *testing.T) {
	g := loadSample(t)

	// Hit: exact "x-y" coordinate key (signs.ts get, player.ts instance).
	s, ok := g.SignAt(20, 20)
	if !ok {
		t.Fatal("SignAt(20,20) miss, want hit")
	}
	if s.Text != "Tread carefully of what lies ahead." {
		t.Fatalf("SignAt(20,20).Text = %q", s.Text)
	}

	// Multi-page raw text is kept verbatim (TS splits on ',' at talk time).
	s, ok = g.SignAt(30, 30)
	if !ok {
		t.Fatal("SignAt(30,30) miss, want hit")
	}
	if s.Text != "In memory of Azaria,my dearest friend." {
		t.Fatalf("SignAt(30,30).Text = %q", s.Text)
	}

	// Miss: empty coordinate and skipped empty-text sign.
	if _, ok := g.SignAt(1, 1); ok {
		t.Fatal("SignAt(1,1) hit, want miss")
	}
	if _, ok := g.SignAt(40, 40); ok {
		t.Fatal("SignAt(40,40) hit, want miss (empty text skipped)")
	}
}

func TestLightsForRegionFilter(t *testing.T) {
	g := loadSample(t)

	// Region 0 holds only the (10,10) light; region 1 the (50,10) light.
	got := g.LightsFor(0)
	if len(got) != 1 || got[0].X != 10 || got[0].Y != 10 {
		t.Fatalf("LightsFor(0) = %+v, want only the (10,10) light", got)
	}
	got = g.LightsFor(1)
	if len(got) != 1 || got[0].X != 50 || got[0].Y != 10 {
		t.Fatalf("LightsFor(1) = %+v, want only the (50,10) light", got)
	}

	// Negative region mirrors handler.ts handleLights early return.
	if got := g.LightsFor(-1); len(got) != 0 {
		t.Fatalf("LightsFor(-1) = %+v, want empty", got)
	}
}

func TestLoadMissingFileReturnsError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("Load(missing file) = nil error, want error")
	}
}

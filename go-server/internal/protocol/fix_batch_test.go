package protocol

import (
	"testing"
)

func TestEntityPetConst(t *testing.T) {
	if EntityPet != 7 {
		t.Fatalf("EntityPet = %d, want 7 (modules.ts EntityType.Pet)", EntityPet)
	}
}

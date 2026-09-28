package world

// Modes selects the map/entity overlay set. Mirrors the main.go TESTMAP /
// CLEAN / COMBAT globals: TESTMAP defaults OFF (proper gameplay with real
// world regions), CLEAN and COMBAT default OFF (pure terrain; CLEAN adds one
// adventurer, COMBAT adds the warbot party + boss dummy). Use TESTMAP=1 or
// --testmap for dev/testing with the showcase grid.
type Modes struct {
	Test   bool
	Clean  bool
	Combat bool
}

// ParseModes resolves the mode flags with the exact main.go precedence: env
// wins, then CLI flags, then defaults (test ON, clean/combat OFF).
//
//   - TESTMAP env: "" = unset; any value except 0/false/off/no = ON.
//     Flags: --testmap[/=true/=1] ON; --testmap=false/=0/--notestmap OFF.
//   - CLEAN env: "" = unset; 0/false/off/no = OFF; anything else = ON.
//     Flags: --clean[/=true/=1] ON; --clean=false/=0/--noclean OFF.
//   - COMBAT env: same shape as CLEAN (--combat/--nocombat flags).
func ParseModes(getenv func(string) string, args []string) Modes {
	return Modes{
		Test:   parseTestMode(getenv("TESTMAP"), args),
		Clean:  parseFlagMode(getenv("CLEAN"), args, "--clean", "--noclean"),
		Combat: parseFlagMode(getenv("COMBAT"), args, "--combat", "--nocombat"),
	}
}

func parseTestMode(env string, args []string) bool {
	if env != "" {
		return env != "0" && env != "false" && env != "off" && env != "no"
	}
	for _, a := range args {
		switch a {
		case "--testmap", "--testmap=true", "--testmap=1":
			return true
		case "--testmap=false", "--testmap=0", "--notestmap":
			return false
		}
	}
	return false // Default OFF for proper gameplay; use TESTMAP=1 or --testmap for dev/testing
}

func parseFlagMode(env string, args []string, on, off string) bool {
	if env != "" {
		switch env {
		case "0", "false", "off", "no":
			return false
		default:
			return true
		}
	}
	for _, a := range args {
		switch a {
		case on, on + "=true", on + "=1":
			return true
		case on + "=false", on + "=0", off:
			return false
		}
	}
	return false
}

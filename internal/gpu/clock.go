package gpu

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"a310ctl/internal/hardware"
)

const RegisterRpCapabilities = 0x145998 // RP0 [7:0], RP1 [15:8], RPn [23:16] x50 MHz

// Clock is the frequency range information (read-only).
type Clock struct {
	RP0     int `json:"rp0_megahertz"`
	RP1     int `json:"rp1_megahertz"`
	RPn     int `json:"rpn_megahertz"`
	Minimum int `json:"minimum_megahertz,omitempty"` // driver sysfs
	Maximum int `json:"maximum_megahertz,omitempty"` // driver sysfs
}

func readInteger(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return value
}

// ReadClock reads RP capabilities from hardware and the driver's min/max from sysfs
// (i915: drm/cardN/gt_{min,max}_freq_mhz, xe: tile0/gt0/freq0/{min,max}_freq).
func ReadClock(register hardware.Register, sysPath string) Clock {
	capabilities := register.Read32(RegisterRpCapabilities)
	clock := Clock{
		RP0: int(capabilities&0xFF) * 50,
		RP1: int((capabilities>>8)&0xFF) * 50,
		RPn: int((capabilities>>16)&0xFF) * 50,
	}
	if base := filepath.Join(sysPath, "tile0", "gt0", "freq0"); fileExists(base) {
		clock.Minimum = readInteger(filepath.Join(base, "min_freq"))
		clock.Maximum = readInteger(filepath.Join(base, "max_freq"))
	} else if cards, _ := filepath.Glob(filepath.Join(sysPath, "drm", "card[0-9]*")); len(cards) > 0 {
		clock.Minimum = readInteger(filepath.Join(cards[0], "gt_min_freq_mhz"))
		clock.Maximum = readInteger(filepath.Join(cards[0], "gt_max_freq_mhz"))
	}
	return clock
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

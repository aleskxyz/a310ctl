// Package gpu implements the A310 controls and telemetry on top of hardware.Register.
// Offsets and encodings are documented in INTERNALS.md.
package gpu

import (
	"errors"
	"fmt"
	"math"
	"time"

	"a310ctl/internal/hardware"
)

const (
	RegisterRaplLimit    = 0x1459A0 // PL1 low dword, PL2 high dword
	RegisterPowerSku     = 0x145930 // TDP [14:0], minimum [30:16], maximum [46:32]
	RegisterPowerUnits   = 0x145938 // power [3:0], energy [12:8], time [19:16]
	RegisterEnergyStatus = 0x14593C // 32-bit energy counter

	limitValueMask   = 0x7FFF
	limitEnable      = 1 << 15
	limitWindowShift = 17
	limitWindowMask  = 0x7F
	raplLockBit      = 1 << 31 // in the high dword (bit 63)

	PowerMinimumWatts = 10.0
	PowerMaximumWatts = 75.0 // slot-powered card: hard cap
)

// Units are the divisors from POWER_SKU_UNIT.
type Units struct{ Power, Energy, Time float64 }

// ReadUnits decodes POWER_SKU_UNIT.
func ReadUnits(register hardware.Register) Units {
	value := register.Read32(RegisterPowerUnits)
	return Units{
		Power:  float64(uint64(1) << (value & 0xF)),
		Energy: float64(uint64(1) << ((value >> 8) & 0x1F)),
		Time:   float64(uint64(1) << ((value >> 16) & 0xF)),
	}
}

// Limit is one decoded power limit (PL1 or PL2).
type Limit struct {
	Watts         float64 `json:"watts"`
	Enabled       bool    `json:"enabled"`
	WindowSeconds float64 `json:"window_seconds"`
}

// PowerState is the decoded RAPL limit register.
type PowerState struct {
	RawLow    uint32  `json:"raw_low"`
	RawHigh   uint32  `json:"raw_high"`
	Locked    bool    `json:"locked"`
	Sustained Limit   `json:"sustained"`
	Burst     Limit   `json:"burst"`
	TDPWatts  float64 `json:"tdp_watts"`
	SkuMin    float64 `json:"sku_minimum_watts"`
	SkuMax    float64 `json:"sku_maximum_watts"`
}

// WindowDecode converts the 7-bit time-window field to seconds.
func WindowDecode(field uint32, timeUnit float64) float64 {
	y, z := field&0x1F, (field>>5)&0x3
	return float64(uint64(1)<<y) * (1 + float64(z)/4) / timeUnit
}

// WindowEncode returns the 7-bit field closest to the requested seconds.
func WindowEncode(seconds, timeUnit float64) uint32 {
	best, bestDiff := uint32(0), math.Inf(1)
	for y := uint32(0); y < 32; y++ {
		for z := uint32(0); z < 4; z++ {
			diff := math.Abs(WindowDecode(y|z<<5, timeUnit) - seconds)
			if diff < bestDiff {
				best, bestDiff = y|z<<5, diff
			}
		}
	}
	return best
}

// DecodeLimit decodes one limit dword.
func DecodeLimit(dword uint32, units Units) Limit {
	return Limit{
		Watts:         float64(dword&limitValueMask) / units.Power,
		Enabled:       dword&limitEnable != 0,
		WindowSeconds: WindowDecode((dword>>limitWindowShift)&limitWindowMask, units.Time),
	}
}

// EncodeWatts sets value (and the enable bit) in a limit dword.
func EncodeWatts(dword uint32, watts float64, units Units) (uint32, error) {
	raw := uint32(math.Round(watts * units.Power))
	if raw == 0 || raw > limitValueMask {
		return 0, fmt.Errorf("%g watts does not fit the 15-bit field", watts)
	}
	return (dword &^ limitValueMask) | raw | limitEnable, nil
}

// EncodeWindow sets the time-window field in a limit dword.
func EncodeWindow(dword uint32, seconds float64, units Units) uint32 {
	return (dword &^ (limitWindowMask << limitWindowShift)) | WindowEncode(seconds, units.Time)<<limitWindowShift
}

// ReadPower reads and decodes RAPL limits and the SKU register.
func ReadPower(register hardware.Register) PowerState {
	units := ReadUnits(register)
	low, high := register.Read32(RegisterRaplLimit), register.Read32(RegisterRaplLimit+4)
	sku := uint64(register.Read32(RegisterPowerSku)) | uint64(register.Read32(RegisterPowerSku+4))<<32
	return PowerState{
		RawLow: low, RawHigh: high, Locked: high&raplLockBit != 0,
		Sustained: DecodeLimit(low, units),
		Burst:     DecodeLimit(high&^raplLockBit, units),
		TDPWatts:  float64(sku&0x7FFF) / units.Power,
		SkuMin:    float64((sku>>16)&0x7FFF) / units.Power,
		SkuMax:    float64((sku>>32)&0x7FFF) / units.Power,
	}
}

// CheckWatts enforces the hard safety range.
func CheckWatts(name string, watts float64) error {
	if !(watts >= PowerMinimumWatts && watts <= PowerMaximumWatts) { // also rejects NaN
		return fmt.Errorf("%s %g watts outside %g..%g", name, watts, PowerMinimumWatts, PowerMaximumWatts)
	}
	return nil
}

// WritePowerRaw writes the changed RAPL dwords and verifies them.
func WritePowerRaw(register hardware.Register, low, high uint32) error {
	state := ReadPower(register)
	if state.Locked {
		return errors.New("RAPL limit register is locked until power cycle")
	}
	high &^= raplLockBit
	if low != state.RawLow {
		register.Write32(RegisterRaplLimit, low)
	}
	if high != state.RawHigh {
		register.Write32(RegisterRaplLimit+4, high)
	}
	after := ReadPower(register)
	if after.RawLow != low || after.RawHigh != high {
		return fmt.Errorf("power limit read-back 0x%08x_%08x, expected 0x%08x_%08x", after.RawHigh, after.RawLow, high, low)
	}
	return nil
}

// Temperature limit: mailbox commands and allowed range.
const (
	pcodeTemperatureWrite = 0x72
	pcodeTemperatureRead  = 0x73
	TemperatureMinimum    = 60
	TemperatureMaximum    = 95 // Intel's maximum for the A310
)

// ReadTemperatureLimit returns the GPU throttle temperature in Celsius.
func ReadTemperatureLimit(register hardware.Register) (int, error) {
	value, _, err := register.PcodeRead(pcodeTemperatureRead, 0, 0)
	return int(value & 0xFF), err
}

// WriteTemperatureLimit sets and verifies the throttle temperature; checkRange=false
// for the factory value.
func WriteTemperatureLimit(register hardware.Register, celsius int, checkRange bool) error {
	if checkRange && (celsius < TemperatureMinimum || celsius > TemperatureMaximum) {
		return fmt.Errorf("temperature limit %d outside %d..%d", celsius, TemperatureMinimum, TemperatureMaximum)
	}
	if err := register.PcodeWrite(pcodeTemperatureWrite, 0, 0, uint32(celsius), 0); err != nil {
		return err
	}
	// The firmware may still report the old value: poll briefly.
	var read int
	for attempt := 0; attempt < 20; attempt++ {
		value, err := ReadTemperatureLimit(register)
		if err != nil {
			return err
		}
		if read = value; read == celsius {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("temperature limit read-back %d, expected %d", read, celsius)
}

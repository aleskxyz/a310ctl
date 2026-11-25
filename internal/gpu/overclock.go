package gpu

import (
	"fmt"
	"math"

	"a310ctl/internal/hardware"
)

const (
	pcodeOverclockCapabilities = 0x7A // read, subcommand = domain
	pcodeOverclock             = 0x6C // subcommand = domain, parameter selects the operation
	pcodeIccMax                = 0x75 // read, U8.2 amperes
	domainGT                   = 1

	parameterVoltageFrequencyRead  = 0
	parameterVoltageFrequencyWrite = 1
	parameterFrequencyRead         = 2
	parameterFrequencyWrite        = 3
	parameterLock                  = 4 // data0: 0 = unlock, 1 = lock

	OverclockVoltageMaximum = 1120 // millivolts (Intel ceiling)
	OverclockOffsetMaximum  = 150  // millivolts magnitude
	OverclockBoostMaximum   = 300  // megahertz above the reference
	FixedVoltageBelow       = 250  // fixed mode: voltage >= reference - 250

	// Intel IGCL voltage-frequency model V(f) = a*f^2 + b*f (volts, megahertz).
	modelA = 1.8684285714286e-7
	modelB = -4.91815769230768e-4
)

// Capabilities is the decoded overclock capability word (PCODE 0x7A).
type Capabilities struct {
	Raw              uint32 `json:"raw"`
	MaximumMegahertz int    `json:"maximum_megahertz"`
	Ratio            bool   `json:"ratio"`
	VoltageOverride  bool   `json:"voltage_override"`
	VoltageOffset    bool   `json:"voltage_offset"`
	NewInterface     bool   `json:"new_interface"`
}

// Reference is the stock voltage-frequency point (PCODE 0x79).
type Reference struct {
	Megahertz  int `json:"megahertz"`
	Millivolts int `json:"millivolts"`
}

// VoltageFrequency is the overclock word (PCODE 0x6C parameter 0/1).
type VoltageFrequency struct {
	Raw        uint32 `json:"raw"`
	Flag       uint32 `json:"flag"`
	Millivolts int    `json:"voltage_millivolts"`
	Mode       string `json:"voltage_mode"` // adaptive | fixed
	Offset     int    `json:"voltage_offset_millivolts"`
}

// OverclockState is what can be read back from the firmware.
type OverclockState struct {
	VoltageFrequency VoltageFrequency `json:"voltage_frequency"`
	Megahertz        int              `json:"frequency_target_megahertz"`
}

// ReadCapabilities reads PCODE 0x7A for the GT domain.
func ReadCapabilities(register hardware.Register) (Capabilities, error) {
	value, _, err := register.PcodeRead(pcodeOverclockCapabilities, domainGT, 0)
	return Capabilities{
		Raw: value, MaximumMegahertz: int(value&0xFF) * 50,
		Ratio: value&(1<<8) != 0, VoltageOverride: value&(1<<9) != 0,
		VoltageOffset: value&(1<<10) != 0, NewInterface: value&(1<<13) != 0,
	}, err
}

// ReadIccMax returns the maximum current in amperes (PCODE 0x75).
func ReadIccMax(register hardware.Register) (float64, error) {
	value, _, err := register.PcodeRead(pcodeIccMax, 0, 0)
	return float64(value&0x3FF) / 4, err
}

// DecodeVoltageFrequency decodes the overclock word.
func DecodeVoltageFrequency(word uint32) VoltageFrequency {
	offset := int((word >> 21) & 0x7FF)
	if offset&0x400 != 0 {
		offset -= 0x800
	}
	mode := "adaptive"
	if word&(1<<20) != 0 {
		mode = "fixed"
	}
	return VoltageFrequency{
		Raw: word, Flag: word & 1, Mode: mode, Offset: offset,
		Millivolts: int(math.Round(float64((word>>8)&0xFFF) * 1000 / 1024)),
	}
}

// EncodeVoltageFrequency builds the overclock word: [19:8] voltage in 1/1024 V,
// [20] fixed mode, [31:21] signed offset in mV.
func EncodeVoltageFrequency(millivolts, offset int, fixed bool) uint32 {
	word := (uint32(math.Round(float64(millivolts)*1024/1000)) & 0xFFF) << 8
	if fixed {
		word |= 1 << 20
	}
	word |= (uint32(offset) & 0x7FF) << 21
	return word
}

// ReadOverclock reads the programmed voltage-frequency word and frequency target.
func ReadOverclock(register hardware.Register) (OverclockState, error) {
	word, _, err := register.PcodeRead(pcodeOverclock, domainGT, parameterVoltageFrequencyRead)
	if err != nil {
		return OverclockState{}, err
	}
	megahertz, _, err := register.PcodeRead(pcodeOverclock, domainGT, parameterFrequencyRead)
	return OverclockState{VoltageFrequency: DecodeVoltageFrequency(word), Megahertz: int(megahertz)}, err
}

// Unlock unlocks overclocking on both domains; this also lifts the ~31 W firmware cap.
func Unlock(register hardware.Register) error {
	_ = register.PcodeWrite(pcodeOverclock, 0, parameterLock, 0, 0) // domain 0 may reject
	return register.PcodeWrite(pcodeOverclock, domainGT, parameterLock, 0, 0)
}

// Lock relocks overclocking (the ~31 W cap returns).
func Lock(register hardware.Register) error {
	return register.PcodeWrite(pcodeOverclock, domainGT, parameterLock, 1, 0)
}

// ResetVoltageFrequency writes the stock voltage-frequency word and frequency target.
func ResetVoltageFrequency(register hardware.Register, frequencyTarget int) error {
	if err := register.PcodeWrite(pcodeOverclock, domainGT, parameterVoltageFrequencyWrite, 0, 0); err != nil {
		return err
	}
	return register.PcodeWrite(pcodeOverclock, domainGT, parameterFrequencyWrite, uint32(frequencyTarget), 0)
}

// Point is a raw voltage-frequency request.
type Point struct {
	Megahertz  int
	Millivolts int // target, 0 = stock
	Offset     int // millivolts, negative = undervolt
	Fixed      bool
}

// BoostPlan reproduces Intel's "Performance Boost" plus an extra undervolt; boost 0
// gives a pure offset at the reference frequency.
func BoostPlan(reference Reference, boost, undervolt int) Point {
	model := func(f float64) float64 { return modelA*f*f + modelB*f }
	f0 := float64(reference.Megahertz)
	delta := int(math.Round((model(f0+float64(boost)) - model(f0)) * 1000))
	point := Point{Megahertz: reference.Megahertz + boost, Offset: -delta - undervolt}
	if boost > 0 {
		point.Millivolts = reference.Millivolts + delta
	}
	return point
}

// CheckPoint enforces the safety limits for a voltage-frequency request.
func CheckPoint(point Point, reference Reference, maximumStockMegahertz int) error {
	if point.Megahertz < 0 || point.Megahertz > maximumStockMegahertz+OverclockBoostMaximum {
		return fmt.Errorf("frequency %d outside 0..%d megahertz", point.Megahertz, maximumStockMegahertz+OverclockBoostMaximum)
	}
	if point.Millivolts < 0 || point.Millivolts > OverclockVoltageMaximum {
		return fmt.Errorf("voltage %d outside 0..%d millivolts", point.Millivolts, OverclockVoltageMaximum)
	}
	if point.Offset < -OverclockOffsetMaximum || point.Offset > OverclockOffsetMaximum {
		return fmt.Errorf("voltage offset %d outside -%d..%d millivolts", point.Offset, OverclockOffsetMaximum, OverclockOffsetMaximum)
	}
	if point.Fixed && point.Millivolts < reference.Millivolts-FixedVoltageBelow {
		return fmt.Errorf("fixed voltage %d below %d millivolts (reference - %d)", point.Millivolts, reference.Millivolts-FixedVoltageBelow, FixedVoltageBelow)
	}
	return nil
}

// WriteVoltageFrequency unlocks, writes word and frequency, and verifies (Windows order).
func WriteVoltageFrequency(register hardware.Register, point Point) error {
	capabilities, err := ReadCapabilities(register)
	if err != nil {
		return err
	}
	if !capabilities.NewInterface {
		return fmt.Errorf("card reports the legacy overclock interface (0x%08x), not supported", capabilities.Raw)
	}
	if err := Unlock(register); err != nil {
		return err
	}
	word := EncodeVoltageFrequency(point.Millivolts, point.Offset, point.Fixed)
	if err := register.PcodeWrite(pcodeOverclock, domainGT, parameterVoltageFrequencyWrite, word, 0); err != nil {
		return err
	}
	if err := register.PcodeWrite(pcodeOverclock, domainGT, parameterFrequencyWrite, uint32(point.Megahertz), 0); err != nil {
		return err
	}
	state, err := ReadOverclock(register)
	if err != nil {
		return err
	}
	vf := state.VoltageFrequency
	wantMode := "adaptive"
	if point.Fixed {
		wantMode = "fixed"
	}
	if abs(vf.Millivolts-point.Millivolts) > 5 || vf.Offset != point.Offset || vf.Mode != wantMode || abs(state.Megahertz-point.Megahertz) > 5 {
		return fmt.Errorf("overclock read-back word 0x%08x frequency %d does not match the request (firmware rejected or clamped)", vf.Raw, state.Megahertz)
	}
	return nil
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

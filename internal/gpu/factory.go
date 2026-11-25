package gpu

// FactoryValues is the card's state after a power cycle, before anything is changed.
type FactoryValues struct {
	RaplLow         uint32    `json:"rapl_low"`  // sustained limit dword
	RaplHigh        uint32    `json:"rapl_high"` // burst limit dword (lock bit clear)
	Temperature     int       `json:"temperature_limit_celsius"`
	IccMax          float64   `json:"iccmax_amperes"`
	Reference       Reference `json:"reference"`                            // read while locked
	FrequencyTarget int       `json:"overclock_frequency_target_megahertz"` // 0x6C parameter 2
}

// Factory values read from a Sparkle A310 (172f:4019) after power cycles: 31.25 W
// sustained and burst, 28 s window, 90 C, 100 A, 2450 MHz at 910 mV, frequency target 0.
// Fan control and the overclock word have firmware-native resets.
var Factory = FactoryValues{
	RaplLow:         0x00dc80fa,
	RaplHigh:        0x00dc80fa,
	Temperature:     90,
	IccMax:          100,
	Reference:       Reference{Megahertz: 2450, Millivolts: 910},
	FrequencyTarget: 0,
}

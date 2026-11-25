package gpu

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"a310ctl/internal/hardware"
)

// Telemetry registers. GT registers read 0 while the GT is idle (RC6).
const (
	registerVoltageFrequency = 0x1381B4 // [10:0] x2.5 mV, [19:11] x50/3 MHz
	registerThrottle         = 0x145900 // limit reasons, see ThrottleReasons
	registerVramTemperature  = 0x1382C0 // [7:0] C, raw 0x18 = invalid
	registerRequestedClock   = 0xA008   // [31:23] round(raw/3)*50 MHz
	registerFanTach          = 0x138140 // 2 pulses per revolution
	registerPackageTemp      = 0x145978 // [7:0] C
	pcodeVramRate            = 0x6F     // subcommand 1: megatransfers per second
	vramBusBits              = 64
	FanFullSpeedRPM          = 6650 // measured at 100 percent duty on this card
)

var (
	busyRegisters = map[string]uint32{"global": 0xDB80, "render": 0xDBDC, "media": 0xDBA0}
	vramReadRegs  = append(span(0x36C448, 0x36C4C0), span(0x36C548, 0x36C5C0)...)
	vramWriteRegs = span(0x36C4C8, 0x36C540)
)

func span(first, last uint32) []uint32 {
	var out []uint32
	for offset := first; offset <= last; offset += 8 {
		out = append(out, offset)
	}
	return out
}

// Snapshot holds raw counters at one instant.
type Snapshot struct {
	Time   time.Time
	Energy uint32
	Tach   uint32
	Read   uint64
	Write  uint64
	Busy   map[string]uint32
}

// TakeSnapshot reads all counters.
func TakeSnapshot(register hardware.Register) Snapshot {
	read64 := func(offsets []uint32) uint64 {
		var sum uint64
		for _, offset := range offsets {
			sum += ReadCounter64(register, offset)
		}
		return sum
	}
	busy := map[string]uint32{}
	for name, offset := range busyRegisters {
		busy[name] = register.Read32(offset)
	}
	return Snapshot{
		Time:   time.Now(),
		Energy: register.Read32(RegisterEnergyStatus), Tach: register.Read32(registerFanTach),
		Read: read64(vramReadRegs), Write: read64(vramWriteRegs), Busy: busy,
	}
}

// ReadCounter64 reads a 64-bit counter without tearing: high, low, high, retried while
// the high half changes.
func ReadCounter64(register hardware.Register, offset uint32) uint64 {
	high := register.Read32(offset + 4)
	var low uint32
	for attempt := 0; attempt < 10; attempt++ {
		low = register.Read32(offset)
		again := register.Read32(offset + 4)
		if again == high {
			break
		}
		high = again
	}
	return uint64(high)<<32 | uint64(low)
}

// Sensors is the decoded telemetry over one interval.
type Sensors struct {
	GPUTemperature   int                `json:"gpu_temperature_celsius"`
	VramTemperature  int                `json:"vram_temperature_celsius"` // -1 = invalid
	PowerWatts       float64            `json:"power_watts"`
	EnergyJoules     float64            `json:"energy_joules_interval"`
	VoltageMillivolt float64            `json:"gpu_voltage_millivolts"`
	ActualMegahertz  int                `json:"gpu_frequency_megahertz"`
	RequestMegahertz int                `json:"gpu_requested_megahertz"`
	FanRPM           int                `json:"fan_rpm"`
	FanPercent       int                `json:"fan_percent"`
	Busy             map[string]float64 `json:"busy_percent"`
	VramReadGBs      float64            `json:"vram_read_gigabytes_per_second"`
	VramWriteGBs     float64            `json:"vram_write_gigabytes_per_second"`
	VramReadBytes    float64            `json:"vram_read_bytes_interval"`
	VramWriteBytes   float64            `json:"vram_write_bytes_interval"`
	VramUtilization  float64            `json:"vram_bandwidth_utilization_percent"`
	VramRate         int                `json:"vram_megatransfers"`
	VramMegahertz    int                `json:"vram_megahertz"`
	VramUsedBytes    int64              `json:"vram_used_bytes"`
	Available        bool               `json:"available"` // false: card read all ones
	ThrottleRaw      uint32             `json:"throttle_raw"`
	Throttle         map[string]bool    `json:"throttle"`
	IntervalSeconds  float64            `json:"interval_seconds"`
}

// ThrottleReasons decodes the limit-reason register with IGCL's masks.
func ThrottleReasons(raw uint32) map[string]bool {
	return map[string]bool{
		"power":           raw&0xC00 != 0,
		"current_voltage": raw&0x100 != 0,
		"thermal":         raw&0xFB != 0,
		"utilization":     raw&0x1000 != 0,
	}
}

// Compute derives sensors from two snapshots; VramUsedBytes is left to the caller.
func Compute(register hardware.Register, before, after Snapshot) Sensors {
	if register.Read32(RegisterPowerUnits) == 0xFFFFFFFF {
		return Sensors{Busy: map[string]float64{}, Throttle: map[string]bool{}}
	}
	units := ReadUnits(register)
	seconds := after.Time.Sub(before.Time).Seconds()
	if seconds <= 0 {
		seconds = 1e-3
	}
	delta := func(a, b uint32) float64 { return float64(b - a) } // uint32 wraps naturally
	// Busy counters tick at 19.2 MHz / 16; use wall-clock time (the GT timestamp stops in RC6).
	ticks := seconds * 19.2e6 / 16
	vf := register.Read32(registerVoltageFrequency)
	throttle := register.Read32(registerThrottle)
	vramTemp := int(register.Read32(registerVramTemperature) & 0xFF)
	if vramTemp == 0x18 {
		vramTemp = -1
	}
	energy := delta(before.Energy, after.Energy) / units.Energy
	sensors := Sensors{
		GPUTemperature:   int(register.Read32(registerPackageTemp) & 0xFF),
		VramTemperature:  vramTemp,
		EnergyJoules:     energy,
		PowerWatts:       round(energy/seconds, 1),
		VoltageMillivolt: float64(vf&0x7FF) * 2.5,
		ActualMegahertz:  int(math.Round(float64((vf>>11)&0x1FF) * 50 / 3)),
		RequestMegahertz: int(math.Round(float64((register.Read32(registerRequestedClock)>>23)&0x1FF)/3)) * 50,
		FanRPM:           int(math.Round(delta(before.Tach, after.Tach) / 2 / seconds * 60)),
		Busy:             map[string]float64{},
		VramReadBytes:    float64(after.Read-before.Read) * 32,
		VramWriteBytes:   float64(after.Write-before.Write) * 32,
		Available:        true,
		ThrottleRaw:      throttle,
		Throttle:         ThrottleReasons(throttle),
		IntervalSeconds:  round(seconds, 3),
	}
	sensors.FanPercent = int(math.Min(100, math.Round(float64(sensors.FanRPM)/FanFullSpeedRPM*100)))
	sensors.VramReadGBs = round(sensors.VramReadBytes/seconds/1e9, 2)
	sensors.VramWriteGBs = round(sensors.VramWriteBytes/seconds/1e9, 2)
	for name := range busyRegisters {
		sensors.Busy[name] = round(math.Min(100, delta(before.Busy[name], after.Busy[name])/ticks*100), 1)
	}
	if rate, _, err := register.PcodeRead(pcodeVramRate, 1, 0); err == nil && rate > 0 {
		sensors.VramRate = int(rate)
		sensors.VramMegahertz = int(math.Round(float64(rate) / 8))
		peak := float64(rate) * 1e6 * vramBusBits / 8 / 1e9
		sensors.VramUtilization = round((sensors.VramReadGBs+sensors.VramWriteGBs)/peak*100, 1)
	}
	return sensors
}

func round(value float64, digits int) float64 {
	scale := math.Pow(10, float64(digits))
	return math.Round(value*scale) / scale
}

var (
	fdinfoClient   = regexp.MustCompile(`drm-client-id:\s*(\d+)`)
	fdinfoResident = regexp.MustCompile(`drm-resident-(?:local0|vram0):[ \t]*(\d+)[ \t]*(KiB|MiB|GiB)?`)

	vramCacheLock  sync.Mutex
	vramCacheAddr  string
	vramCacheBytes int64
	vramCacheUntil time.Time
)

const vramCacheTTL = 2 * time.Second

// VramUsedBytes sums drm-resident-local0 over the GPU's DRM clients (i915/xe fdinfo).
func VramUsedBytes(address string) int64 {
	now := time.Now()
	vramCacheLock.Lock()
	if address == vramCacheAddr && now.Before(vramCacheUntil) {
		value := vramCacheBytes
		vramCacheLock.Unlock()
		return value
	}
	vramCacheLock.Unlock()
	total := scanVramUsed(address)
	vramCacheLock.Lock()
	vramCacheAddr, vramCacheBytes, vramCacheUntil = address, total, now.Add(vramCacheTTL)
	vramCacheLock.Unlock()
	return total
}

func scanVramUsed(address string) int64 {
	files, _ := filepath.Glob("/proc/[0-9]*/fdinfo/*")
	seen := map[string]bool{}
	var total int64
	marker := "drm-pdev:\t" + address
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil || !strings.Contains(string(data), marker) {
			continue
		}
		client := fdinfoClient.FindStringSubmatch(string(data))
		resident := fdinfoResident.FindStringSubmatch(string(data))
		if client == nil || resident == nil || seen[client[1]] {
			continue
		}
		seen[client[1]] = true
		value, _ := strconv.ParseInt(resident[1], 10, 64)
		total += value * map[string]int64{"": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30}[resident[2]]
	}
	return total
}

// Pcie is the real link (DG2 internal switch upstream port) and BAR sizes.
type Pcie struct {
	SpeedGT      float64 `json:"link_speed_gigatransfers"`
	Width        int     `json:"link_width"`
	MaxSpeedGT   float64 `json:"maximum_link_speed_gigatransfers"`
	MaxWidth     int     `json:"maximum_link_width"`
	BarMegabytes []int   `json:"bar_megabytes"`
}

func parseSpeed(path string) float64 {
	data, _ := os.ReadFile(path)
	value, _ := strconv.ParseFloat(strings.Fields(string(data) + " 0")[0], 64)
	return value
}

// ReadPcie reads the DG2 switch upstream port (8086:4fa0/4fa1): the GPU's own link is a
// virtual 2.5 GT/s x1.
func ReadPcie(sysPath string) Pcie {
	real, _ := filepath.EvalSymlinks(sysPath)
	link := sysPath
	for _, component := range strings.Split(real, "/") {
		base := filepath.Join("/sys/bus/pci/devices", component)
		vendor, err1 := os.ReadFile(filepath.Join(base, "vendor"))
		device, err2 := os.ReadFile(filepath.Join(base, "device"))
		if err1 == nil && err2 == nil && strings.TrimSpace(string(vendor)) == "0x8086" {
			if id := strings.TrimSpace(string(device)); id == "0x4fa0" || id == "0x4fa1" {
				link = base
				break
			}
		}
	}
	pcie := Pcie{
		SpeedGT: parseSpeed(filepath.Join(link, "current_link_speed")), Width: readInteger(filepath.Join(link, "current_link_width")),
		MaxSpeedGT: parseSpeed(filepath.Join(link, "max_link_speed")), MaxWidth: readInteger(filepath.Join(link, "max_link_width")),
	}
	if file, err := os.Open(filepath.Join(sysPath, "resource")); err == nil {
		scanner := bufio.NewScanner(file)
		for index := 0; scanner.Scan() && index < 6; index++ {
			var start, end, flags uint64
			fmt.Sscanf(scanner.Text(), "0x%x 0x%x 0x%x", &start, &end, &flags)
			size := 0
			if end != 0 {
				size = int((end - start + 1) >> 20)
			}
			pcie.BarMegabytes = append(pcie.BarMegabytes, size)
		}
		file.Close()
	}
	return pcie
}

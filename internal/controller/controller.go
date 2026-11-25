// Package controller serializes hardware access and applies configuration groups.
package controller

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"a310ctl/internal/config"
	"a310ctl/internal/gpu"
	"a310ctl/internal/hardware"
)

// Groups accepted by Reset.
var Groups = []string{"all", "power", "temperature", "overclock"}

// ErrClosed is returned after Close.
var ErrClosed = errors.New("device closed")

// Applied is write-only state, as last set by a310ctl.
type Applied struct {
	Unlocked     *bool           `json:"overclock_unlocked,omitempty"`
	FanMode      string          `json:"fan_mode"` // points | flat | factory | unknown
	FanPoints    []gpu.FanPoint  `json:"fan_points,omitempty"`
	Boost        int             `json:"overclock_boost_megahertz"`
	Undervolt    int             `json:"overclock_undervolt_millivolts"`
	PowerMarker  bool            `json:"power_reset_marker"` // burst limit is factory + 1 step
	ResetActive  map[string]bool `json:"reset_active"`
	LastApply    time.Time       `json:"last_apply"`
	ApplySuccess bool            `json:"apply_success"`
	ApplyErrors  []string        `json:"apply_errors,omitempty"`
	ConfigPath   string          `json:"config_path,omitempty"`
	ConfigHash   string          `json:"config_sha256,omitempty"`
}

// Status is everything readable from the card plus the applied state.
type Status struct {
	Address          string             `json:"pci_address"`
	Driver           string             `json:"driver"`
	Subsystem        string             `json:"subsystem"`
	Pcie             gpu.Pcie           `json:"pcie"`
	Power            gpu.PowerState     `json:"power"`
	TemperatureLimit int                `json:"temperature_limit_celsius"`
	IccMax           *float64           `json:"iccmax_amperes,omitempty"`
	Overclock        gpu.OverclockState `json:"overclock"`
	Capabilities     *gpu.Capabilities  `json:"overclock_capabilities,omitempty"`
	Reference        gpu.Reference      `json:"reference"`
	Clock            gpu.Clock          `json:"clock"`
	Factory          gpu.FactoryValues  `json:"factory"`
	Applied          Applied            `json:"applied"`
	Daemon           bool               `json:"daemon"`
	Available        bool               `json:"available"`
	Errors           []string           `json:"errors,omitempty"`
}

const (
	SensorsWindowMin = 500 * time.Millisecond
	SensorsWindowMax = 10 * time.Second
)

// MarkerActive reports that the burst limit holds the reset marker right now.
func (status Status) MarkerActive() bool {
	return status.Applied.PowerMarker && status.Power.RawHigh&0x7FFFFFFF == status.Factory.RaplHigh+1
}

// expected is what readable registers should hold, for power-loss detection (after a
// failed write: the read-back). The overclock word is not tracked: the firmware may clear
// it after a GPU hang, and re-applying that undervolt would be wrong.
type expected struct {
	raplKnown         bool
	raplLow, raplHigh uint32
	temperatureKnown  bool
	temperature       int
}

// temperatureCheckEvery limits PowerLost's mailbox reads (shared with the kernel driver).
const temperatureCheckEvery = 5

// Controller owns the opened device.
type Controller struct {
	mutex   sync.Mutex
	device  hardware.Access
	closed  bool
	applied Applied
	current *config.Config
	failed  map[string]string // group or "reset marker" -> error; retried
	// group -> failed factory reset; reported, not retried (that would undo the reset)
	resetFailed map[string]string
	expect      expected
	checks      int // PowerLost calls

	// Read once: they never change (saves mailbox traffic).
	capabilities *gpu.Capabilities
	iccMax       *float64
}

// New wraps an opened device (or a test fake).
func New(device hardware.Access) *Controller {
	return &Controller{
		device:      device,
		applied:     Applied{FanMode: "unknown", ResetActive: map[string]bool{}},
		failed:      map[string]string{},
		resetFailed: map[string]string{},
		expect:      factoryExpectation(),
	}
}

// factoryExpectation is the state after a power cycle.
func factoryExpectation() expected {
	return expected{raplKnown: true, raplLow: gpu.Factory.RaplLow, raplHigh: gpu.Factory.RaplHigh,
		temperatureKnown: true, temperature: gpu.Factory.Temperature}
}

// Close releases the device; later calls return ErrClosed or do nothing.
func (controller *Controller) Close() {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if !controller.closed {
		controller.closed = true
		controller.device.Close()
	}
}

// reference is the factory voltage-frequency point (a live read drifts while unlocked).
func (controller *Controller) reference() gpu.Reference { return gpu.Factory.Reference }

// Apply applies the configuration: all groups if full, else changed or failed ones.
// Omitted groups are never touched.
func (controller *Controller) Apply(next *config.Config, full bool) []error {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.closed {
		return []error{ErrClosed}
	}
	return controller.apply(next, full, false)
}

// RetryFailed re-applies only failed groups (and the reset marker); reset groups stay reset.
func (controller *Controller) RetryFailed(next *config.Config) []error {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.closed {
		return []error{ErrClosed}
	}
	return controller.apply(next, false, true)
}

// ApplyAfterPowerLoss re-applies everything after a suspend or card reset; reset groups
// stay reset.
func (controller *Controller) ApplyAfterPowerLoss(next *config.Config) []error {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.closed {
		return []error{ErrClosed}
	}
	controller.setUnlocked(false)
	controller.applied.FanMode, controller.applied.FanPoints = "factory", nil
	controller.applied.Boost, controller.applied.Undervolt = 0, 0
	controller.applied.PowerMarker = false
	controller.expect = factoryExpectation()
	return controller.apply(next, true, true)
}

// Ready reports whether the card answers (false while resuming).
func (controller *Controller) Ready() bool {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	return !controller.closed && controller.device.Read32(gpu.RegisterPowerUnits) != 0xFFFFFFFF
}

// PowerLost reports that the card lost its settings: it reads all ones, or a changed
// register reads factory again (see updateMarker).
func (controller *Controller) PowerLost() bool {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.closed {
		return false
	}
	if controller.device.Read32(gpu.RegisterPowerUnits) == 0xFFFFFFFF {
		return true
	}
	e := controller.expect
	if e.raplKnown && (e.raplLow != gpu.Factory.RaplLow || e.raplHigh != gpu.Factory.RaplHigh) {
		state := gpu.ReadPower(controller.device)
		// A reset clears both registers: no need for the temperature mailbox read.
		return state.RawLow == gpu.Factory.RaplLow && state.RawHigh&0x7FFFFFFF == gpu.Factory.RaplHigh
	}
	// Power group omitted: fall back to the temperature limit.
	controller.checks++
	if e.temperatureKnown && e.temperature != gpu.Factory.Temperature && controller.checks%temperatureCheckEvery == 0 {
		if limit, err := gpu.ReadTemperatureLimit(controller.device); err == nil && limit == gpu.Factory.Temperature {
			return true
		}
	}
	return false
}

// HasFailures reports whether a group failed to apply.
func (controller *Controller) HasFailures() bool {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	return len(controller.failed) > 0
}

// apply writes the groups that changed, failed last time or were reset since (all on a
// full apply), then updates the reset marker. keepResets leaves reset groups alone.
func (controller *Controller) apply(next *config.Config, full, keepResets bool) []error {
	previous := controller.current
	changed := func(group string, get func(*config.Config) any) bool {
		if keepResets && controller.applied.ResetActive[group] {
			return false
		}
		return full || previous == nil || controller.applied.ResetActive[group] ||
			controller.failed[group] != "" || !reflect.DeepEqual(get(previous), get(next))
	}
	var errs []error
	record := func(group string, err error) {
		if err != nil {
			controller.failed[group] = err.Error()
			errs = append(errs, fmt.Errorf("%s: %w", group, err))
		} else {
			delete(controller.failed, group)
		}
	}
	overclockChanged := changed("overclock", func(c *config.Config) any { return c.Overclock }) && next.Overclock.Present
	powerChanged := changed("power", func(c *config.Config) any { return c.Power }) && next.Power.Present
	temperatureChanged := changed("temperature", func(c *config.Config) any { return c.Temperature }) && next.Temperature.Present
	var overclockErr error
	if overclockChanged {
		overclockErr = controller.applyUnlock(next.Overclock)
		delete(controller.applied.ResetActive, "overclock")
		delete(controller.resetFailed, "overclock")
	}
	if powerChanged {
		record("power", controller.applyPower(next.Power))
		delete(controller.applied.ResetActive, "power")
		delete(controller.resetFailed, "power")
	}
	if temperatureChanged {
		record("temperature", controller.applyTemperature(next.Temperature))
		delete(controller.applied.ResetActive, "temperature")
		delete(controller.resetFailed, "temperature")
	}
	if overclockChanged {
		if overclockErr == nil {
			overclockErr = controller.applyVoltageFrequency(next.Overclock)
		}
		record("overclock", overclockErr)
	}
	// Stop retrying groups that are no longer in the file.
	for group := range controller.failed {
		if !groupPresent(next, group) {
			delete(controller.failed, group)
		}
	}
	controller.current = next
	if err := controller.recordMarker(); err != nil {
		errs = append(errs, fmt.Errorf("reset marker: %w", err))
	}
	controller.applied.LastApply = time.Now()
	controller.summarize()
	controller.applied.ConfigPath, controller.applied.ConfigHash = next.Path, next.Hash
	return errs
}

// summarize updates ApplySuccess and ApplyErrors from failed and resetFailed.
func (controller *Controller) summarize() {
	controller.applied.ApplySuccess = len(controller.failed) == 0 && len(controller.resetFailed) == 0
	controller.applied.ApplyErrors = nil
	for _, group := range sortedKeys(controller.failed) {
		controller.applied.ApplyErrors = append(controller.applied.ApplyErrors, group+": "+controller.failed[group])
	}
	for _, group := range sortedKeys(controller.resetFailed) {
		controller.applied.ApplyErrors = append(controller.applied.ApplyErrors, group+": factory reset: "+controller.resetFailed[group])
	}
}

func groupPresent(c *config.Config, group string) bool {
	switch group {
	case "power":
		return c.Power.Present
	case "temperature":
		return c.Temperature.Present
	case "overclock":
		return c.Overclock.Present
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (controller *Controller) setUnlocked(value bool) { controller.applied.Unlocked = &value }

// stockVoltageFrequency writes the stock overclock word and frequency target.
func (controller *Controller) stockVoltageFrequency() error {
	if err := gpu.ResetVoltageFrequency(controller.device, gpu.Factory.FrequencyTarget); err != nil {
		return err
	}
	controller.applied.Boost, controller.applied.Undervolt = 0, 0
	return nil
}

func hasValue[T any](setting config.Setting[T]) bool { return setting.Set && !setting.Factory }

// wantsVoltageFrequency reports a non-factory voltage-frequency key (implies unlock).
func wantsVoltageFrequency(group config.Overclock) bool {
	return hasValue(group.Boost) || hasValue(group.Undervolt) || hasValue(group.Frequency) ||
		hasValue(group.Voltage) || hasValue(group.VoltageOffset) || hasValue(group.VoltageMode)
}

// applyUnlock locks or unlocks overclocking as configured.
func (controller *Controller) applyUnlock(group config.Overclock) error {
	unlock := wantsVoltageFrequency(group)
	switch {
	case group.Factory:
		unlock = false
	case group.Unlock.Set:
		unlock = !group.Unlock.Factory && group.Unlock.Value
	case !unlock:
		return nil // nothing to change
	}
	if unlock {
		if err := gpu.Unlock(controller.device); err != nil {
			return err
		}
		controller.setUnlocked(true)
		return nil
	}
	// Relock like the Windows reset: stock voltage-frequency first.
	if err := controller.stockVoltageFrequency(); err != nil {
		return err
	}
	if err := gpu.Lock(controller.device); err != nil {
		return err
	}
	controller.setUnlocked(false)
	return nil
}

// applyVoltageFrequency writes the boost/undervolt or raw voltage-frequency point.
func (controller *Controller) applyVoltageFrequency(group config.Overclock) error {
	if group.Factory {
		return nil // done by applyUnlock
	}
	reference := controller.reference()
	var point gpu.Point
	switch {
	case group.Boost.Set || group.Undervolt.Set:
		boost, undervolt := 0, 0
		if hasValue(group.Boost) {
			boost = group.Boost.Value
		}
		if hasValue(group.Undervolt) {
			undervolt = group.Undervolt.Value
		}
		if boost == 0 && undervolt == 0 {
			return controller.stockVoltageFrequency()
		}
		point = gpu.BoostPlan(reference, boost, undervolt)
		controller.applied.Boost, controller.applied.Undervolt = boost, undervolt
	case group.Frequency.Set || group.Voltage.Set || group.VoltageOffset.Set || group.VoltageMode.Set:
		point = gpu.Point{Megahertz: reference.Megahertz}
		if hasValue(group.Frequency) {
			point.Megahertz = group.Frequency.Value
		}
		if hasValue(group.Voltage) {
			point.Millivolts = group.Voltage.Value
		}
		if hasValue(group.VoltageOffset) {
			point.Offset = group.VoltageOffset.Value
		}
		point.Fixed = hasValue(group.VoltageMode) && group.VoltageMode.Value == "fixed"
		if point.Millivolts == 0 && point.Offset == 0 && !point.Fixed && point.Megahertz == reference.Megahertz {
			return controller.stockVoltageFrequency()
		}
		controller.applied.Boost, controller.applied.Undervolt = 0, 0
	default:
		return nil
	}
	// Cap against the built-in RP0, not a live read.
	if err := gpu.CheckPoint(point, reference, reference.Megahertz); err != nil {
		return err
	}
	if err := gpu.WriteVoltageFrequency(controller.device, point); err != nil {
		return err
	}
	controller.setUnlocked(true)
	return nil
}

// applyPower read-modify-writes the RAPL limits.
func (controller *Controller) applyPower(group config.Power) error {
	if group.Factory {
		return controller.writePower(gpu.Factory.RaplLow, gpu.Factory.RaplHigh)
	}
	state := gpu.ReadPower(controller.device)
	units := gpu.ReadUnits(controller.device)
	low, high := state.RawLow, state.RawHigh
	if controller.applied.PowerMarker && high&0x7FFFFFFF == markerHigh {
		high = high&^0x7FFFFFFF | gpu.Factory.RaplHigh // drop the marker step
	}
	const valueAndEnable = 0xFFFF // value [14:0] + enable [15]
	const window = 0x7F << 17     // time window [23:17]
	var err error
	switch {
	case group.Sustained.Factory:
		low = low&^valueAndEnable | gpu.Factory.RaplLow&valueAndEnable
	case group.Sustained.Set:
		if low, err = gpu.EncodeWatts(low, group.Sustained.Value, units); err != nil {
			return err
		}
	}
	switch {
	case group.Burst.Factory:
		high = high&^valueAndEnable | gpu.Factory.RaplHigh&valueAndEnable
	case group.Burst.Set:
		if high, err = gpu.EncodeWatts(high, group.Burst.Value, units); err != nil {
			return err
		}
	}
	switch {
	case group.Window.Factory:
		low = low&^window | gpu.Factory.RaplLow&window
	case group.Window.Set:
		low = gpu.EncodeWindow(low, group.Window.Value, units)
	}
	if low == state.RawLow && high == state.RawHigh {
		controller.expectPower(low, high)
		return nil
	}
	return controller.writePower(low, high)
}

func (controller *Controller) expectPower(low, high uint32) {
	controller.expect.raplKnown, controller.expect.raplLow, controller.expect.raplHigh = true, low, high&0x7FFFFFFF
	if high&0x7FFFFFFF != markerHigh {
		controller.applied.PowerMarker = false
	}
}

// writePower writes RAPL and records it for power-loss detection.
func (controller *Controller) writePower(low, high uint32) error {
	controller.applied.PowerMarker = false
	if err := gpu.WritePowerRaw(controller.device, low, high); err != nil {
		if controller.device.Read32(gpu.RegisterPowerUnits) == 0xFFFFFFFF {
			controller.expect.raplKnown = false
		} else {
			state := gpu.ReadPower(controller.device)
			controller.expectPower(state.RawLow, state.RawHigh)
		}
		return err
	}
	controller.expectPower(low, high)
	return nil
}

// markerHigh is the factory burst limit plus one step (1/8 W on the A310).
var markerHigh = gpu.Factory.RaplHigh + 1

// updateMarker raises the factory burst limit by one step while other settings are not
// factory, so a card reset (exact factory) is detectable. No effect while locked (~31.25 W
// firmware cap). Only for a power group in the file or reset; removed when not needed.
func (controller *Controller) updateMarker() error {
	e, a := controller.expect, controller.applied
	owned := a.ResetActive["power"] || (controller.current != nil && controller.current.Power.Present)
	need := owned && ((a.Unlocked != nil && *a.Unlocked) || a.FanMode == "points" || a.FanMode == "flat" ||
		!e.temperatureKnown || e.temperature != gpu.Factory.Temperature)
	atFactory := e.raplKnown && e.raplLow == gpu.Factory.RaplLow && e.raplHigh == gpu.Factory.RaplHigh
	marked := a.PowerMarker && e.raplKnown && e.raplLow == gpu.Factory.RaplLow && e.raplHigh == markerHigh
	switch {
	case need && atFactory:
		if err := controller.writePower(gpu.Factory.RaplLow, markerHigh); err != nil {
			return err
		}
		controller.applied.PowerMarker = true
	case !need && marked:
		return controller.writePower(gpu.Factory.RaplLow, gpu.Factory.RaplHigh)
	}
	return nil
}

// recordMarker runs updateMarker; a failure is retried like a failed group.
func (controller *Controller) recordMarker() error {
	err := controller.updateMarker()
	if err != nil {
		controller.failed["reset marker"] = err.Error()
	} else {
		delete(controller.failed, "reset marker")
	}
	return err
}

// writeTemperature sets the limit and records it for power-loss detection.
func (controller *Controller) writeTemperature(celsius int, checkRange bool) error {
	if err := gpu.WriteTemperatureLimit(controller.device, celsius, checkRange); err != nil {
		if limit, readErr := gpu.ReadTemperatureLimit(controller.device); readErr == nil {
			controller.expect.temperatureKnown, controller.expect.temperature = true, limit
		} else {
			controller.expect.temperatureKnown = false
		}
		return err
	}
	controller.expect.temperatureKnown, controller.expect.temperature = true, celsius
	return nil
}

// applyTemperature sets the temperature limit and the fan curve.
func (controller *Controller) applyTemperature(group config.Temperature) error {
	var errs []error
	switch {
	case group.Factory || group.Limit.Factory:
		errs = append(errs, controller.writeTemperature(gpu.Factory.Temperature, false))
	case group.Limit.Set:
		errs = append(errs, controller.writeTemperature(group.Limit.Value, true))
	}
	switch {
	case group.Factory || group.FanCurve.Factory:
		if err := gpu.FactoryFan(controller.device); err != nil {
			errs = append(errs, err)
			controller.applied.FanMode, controller.applied.FanPoints = "unknown", nil
		} else {
			controller.applied.FanMode, controller.applied.FanPoints = "factory", nil
		}
	case group.FanCurve.Set:
		if err := gpu.WriteFanCurve(controller.device, group.FanCurve.Points); err != nil {
			// A half-written table is undefined: fall back to the firmware curve.
			if gpu.FactoryFan(controller.device) == nil {
				controller.applied.FanMode, controller.applied.FanPoints = "factory", nil
			} else {
				controller.applied.FanMode, controller.applied.FanPoints = "unknown", nil
			}
			errs = append(errs, fmt.Errorf("fan curve: %w (fan returned to factory control)", err))
		} else {
			controller.applied.FanMode, controller.applied.FanPoints = "points", group.FanCurve.Points
			if group.FanCurve.Text == "" {
				controller.applied.FanMode = "flat"
			}
		}
	}
	return errors.Join(errs...)
}

// Reset restores factory settings for a group (see Groups).
func (controller *Controller) Reset(group string) error {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.closed {
		return ErrClosed
	}
	switch group {
	case "all", "overclock", "power", "temperature":
	default:
		return fmt.Errorf("unknown group %q (use %v)", group, Groups)
	}
	var errs []error
	do := func(name string, err error) {
		delete(controller.failed, name) // a retry would undo the reset
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			controller.resetFailed[name] = err.Error()
			delete(controller.applied.ResetActive, name)
		} else {
			controller.applied.ResetActive[name] = true
			delete(controller.resetFailed, name)
		}
	}
	if group == "all" || group == "overclock" {
		do("overclock", controller.applyUnlock(config.Overclock{Present: true, Factory: true}))
	}
	if group == "all" || group == "power" {
		do("power", controller.applyPower(config.Power{Present: true, Factory: true}))
	}
	if group == "all" || group == "temperature" {
		do("temperature", controller.applyTemperature(config.Temperature{Present: true, Factory: true}))
	}
	_ = controller.recordMarker() // retried via failed; the reset itself worked
	controller.summarize()
	return errors.Join(errs...)
}

// Status reads the card. The result shares no memory with the controller.
func (controller *Controller) Status() Status {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	device := controller.device
	applied := controller.applied
	applied.ResetActive = maps.Clone(controller.applied.ResetActive)
	applied.FanPoints = slices.Clone(controller.applied.FanPoints)
	applied.ApplyErrors = slices.Clone(controller.applied.ApplyErrors)
	if controller.applied.Unlocked != nil {
		unlocked := *controller.applied.Unlocked
		applied.Unlocked = &unlocked
	}
	status := Status{
		Address: device.PCIAddress(), Factory: gpu.Factory, Applied: applied,
		Reference: controller.reference(), Available: true,
	}
	if controller.closed {
		status.Available = false
		status.Errors = []string{ErrClosed.Error()}
		return status
	}
	status.Driver, status.Subsystem = device.Driver(), device.Subsystem()
	status.Pcie = gpu.ReadPcie(device.Sysfs())
	if device.Read32(gpu.RegisterPowerUnits) == 0xFFFFFFFF {
		status.Available = false
		status.Errors = []string{hardware.ErrUnavailable.Error()}
		return status
	}
	status.Power = gpu.ReadPower(device)
	status.Clock = gpu.ReadClock(device, device.Sysfs())
	note := func(err error) {
		if err != nil {
			status.Errors = append(status.Errors, err.Error())
		}
	}
	var err error
	status.TemperatureLimit, err = gpu.ReadTemperatureLimit(device)
	note(err)
	status.Overclock, err = gpu.ReadOverclock(device)
	note(err)
	if controller.iccMax == nil {
		if value, err := gpu.ReadIccMax(device); err == nil {
			controller.iccMax = &value
		} else {
			note(err)
		}
	}
	if controller.capabilities == nil {
		if value, err := gpu.ReadCapabilities(device); err == nil {
			controller.capabilities = &value
		} else {
			note(err)
		}
	}
	if controller.iccMax != nil {
		value := *controller.iccMax
		status.IccMax = &value
	}
	if controller.capabilities != nil {
		value := *controller.capabilities
		status.Capabilities = &value
	}
	return status
}

// Snapshot reads the counters.
func (controller *Controller) Snapshot() (gpu.Snapshot, error) {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	if controller.closed {
		return gpu.Snapshot{}, ErrClosed
	}
	return gpu.TakeSnapshot(controller.device), nil
}

// Compute derives sensors from two snapshots; the /proc scan runs outside the lock.
func (controller *Controller) Compute(before, after gpu.Snapshot) (gpu.Sensors, error) {
	controller.mutex.Lock()
	if controller.closed {
		controller.mutex.Unlock()
		return gpu.Sensors{}, ErrClosed
	}
	sensors := gpu.Compute(controller.device, before, after)
	controller.mutex.Unlock()
	sensors.VramUsedBytes = gpu.VramUsedBytes(controller.device.PCIAddress())
	return sensors, nil
}

// ClampSensorsWindow bounds a sensors sample window to 0.5..10 s.
func ClampSensorsWindow(window time.Duration) time.Duration {
	return min(max(window, SensorsWindowMin), SensorsWindowMax)
}

// Sensors measures over a window without holding the lock while waiting.
func (controller *Controller) Sensors(window time.Duration) (gpu.Sensors, error) {
	window = ClampSensorsWindow(window)
	before, err := controller.Snapshot()
	if err != nil {
		return gpu.Sensors{}, err
	}
	time.Sleep(window)
	after, err := controller.Snapshot()
	if err != nil {
		return gpu.Sensors{}, err
	}
	return controller.Compute(before, after)
}

// Drift reports settings whose read-back differs from the configuration (reset groups
// skipped).
func Drift(current *config.Config, status Status) map[string]bool {
	drift := map[string]bool{}
	if current == nil {
		return drift
	}
	near := func(a, b, tolerance float64) bool { return math.Abs(a-b) <= tolerance }
	if p := current.Power; !status.Applied.ResetActive["power"] {
		if hasValue(p.Sustained) {
			drift["power.sustained"] = !near(status.Power.Sustained.Watts, p.Sustained.Value, 0.25)
		}
		if hasValue(p.Burst) {
			drift["power.burst"] = !near(status.Power.Burst.Watts, p.Burst.Value, 0.25)
		}
		if hasValue(p.Window) {
			drift["power.window"] = !near(status.Power.Sustained.WindowSeconds, p.Window.Value, p.Window.Value*0.15)
		}
	}
	if t := current.Temperature.Limit; hasValue(t) && !status.Applied.ResetActive["temperature"] {
		drift["temperature.limit"] = status.TemperatureLimit != t.Value
	}
	return drift
}

// Current returns the last applied configuration.
func (controller *Controller) Current() *config.Config {
	controller.mutex.Lock()
	defer controller.mutex.Unlock()
	return controller.current
}

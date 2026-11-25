package controller

import (
	"errors"
	"testing"
	"time"

	"a310ctl/internal/config"
	"a310ctl/internal/gpu"
	"a310ctl/internal/hardware"
)

// pcode command IDs as in INTERNALS.md (unexported in package gpu).
const (
	pcodeTempWrite = 0x72
	pcodeTempRead  = 0x73
	pcodeOC        = 0x6C
	pcodeCap       = 0x7A
	pcodeIcc       = 0x75
	pcodeFan       = 0x7D
	pcodeVramRate  = 0x6F
	domainGT       = 1
	paramVFRead    = 0
	paramVFWrite   = 1
	paramFreqRead  = 2
	paramFreqWrite = 3
	paramLock      = 4
	raplLockBit    = 1 << 31
)

type fakeGPU struct {
	mem      map[uint32]uint32
	temp     int
	vf       uint32
	freq     uint32
	fanCount uint32
	writeErr error
	ignore   map[uint32]uint32 // offset -> value the "firmware" drops
}

func newFake() *fakeGPU {
	f := &fakeGPU{mem: map[uint32]uint32{}}
	f.mem[gpu.RegisterPowerUnits] = 3 | 14<<8 | 10<<16 // 1/8 W, 1/16384 J, 1/1024 s
	f.mem[gpu.RegisterRaplLimit] = gpu.Factory.RaplLow
	f.mem[gpu.RegisterRaplLimit+4] = gpu.Factory.RaplHigh
	f.temp = gpu.Factory.Temperature
	return f
}

func (f *fakeGPU) Read32(offset uint32) uint32 { return f.mem[offset] }
func (f *fakeGPU) Write32(offset, value uint32) {
	if v, ok := f.ignore[offset]; ok && v == value {
		return
	}
	f.mem[offset] = value
}
func (f *fakeGPU) Close()             {}
func (f *fakeGPU) PCIAddress() string { return "0000:03:00.0" }
func (f *fakeGPU) Sysfs() string      { return "/dev/null" }
func (f *fakeGPU) Driver() string     { return "i915" }
func (f *fakeGPU) Subsystem() string  { return "172f:4019" }

func (f *fakeGPU) PcodeRead(command, subcommand, parameter uint32) (uint32, uint32, error) {
	if f.writeErr != nil {
		return 0, 0, f.writeErr
	}
	switch command {
	case pcodeTempRead:
		return uint32(f.temp), 0, nil
	case pcodeCap:
		return 0x3f55, 0, nil
	case pcodeIcc:
		return 400, 0, nil // 100 A
	case pcodeOC:
		switch parameter {
		case paramVFRead:
			return f.vf, 0, nil
		case paramFreqRead:
			return f.freq, 0, nil
		}
	case pcodeVramRate:
		return 15494, 0, nil
	}
	return 0, 0, nil
}

func (f *fakeGPU) PcodeWrite(command, subcommand, parameter, data0, data1 uint32) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	switch command {
	case pcodeTempWrite:
		f.temp = int(data0)
	case pcodeFan:
		if subcommand == 1 {
			f.fanCount = data0
		}
	case pcodeOC:
		switch parameter {
		case paramVFWrite:
			f.vf = data0
		case paramFreqWrite:
			f.freq = data0
		}
	}
	return nil
}

func parseConfig(t *testing.T, yaml string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml), "test")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestApplyPowerAndPowerLost(t *testing.T) {
	fake := newFake()
	control := New(fake)
	cfg := parseConfig(t, "power:\n  sustained: 50\n  burst: 50\n")
	if errs := control.Apply(cfg, true); len(errs) > 0 {
		t.Fatalf("apply: %v", errs)
	}
	if fake.mem[gpu.RegisterRaplLimit]&0x7FFF != 400 { // 50 W * 8
		t.Fatalf("sustained raw = 0x%x", fake.mem[gpu.RegisterRaplLimit])
	}
	if control.PowerLost() {
		t.Fatal("PowerLost after a successful apply")
	}
	fake.mem[gpu.RegisterRaplLimit] = gpu.Factory.RaplLow
	fake.mem[gpu.RegisterRaplLimit+4] = gpu.Factory.RaplHigh
	if !control.PowerLost() {
		t.Fatal("PowerLost after RAPL fell back to factory")
	}
}

func TestPowerLostAllOnes(t *testing.T) {
	fake := newFake()
	control := New(fake)
	fake.mem[gpu.RegisterPowerUnits] = 0xFFFFFFFF
	if !control.PowerLost() {
		t.Fatal("PowerLost when units read all ones")
	}
	status := control.Status()
	if status.Available || len(status.Errors) == 0 {
		t.Fatalf("status unavailable = %+v", status)
	}
	if status.Errors[0] != hardware.ErrUnavailable.Error() {
		t.Fatalf("errors = %v", status.Errors)
	}
	if status.Power.RawLow != 0 {
		t.Fatalf("all-ones status must skip RAPL decode, got %+v", status.Power)
	}
}

func TestFailedWriteExpectsReadBack(t *testing.T) {
	fake := newFake()
	control := New(fake)
	cfg50 := parseConfig(t, "power:\n  sustained: 50\n  burst: 50\n")
	if errs := control.Apply(cfg50, true); len(errs) > 0 {
		t.Fatal(errs)
	}
	fake.mem[gpu.RegisterRaplLimit+4] |= raplLockBit
	cfg40 := parseConfig(t, "power:\n  sustained: 40\n  burst: 40\n")
	if errs := control.Apply(cfg40, true); len(errs) == 0 {
		t.Fatal("locked RAPL must fail")
	}
	fake.mem[gpu.RegisterRaplLimit+4] &^= raplLockBit
	fake.mem[gpu.RegisterRaplLimit] = gpu.Factory.RaplLow
	fake.mem[gpu.RegisterRaplLimit+4] = gpu.Factory.RaplHigh
	if !control.PowerLost() {
		t.Fatal("failed 40 W write must expect the 50 W read back")
	}
}

func TestResetPower(t *testing.T) {
	fake := newFake()
	control := New(fake)
	cfg := parseConfig(t, "power:\n  sustained: 50\n  burst: 50\n")
	if errs := control.Apply(cfg, true); len(errs) > 0 {
		t.Fatal(errs)
	}
	if err := control.Reset("power"); err != nil {
		t.Fatal(err)
	}
	if fake.mem[gpu.RegisterRaplLimit] != gpu.Factory.RaplLow {
		t.Fatalf("reset RAPL = 0x%x", fake.mem[gpu.RegisterRaplLimit])
	}
	status := control.Status()
	if !status.Applied.ResetActive["power"] {
		t.Fatal("ResetActive[power] after reset")
	}
}

func TestApplyTemperatureAndFan(t *testing.T) {
	fake := newFake()
	control := New(fake)
	cfg := parseConfig(t, "temperature:\n  limit: 85\n  fan_curve: factory\n")
	if errs := control.Apply(cfg, true); len(errs) > 0 {
		t.Fatal(errs)
	}
	if fake.temp != 85 {
		t.Fatalf("temp = %d", fake.temp)
	}
	if fake.fanCount != 0 {
		t.Fatalf("fan count = %d", fake.fanCount)
	}
}

func TestApplyOverclockUnlock(t *testing.T) {
	fake := newFake()
	control := New(fake)
	cfg := parseConfig(t, "overclock:\n  unlock: true\n")
	if errs := control.Apply(cfg, true); len(errs) > 0 {
		t.Fatal(errs)
	}
	status := control.Status()
	if status.Applied.Unlocked == nil || !*status.Applied.Unlocked {
		t.Fatalf("unlocked = %v", status.Applied.Unlocked)
	}
}

func TestStatusOmitsUnreadIccMax(t *testing.T) {
	fake := newFake()
	fake.writeErr = errors.New("mailbox down")
	control := New(fake)
	status := control.Status()
	if status.IccMax != nil || status.Capabilities != nil {
		t.Fatalf("unread gauges present: icc=%v cap=%v", status.IccMax, status.Capabilities)
	}
	if len(status.Errors) == 0 {
		t.Fatal("expected mailbox errors")
	}
}

func TestClampSensorsWindow(t *testing.T) {
	if got := ClampSensorsWindow(time.Millisecond); got != SensorsWindowMin {
		t.Fatalf("min: %s", got)
	}
	if got := ClampSensorsWindow(time.Minute); got != SensorsWindowMax {
		t.Fatalf("max: %s", got)
	}
	if got := ClampSensorsWindow(2 * time.Second); got != 2*time.Second {
		t.Fatalf("passthrough: %s", got)
	}
}

func burst(fake *fakeGPU) uint32 { return fake.mem[gpu.RegisterRaplLimit+4] }

func TestMarkerWithFanCurve(t *testing.T) {
	fake := newFake()
	control := New(fake)
	if errs := control.Apply(parseConfig(t, "power: factory\ntemperature:\n  fan_curve: 60\n"), true); len(errs) > 0 {
		t.Fatal(errs)
	}
	if burst(fake) != markerHigh || fake.mem[gpu.RegisterRaplLimit] != gpu.Factory.RaplLow {
		t.Fatalf("burst = 0x%x, want marker 0x%x", burst(fake), markerHigh)
	}
	if !control.Status().Applied.PowerMarker {
		t.Fatal("PowerMarker not reported")
	}
	if control.PowerLost() {
		t.Fatal("PowerLost with the marker in place")
	}
	fake.mem[gpu.RegisterRaplLimit+4] = gpu.Factory.RaplHigh
	if !control.PowerLost() {
		t.Fatal("reset not detected through the marker")
	}
	control.ApplyAfterPowerLoss(control.Current())
	if burst(fake) != markerHigh {
		t.Fatal("marker not restored after power loss")
	}
}

func TestNoMarkerAllFactory(t *testing.T) {
	fake := newFake()
	control := New(fake)
	if errs := control.Apply(parseConfig(t, "power: factory\ntemperature: factory\noverclock: factory\n"), true); len(errs) > 0 {
		t.Fatal(errs)
	}
	if burst(fake) != gpu.Factory.RaplHigh || control.Status().Applied.PowerMarker {
		t.Fatalf("all-factory config must stay exact factory, burst = 0x%x", burst(fake))
	}
}

func TestNoMarkerWhenPowerOmitted(t *testing.T) {
	fake := newFake()
	control := New(fake)
	if errs := control.Apply(parseConfig(t, "temperature:\n  fan_curve: 60\n"), true); len(errs) > 0 {
		t.Fatal(errs)
	}
	if burst(fake) != gpu.Factory.RaplHigh {
		t.Fatal("omitted power group must not be touched")
	}
}

func TestMarkerRemovedWhenNotNeeded(t *testing.T) {
	fake := newFake()
	control := New(fake)
	control.Apply(parseConfig(t, "power: factory\ntemperature:\n  limit: 85\n"), true)
	if burst(fake) != markerHigh {
		t.Fatal("marker expected for a non-factory temperature limit")
	}
	control.Apply(parseConfig(t, "power: factory\ntemperature:\n  limit: factory\n"), false)
	if burst(fake) != gpu.Factory.RaplHigh || control.Status().Applied.PowerMarker {
		t.Fatalf("marker not removed, burst = 0x%x", burst(fake))
	}
}

func TestPartialPowerDropsMarker(t *testing.T) {
	fake := newFake()
	control := New(fake)
	control.Apply(parseConfig(t, "power: factory\noverclock:\n  unlock: true\n"), true)
	if burst(fake) != markerHigh {
		t.Fatal("marker expected while unlocked")
	}
	control.Apply(parseConfig(t, "power:\n  sustained: 40\noverclock:\n  unlock: true\n"), false)
	if burst(fake) != gpu.Factory.RaplHigh {
		t.Fatalf("burst kept the marker step: 0x%x", burst(fake))
	}
}

func TestResetAllClearsMarker(t *testing.T) {
	fake := newFake()
	control := New(fake)
	control.Apply(parseConfig(t, "power: factory\ntemperature:\n  fan_curve: 60\n"), true)
	if err := control.Reset("power"); err != nil {
		t.Fatal(err)
	}
	if burst(fake) != markerHigh {
		t.Fatal("reset power with a custom fan curve keeps the marker")
	}
	if err := control.Reset("all"); err != nil {
		t.Fatal(err)
	}
	if burst(fake) != gpu.Factory.RaplHigh || fake.mem[gpu.RegisterRaplLimit] != gpu.Factory.RaplLow {
		t.Fatalf("reset all must be exact factory, burst = 0x%x", burst(fake))
	}
}

func TestTemperatureFallbackWithoutPowerGroup(t *testing.T) {
	fake := newFake()
	control := New(fake)
	control.Apply(parseConfig(t, "temperature:\n  limit: 85\n"), true)
	fake.temp = gpu.Factory.Temperature
	for i := 0; i < temperatureCheckEvery; i++ {
		if control.PowerLost() {
			return
		}
	}
	t.Fatal("temperature reset not detected without a power group")
}

func TestMarkerSkipsTemperatureRead(t *testing.T) {
	fake := newFake()
	control := New(fake)
	control.Apply(parseConfig(t, "power: factory\ntemperature:\n  limit: 85\n"), true)
	fake.writeErr = errors.New("mailbox must not be used")
	for i := 0; i < 2*temperatureCheckEvery; i++ {
		if control.PowerLost() {
			t.Fatal("false power loss")
		}
	}
	if control.checks != 0 {
		t.Fatal("temperature checked although the power marker is active")
	}
}

func TestFailedResetReportedNotRetried(t *testing.T) {
	fake := newFake()
	control := New(fake)
	control.Apply(parseConfig(t, "overclock:\n  unlock: true\n  boost: 150\n"), true)
	fake.writeErr = errors.New("mailbox timeout")
	if err := control.Reset("overclock"); err == nil {
		t.Fatal("reset must fail")
	}
	a := control.Status().Applied
	if a.ApplySuccess || len(a.ApplyErrors) == 0 || a.ResetActive["overclock"] {
		t.Fatalf("failed reset not reported: %+v", a)
	}
	if a.Boost != 150 {
		t.Fatalf("boost cleared by a failed reset: %d", a.Boost)
	}
	if control.HasFailures() {
		t.Fatal("a failed reset must not be retried (it would re-apply the configuration)")
	}
	fake.writeErr = nil
	if err := control.Reset("overclock"); err != nil {
		t.Fatal(err)
	}
	if a := control.Status().Applied; !a.ApplySuccess || !a.ResetActive["overclock"] || a.Boost != 0 {
		t.Fatalf("successful reset not recorded: %+v", a)
	}
}

func TestMarkerRejectedIsRetried(t *testing.T) {
	fake := newFake()
	fake.ignore = map[uint32]uint32{gpu.RegisterRaplLimit + 4: markerHigh}
	control := New(fake)
	cfg := parseConfig(t, "power: factory\ntemperature:\n  fan_curve: 60\n")
	if errs := control.Apply(cfg, true); len(errs) == 0 {
		t.Fatal("rejected marker must be reported")
	}
	if !control.HasFailures() || control.Status().Applied.ApplySuccess {
		t.Fatal("rejected marker must count as a failure")
	}
	fake.ignore = nil
	if errs := control.RetryFailed(cfg); len(errs) > 0 {
		t.Fatal(errs)
	}
	if burst(fake) != markerHigh || control.HasFailures() {
		t.Fatalf("marker not retried, burst = 0x%x", burst(fake))
	}
}

func TestPowerLossKeepsResetPowerGroup(t *testing.T) {
	fake := newFake()
	control := New(fake)
	cfg := parseConfig(t, "power:\n  sustained: 50\n  burst: 50\ntemperature:\n  fan_curve: 60\n")
	control.Apply(cfg, true)
	if err := control.Reset("power"); err != nil {
		t.Fatal(err)
	}
	if burst(fake) != markerHigh {
		t.Fatal("marker expected after reset power with a fan curve")
	}
	fake.mem[gpu.RegisterRaplLimit+4] = gpu.Factory.RaplHigh // card reset
	fake.fanCount = 0
	if !control.PowerLost() {
		t.Fatal("reset not detected")
	}
	if errs := control.ApplyAfterPowerLoss(cfg); len(errs) > 0 {
		t.Fatal(errs)
	}
	if fake.mem[gpu.RegisterRaplLimit] != gpu.Factory.RaplLow || burst(fake) != markerHigh {
		t.Fatalf("reset power group must stay factory (+marker): 0x%x_%x", burst(fake), fake.mem[gpu.RegisterRaplLimit])
	}
	if fake.fanCount == 0 {
		t.Fatal("fan curve not restored")
	}
}

func TestMarkerActiveFollowsRegister(t *testing.T) {
	fake := newFake()
	control := New(fake)
	control.Apply(parseConfig(t, "power: factory\ntemperature:\n  fan_curve: 60\n"), true)
	if !control.Status().MarkerActive() {
		t.Fatal("marker not active")
	}
	fake.mem[gpu.RegisterRaplLimit+4] = gpu.Factory.RaplHigh // card reset, not yet re-applied
	if control.Status().MarkerActive() {
		t.Fatal("marker reported although the register lost it")
	}
}

func TestMarkerFlagClearedWithoutWrite(t *testing.T) {
	fake := newFake()
	control := New(fake)
	control.Apply(parseConfig(t, "power: factory\ntemperature:\n  fan_curve: 60\n"), true)
	fake.mem[gpu.RegisterRaplLimit+4] = gpu.Factory.RaplHigh // reset, not recovered
	control.Apply(parseConfig(t, "power: factory\ntemperature: factory\n"), false)
	if control.Status().Applied.PowerMarker {
		t.Fatal("stale PowerMarker after an apply that found factory already in place")
	}
}

package gpu

import "testing"

// Values below were verified on the A310 during the reverse-engineering session.
var units = Units{Power: 8, Energy: 16384, Time: 1024}

func TestWindowEncode(t *testing.T) {
	if got := WindowEncode(28, units.Time); got != 0x6e {
		t.Fatalf("window 28 s = 0x%x, want 0x6e", got)
	}
	if got := WindowDecode(0x6e, units.Time); got != 28 {
		t.Fatalf("decode 0x6e = %g, want 28", got)
	}
}

func TestEncodeWatts(t *testing.T) {
	got, err := EncodeWatts(0x00dc80fa, 50, units)
	if err != nil || got != 0x00dc8190 {
		t.Fatalf("50 W = 0x%08x %v, want 0x00dc8190", got, err)
	}
	if limit := DecodeLimit(0x00dc80fa, units); limit.Watts != 31.25 || !limit.Enabled || limit.WindowSeconds != 28 {
		t.Fatalf("decode factory limit = %+v", limit)
	}
}

type vfFake struct {
	word, freq uint32
	stripFixed bool
}

func (f *vfFake) Read32(uint32) uint32   { return 0 }
func (f *vfFake) Write32(uint32, uint32) {}
func (f *vfFake) PcodeRead(command, _, parameter uint32) (uint32, uint32, error) {
	switch command {
	case pcodeOverclockCapabilities:
		return 0x3f55, 0, nil
	case pcodeOverclock:
		if parameter == parameterVoltageFrequencyRead {
			return f.word, 0, nil
		}
		if parameter == parameterFrequencyRead {
			return f.freq, 0, nil
		}
	}
	return 0, 0, nil
}
func (f *vfFake) PcodeWrite(command, _, parameter, data0, _ uint32) error {
	if command != pcodeOverclock {
		return nil
	}
	switch parameter {
	case parameterVoltageFrequencyWrite:
		if f.stripFixed {
			data0 &^= 1 << 20
		}
		f.word = data0
	case parameterFrequencyWrite:
		f.freq = data0
	}
	return nil
}

func TestWriteVoltageFrequencyMode(t *testing.T) {
	point := Point{Megahertz: 2450, Millivolts: 910, Fixed: true}
	ok := &vfFake{}
	if err := WriteVoltageFrequency(ok, point); err != nil {
		t.Fatal(err)
	}
	clamped := &vfFake{stripFixed: true}
	if err := WriteVoltageFrequency(clamped, point); err == nil {
		t.Fatal("fixed mode stripped on read-back must fail")
	}
}

func TestVoltageFrequencyWord(t *testing.T) {
	if got := EncodeVoltageFrequency(954, -44, false); got != 0xfa83d100 {
		t.Fatalf("954 mV / -44 = 0x%08x, want 0xfa83d100", got)
	}
	if got := EncodeVoltageFrequency(0, -20, false); got != 0xfd800000 {
		t.Fatalf("-20 offset = 0x%08x, want 0xfd800000", got)
	}
	vf := DecodeVoltageFrequency(0xfa83d100)
	if vf.Millivolts != 954 || vf.Offset != -44 || vf.Mode != "adaptive" {
		t.Fatalf("decode = %+v", vf)
	}
	if got := EncodeVoltageFrequency(880, 0, true); got != 0x00138500 {
		t.Fatalf("fixed 880 mV = 0x%08x, want 0x00138500", got)
	}
}

func TestBoostPlan(t *testing.T) {
	reference := Reference{Megahertz: 2450, Millivolts: 910}
	point := BoostPlan(reference, 100, 0)
	if point.Megahertz != 2550 || point.Millivolts != 954 || point.Offset != -44 {
		t.Fatalf("boost +100 = %+v, want 2550/954/-44", point)
	}
	point = BoostPlan(reference, 150, 20)
	if point.Offset != -88 || point.Millivolts != 978 {
		t.Fatalf("boost +150 undervolt 20 = %+v, want 978/-88", point)
	}
	point = BoostPlan(reference, 0, 20)
	if point.Megahertz != 2450 || point.Millivolts != 0 || point.Offset != -20 {
		t.Fatalf("undervolt only = %+v", point)
	}
	if err := CheckPoint(Point{Megahertz: 2450, Millivolts: 600, Fixed: true}, reference, 2450); err == nil {
		t.Fatal("fixed 600 mV must be rejected (< reference - 250)")
	}
}

func TestFanCurve(t *testing.T) {
	points, err := ParseFanCurve("40:20 55:30, 70:60 85:100")
	if err != nil {
		t.Fatal(err)
	}
	if got := EncodeFanPair(points[0], points[1]); got != 0x1e371428 {
		t.Fatalf("pair = 0x%08x, want 0x1e371428", got)
	}
	for _, bad := range []string{"40:20", "40:20 55:30 70:60", "40:50 55:30", "40:20 99:100", "40:20 85:90"} {
		if _, err := ParseFanCurve(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
	flat, err := FlatFanCurve(60)
	if err != nil || len(flat) != 2 || flat[0] != (FanPoint{20, 60}) || flat[1] != (FanPoint{105, 60}) {
		t.Fatalf("flat 60 = %v %v", flat, err)
	}
	if _, err := FlatFanCurve(10); err == nil {
		t.Fatal("flat 10 must be rejected")
	}
}

func TestThrottleReasons(t *testing.T) {
	reasons := ThrottleReasons(0x0d020100)
	if !reasons["current_voltage"] || reasons["power"] || reasons["thermal"] {
		t.Fatalf("0x0d020100 = %v", reasons)
	}
}

func TestFactoryValues(t *testing.T) {
	sustained, burst := DecodeLimit(Factory.RaplLow, units), DecodeLimit(Factory.RaplHigh, units)
	if sustained.Watts != 31.25 || burst.Watts != 31.25 || !sustained.Enabled || sustained.WindowSeconds != 28 {
		t.Fatalf("factory power = %+v / %+v", sustained, burst)
	}
	if Factory.Temperature != 90 || Factory.Reference != (Reference{2450, 910}) || Factory.FrequencyTarget != 0 {
		t.Fatalf("factory = %+v", Factory)
	}
}

// fakeRegister simulates a 64-bit counter whose low half wraps between the two reads.
type fakeRegister struct {
	reads int
}

func (f *fakeRegister) Read32(offset uint32) uint32 {
	f.reads++
	if offset%8 == 4 { // high half: 1 before the wrap, 2 after
		if f.reads <= 2 {
			return 1
		}
		return 2
	}
	if f.reads <= 2 { // low half read just before the wrap
		return 0xFFFFFFF0
	}
	return 0x10
}
func (f *fakeRegister) Write32(uint32, uint32)                                   {}
func (f *fakeRegister) PcodeRead(uint32, uint32, uint32) (uint32, uint32, error) { return 0, 0, nil }
func (f *fakeRegister) PcodeWrite(uint32, uint32, uint32, uint32, uint32) error  { return nil }

func TestReadCounter64NoTear(t *testing.T) {
	got := ReadCounter64(&fakeRegister{}, 0x36C448)
	if got != 2<<32|0x10 {
		t.Fatalf("torn read: 0x%x, want 0x%x", got, uint64(2<<32|0x10))
	}
}

func TestFdinfoRegex(t *testing.T) {
	cases := map[string][]string{
		"drm-resident-local0:\t0\ndrm-total-stolen-local0:\t0 KiB\n": {"0", ""},
		"drm-resident-local0:\t530 MiB\n":                            {"530", "MiB"},
		"drm-resident-vram0:\t12 KiB\n":                              {"12", "KiB"},
	}
	for text, want := range cases {
		m := fdinfoResident.FindStringSubmatch(text)
		if m == nil || m[1] != want[0] || m[2] != want[1] {
			t.Errorf("%q: got %v, want %v", text, m, want)
		}
	}
}

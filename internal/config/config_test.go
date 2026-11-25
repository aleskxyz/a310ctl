package config

import (
	"os"
	"strings"
	"testing"
)

func TestExampleConfig(t *testing.T) {
	data, err := os.ReadFile("../../packaging/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	config, err := Parse(data, "example")
	if err != nil {
		t.Fatal(err)
	}
	// The installed default must keep the card at factory and the exporter off.
	if config.Exporter.Enabled || config.Exporter.Listen != "127.0.0.1:9310" {
		t.Fatalf("exporter = %+v", config.Exporter)
	}
	p, tm, o := config.Power, config.Temperature, config.Overclock
	for name, factory := range map[string]bool{
		"power.sustained": p.Sustained.Factory, "power.burst": p.Burst.Factory, "power.window": p.Window.Factory,
		"temperature.limit": tm.Limit.Factory, "temperature.fan_curve": tm.FanCurve.Factory,
		"overclock.unlock": o.Unlock.Factory,
	} {
		if !factory {
			t.Errorf("%s must be factory in the default configuration", name)
		}
	}
	if o.Boost.Set || o.Undervolt.Set || o.Frequency.Set || o.Voltage.Set || o.VoltageOffset.Set || o.VoltageMode.Set {
		t.Errorf("default configuration must not set an overclock: %+v", o)
	}
}

func TestFactoryAndFlat(t *testing.T) {
	config, err := Parse([]byte("power: factory\ntemperature:\n  limit: factory\n  fan_curve: 60\noverclock:\n  boost: factory\n"), "test")
	if err != nil {
		t.Fatal(err)
	}
	if !config.Power.Present || !config.Power.Factory {
		t.Fatalf("power group factory not parsed: %+v", config.Power)
	}
	if !config.Temperature.Limit.Factory {
		t.Fatal("temperature.limit factory not parsed")
	}
	if config.Temperature.FanCurve.Flat != 60 || len(config.Temperature.FanCurve.Points) != 2 {
		t.Fatalf("flat curve = %+v", config.Temperature.FanCurve)
	}
	if !config.Overclock.Boost.Factory {
		t.Fatal("overclock.boost factory not parsed")
	}
}

func TestDisabledExporterIntervalIgnored(t *testing.T) {
	if _, err := Parse([]byte("exporter:\n  enabled: false\n  interval: 300\n"), "test"); err != nil {
		t.Fatalf("a disabled exporter's interval must not be validated: %v", err)
	}
}

func TestEmptyAndOmitted(t *testing.T) {
	config, err := Parse([]byte(""), "empty")
	if err != nil {
		t.Fatal(err)
	}
	if config.Power.Present || config.Temperature.Present || config.Overclock.Present || config.Exporter.Enabled {
		t.Fatalf("empty config must touch nothing: %+v", config)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]string{
		"power:\n  sustained: 100\n":                        "10..75",
		"temperature:\n  limit: 98\n":                       "60..95",
		"power:\n  sustaind: 40\n":                          "unknown key",
		"fan_curve: 50\n":                                   "not found",
		"overclock:\n  unlock: false\n  boost: 100\n":       "need unlock",
		"overclock:\n  boost: 100\n  voltage_offset: -20\n": "not both",
		"overclock:\n  voltage_mode: turbo\n":               "adaptive or fixed",
		"temperature:\n  fan_curve: \"40:20 99:100\"\n":     "last point",
		"exporter:\n  enabled: true\n  interval: 0\n":       "interval",
		"exporter:\n  enabled: true\n  interval: 120\n":     "0.5..60",
		"overclock:\n  frequency: 9000\n":                   "frequency must be",
		"exporter:\n  enabled: true\n  interval: .nan\n":    "0.5..60",
		"power:\n  window: .nan\n":                          "power.window",
		"power:\n  sustained: .nan\n":                       "outside",
		"overclock:\n  voltage_mode: fixed\n":               "at least 660",
		"exporter:\n  enabled: true\n  listen: nonsense\n":  "host:port",
		"overclock:\n  boost: factory\n  frequency: 2500\n": "not both",
		"overclock:\n  unlock: factory\n  boost: 100\n":     "need unlock",
		"overclock:\n  boost: 300\n  undervolt: 10\n":       "beyond -150",
	}
	for input, want := range cases {
		_, err := Parse([]byte(input), "test")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %v, want containing %q", input, err, want)
		}
	}
}

// Package config loads and validates /etc/a310ctl/config.yaml.
// Omitted keys/groups leave the hardware untouched; any GPU key or group may be "factory".
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"a310ctl/internal/gpu"
)

const (
	DefaultPath   = "/etc/a310ctl/config.yaml"
	DefaultSocket = "/run/a310ctl.sock"
	DefaultListen = "127.0.0.1:9310"
	factoryWord   = "factory"
)

// Setting is a value that may be absent, "factory", or a concrete value.
type Setting[T any] struct {
	Set     bool
	Factory bool
	Value   T
}

// isFactory reports the scalar "factory" (any case).
func isFactory(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && strings.EqualFold(node.Value, factoryWord)
}

// UnmarshalYAML accepts "factory" or a T.
func (setting *Setting[T]) UnmarshalYAML(node *yaml.Node) error {
	setting.Set = true
	if isFactory(node) {
		setting.Factory = true
		return nil
	}
	return node.Decode(&setting.Value)
}

// FanCurve is "factory", a flat duty (integer) or a point list ("40:20 55:30 ...").
type FanCurve struct {
	Set     bool
	Factory bool
	Flat    int
	Text    string
	Points  []gpu.FanPoint
}

// UnmarshalYAML parses the three forms.
func (curve *FanCurve) UnmarshalYAML(node *yaml.Node) error {
	curve.Set = true
	if isFactory(node) {
		curve.Factory = true
		return nil
	}
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: fan_curve must be a number (flat duty), a \"temperature:percent ...\" string, or factory", node.Line)
	}
	var flat int
	if node.Tag == "!!int" && node.Decode(&flat) == nil {
		points, err := gpu.FlatFanCurve(flat)
		if err != nil {
			return fmt.Errorf("line %d: %w", node.Line, err)
		}
		curve.Flat, curve.Points = flat, points
		return nil
	}
	points, err := gpu.ParseFanCurve(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", node.Line, err)
	}
	curve.Text, curve.Points = node.Value, points
	return nil
}

// Exporter configures the optional Prometheus endpoint.
type Exporter struct {
	Enabled  bool    `yaml:"enabled"`
	Listen   string  `yaml:"listen"`
	Interval float64 `yaml:"interval"`
}

// Temperature group: throttle limit and fan curve.
type Temperature struct {
	Present  bool         `yaml:"-"`
	Factory  bool         `yaml:"-"`
	Limit    Setting[int] `yaml:"limit"`
	FanCurve FanCurve     `yaml:"fan_curve"`
}

// Power group: RAPL limits.
type Power struct {
	Present   bool             `yaml:"-"`
	Factory   bool             `yaml:"-"`
	Sustained Setting[float64] `yaml:"sustained"`
	Burst     Setting[float64] `yaml:"burst"`
	Window    Setting[float64] `yaml:"window"`
}

// Overclock group: unlock and voltage-frequency.
type Overclock struct {
	Present       bool            `yaml:"-"`
	Factory       bool            `yaml:"-"`
	Unlock        Setting[bool]   `yaml:"unlock"`
	Boost         Setting[int]    `yaml:"boost"`
	Undervolt     Setting[int]    `yaml:"undervolt"`
	Frequency     Setting[int]    `yaml:"frequency"`
	Voltage       Setting[int]    `yaml:"voltage"`
	VoltageOffset Setting[int]    `yaml:"voltage_offset"`
	VoltageMode   Setting[string] `yaml:"voltage_mode"`
}

// checkKeys rejects unknown keys in a group (typos would otherwise be ignored).
func checkKeys(node *yaml.Node, group string, allowed ...string) error {
	for index := 0; index+1 < len(node.Content); index += 2 {
		key := node.Content[index].Value
		known := false
		for _, name := range allowed {
			known = known || key == name
		}
		if !known {
			return fmt.Errorf("line %d: unknown key %q in %s (allowed: %s)", node.Content[index].Line, key, group, strings.Join(allowed, ", "))
		}
	}
	return nil
}

// decodeGroup decodes a group that is either "factory" or a mapping of allowed keys.
func decodeGroup(node *yaml.Node, group string, present, factory *bool, target any, allowed ...string) error {
	*present = true
	if isFactory(node) {
		*factory = true
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: %s must be a group of keys or factory", node.Line, group)
	}
	if err := checkKeys(node, group, allowed...); err != nil {
		return err
	}
	return node.Decode(target)
}

// UnmarshalYAML for the temperature group.
func (group *Temperature) UnmarshalYAML(node *yaml.Node) error {
	type plain Temperature
	var value plain
	err := decodeGroup(node, "temperature", &group.Present, &group.Factory, &value, "limit", "fan_curve")
	group.Limit, group.FanCurve = value.Limit, value.FanCurve
	return err
}

// UnmarshalYAML for the power group.
func (group *Power) UnmarshalYAML(node *yaml.Node) error {
	type plain Power
	var value plain
	err := decodeGroup(node, "power", &group.Present, &group.Factory, &value, "sustained", "burst", "window")
	group.Sustained, group.Burst, group.Window = value.Sustained, value.Burst, value.Window
	return err
}

// UnmarshalYAML for the overclock group.
func (group *Overclock) UnmarshalYAML(node *yaml.Node) error {
	type plain Overclock
	var value plain
	err := decodeGroup(node, "overclock", &group.Present, &group.Factory, &value,
		"unlock", "boost", "undervolt", "frequency", "voltage", "voltage_offset", "voltage_mode")
	present, factory := group.Present, group.Factory
	*group = Overclock(value)
	group.Present, group.Factory = present, factory
	return err
}

// Config is the whole file.
type Config struct {
	Device      string      `yaml:"device"`
	Socket      string      `yaml:"socket"`
	Exporter    Exporter    `yaml:"exporter"`
	Temperature Temperature `yaml:"temperature"`
	Power       Power       `yaml:"power"`
	Overclock   Overclock   `yaml:"overclock"`

	Path string `yaml:"-"`
	Hash string `yaml:"-"`
}

// Load reads, parses and validates a configuration file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, path)
}

// Parse parses and validates configuration bytes.
func Parse(data []byte, path string) (*Config, error) {
	sum := sha256.Sum256(data)
	config := &Config{Device: "automatic", Socket: DefaultSocket, Path: path, Hash: hex.EncodeToString(sum[:])}
	config.Exporter = Exporter{Listen: DefaultListen, Interval: 2}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(config); err != nil && !errors.Is(err, io.EOF) { // empty file = no-op
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if errs := config.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("%s: %w", path, errors.Join(errs...))
	}
	return config, nil
}

// within reports low <= value <= high; false for NaN.
func within(value, low, high float64) bool { return value >= low && value <= high }

// Validate checks ranges that do not need the hardware.
func (config *Config) Validate() []error {
	var errs []error
	add := func(format string, arguments ...any) { errs = append(errs, fmt.Errorf(format, arguments...)) }
	if config.Exporter.Enabled {
		// At most 60 s, so 32-bit counters wrap at most once between samples.
		if !within(config.Exporter.Interval, 0.5, 60) {
			add("exporter.interval must be 0.5..60 seconds")
		}
		if _, port, err := net.SplitHostPort(config.Exporter.Listen); err != nil || port == "" {
			add("exporter.listen must be host:port, e.g. 127.0.0.1:9310")
		}
	}
	if t := config.Temperature; t.Limit.Set && !t.Limit.Factory &&
		(t.Limit.Value < gpu.TemperatureMinimum || t.Limit.Value > gpu.TemperatureMaximum) {
		add("temperature.limit must be %d..%d", gpu.TemperatureMinimum, gpu.TemperatureMaximum)
	}
	p := config.Power
	for name, setting := range map[string]Setting[float64]{"sustained": p.Sustained, "burst": p.Burst} {
		if setting.Set && !setting.Factory {
			if err := gpu.CheckWatts("power."+name, setting.Value); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if p.Window.Set && !p.Window.Factory && !within(p.Window.Value, 0.001, 300) {
		add("power.window must be 0.001..300 seconds")
	}
	o := config.Overclock
	value := func(s Setting[int]) bool { return s.Set && !s.Factory }
	boost := value(o.Boost) || value(o.Undervolt)
	raw := value(o.Frequency) || value(o.Voltage) || value(o.VoltageOffset) || (o.VoltageMode.Set && !o.VoltageMode.Factory)
	// boost/undervolt (even as factory) and the raw keys select different modes.
	if (o.Boost.Set || o.Undervolt.Set) && raw {
		add("overclock: use boost/undervolt or frequency/voltage/voltage_offset/voltage_mode, not both")
	}
	if (boost || raw) && o.Unlock.Set && (o.Unlock.Factory || !o.Unlock.Value) {
		add("overclock: voltage-frequency settings need unlock (remove unlock: false / unlock: factory)")
	}
	if value(o.Boost) && (o.Boost.Value < 0 || o.Boost.Value > gpu.OverclockBoostMaximum) {
		add("overclock.boost must be 0..%d megahertz", gpu.OverclockBoostMaximum)
	}
	if value(o.Undervolt) && (o.Undervolt.Value < 0 || o.Undervolt.Value > gpu.OverclockOffsetMaximum) {
		add("overclock.undervolt must be 0..%d millivolts", gpu.OverclockOffsetMaximum)
	}
	if boost {
		// boost converts to a negative offset: check what will actually be written.
		var b, u int
		if value(o.Boost) {
			b = o.Boost.Value
		}
		if value(o.Undervolt) {
			u = o.Undervolt.Value
		}
		if offset := gpu.BoostPlan(gpu.Factory.Reference, b, u).Offset; offset < -gpu.OverclockOffsetMaximum {
			add("overclock: boost %d with undervolt %d gives a %d millivolt offset, beyond -%d", b, u, offset, gpu.OverclockOffsetMaximum)
		}
	}
	if maximum := gpu.Factory.Reference.Megahertz + gpu.OverclockBoostMaximum; value(o.Frequency) &&
		(o.Frequency.Value < 0 || o.Frequency.Value > maximum) {
		add("overclock.frequency must be 0..%d megahertz", maximum)
	}
	if value(o.Voltage) && (o.Voltage.Value < 0 || o.Voltage.Value > gpu.OverclockVoltageMaximum) {
		add("overclock.voltage must be 0..%d millivolts", gpu.OverclockVoltageMaximum)
	}
	if value(o.VoltageOffset) && (o.VoltageOffset.Value < -gpu.OverclockOffsetMaximum || o.VoltageOffset.Value > gpu.OverclockOffsetMaximum) {
		add("overclock.voltage_offset must be -%d..%d millivolts", gpu.OverclockOffsetMaximum, gpu.OverclockOffsetMaximum)
	}
	if m := o.VoltageMode; m.Set && !m.Factory && m.Value != "adaptive" && m.Value != "fixed" {
		add("overclock.voltage_mode must be adaptive or fixed")
	}
	if m := o.VoltageMode; m.Set && !m.Factory && m.Value == "fixed" {
		if minimum := gpu.Factory.Reference.Millivolts - gpu.FixedVoltageBelow; !value(o.Voltage) || o.Voltage.Value < minimum {
			add("overclock: voltage_mode fixed needs a voltage of at least %d millivolts", minimum)
		}
	}
	return errs
}

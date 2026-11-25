// Package client implements status, sensors and reset: through the daemon socket, or
// directly on the hardware when no daemon runs.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"a310ctl/internal/config"
	"a310ctl/internal/controller"
	"a310ctl/internal/gpu"
	"a310ctl/internal/hardware"
)

// Backend is either the daemon or the hardware.
type Backend interface {
	Status() (controller.Status, error)
	Sensors(window time.Duration) (gpu.Sensors, error)
	Reset(group string) error
	Close()
}

type socketBackend struct{ http *http.Client }

// call performs a request and decodes a JSON response; non-200 replies become errors.
func (backend socketBackend) call(method, path string, target any) error {
	request, err := http.NewRequest(method, "http://a310ctl"+path, nil)
	if err != nil {
		return err
	}
	response, err := backend.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		var reply map[string]string
		if json.Unmarshal(body, &reply) == nil && reply["error"] != "" {
			return errors.New(reply["error"])
		}
		return fmt.Errorf("daemon replied %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	if target == nil {
		return nil
	}
	return json.Unmarshal(body, target)
}

func (backend socketBackend) Status() (controller.Status, error) {
	var status controller.Status
	return status, backend.call(http.MethodGet, "/v1/status", &status)
}

func (backend socketBackend) Sensors(window time.Duration) (gpu.Sensors, error) {
	var sensors gpu.Sensors
	return sensors, backend.call(http.MethodGet, fmt.Sprintf("/v1/sensors?window=%g", window.Seconds()), &sensors)
}

func (backend socketBackend) Reset(group string) error {
	return backend.call(http.MethodPost, "/v1/reset?group="+url.QueryEscape(group), nil)
}

func (socketBackend) Close() {}

type directBackend struct{ control *controller.Controller }

func (backend directBackend) Status() (controller.Status, error) {
	return backend.control.Status(), nil
}
func (backend directBackend) Sensors(window time.Duration) (gpu.Sensors, error) {
	return backend.control.Sensors(window)
}
func (backend directBackend) Reset(group string) error { return backend.control.Reset(group) }
func (backend directBackend) Close()                   { backend.control.Close() }

// Connect returns the daemon if its socket answers, else the hardware (device from the
// default configuration file). Close the backend when done.
func Connect(socket string) (Backend, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", socket)
	}}
	if connection, err := net.DialTimeout("unix", socket, time.Second); err == nil {
		connection.Close()
		return socketBackend{http: &http.Client{Transport: transport, Timeout: 30 * time.Second}}, nil
	}
	address := "automatic"
	if cfg, err := config.Load(config.DefaultPath); err == nil {
		address = cfg.Device
	} else if !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "a310ctl: ignoring invalid %s, using the automatic device: %v\n", config.DefaultPath, err)
	}
	device, err := hardware.Open(address)
	if errors.Is(err, hardware.ErrBusy) {
		return nil, fmt.Errorf("another a310ctl process (the daemon or a CLI command) is using the GPU; "+
			"if it is the daemon, its socket %s does not answer (wrong --socket?)", socket)
	}
	if err != nil {
		return nil, err
	}
	return directBackend{control: controller.New(device)}, nil
}

func onOff(value bool) string {
	if value {
		return "enabled"
	}
	return "disabled"
}

// PrintStatus renders the status report.
func PrintStatus(out io.Writer, status controller.Status) {
	line := func(name, format string, arguments ...any) {
		fmt.Fprintf(out, "%-22s %s\n", name, fmt.Sprintf(format, arguments...))
	}
	source := "direct (no daemon)"
	if status.Daemon {
		source = "daemon"
	}
	line("device", "%s (driver %s, board %s, via %s)", status.Address, status.Driver, status.Subsystem, source)
	if !status.Available {
		line("card", "not responding (reads all ones: resetting, resuming or off the bus)")
		for _, err := range status.Errors {
			line("error", "%s", err)
		}
		return
	}
	p := status.Pcie
	line("pcie", "%g GT/s x%d (maximum %g GT/s x%d), BAR sizes %v MB", p.SpeedGT, p.Width, p.MaxSpeedGT, p.MaxWidth, p.BarMegabytes)
	w := status.Power
	line("power sustained", "%.2f W %s, window %.2f s", w.Sustained.Watts, onOff(w.Sustained.Enabled), w.Sustained.WindowSeconds)
	marker := ""
	if status.MarkerActive() {
		marker = " (factory + one step: reset marker)"
	}
	line("power burst", "%.2f W %s%s", w.Burst.Watts, onOff(w.Burst.Enabled), marker)
	line("power register", "0x%08x_%08x %s", w.RawHigh, w.RawLow, map[bool]string{true: "LOCKED", false: "unlocked"}[w.Locked])
	line("temperature limit", "%d C", status.TemperatureLimit)
	a := status.Applied
	switch a.FanMode {
	case "points", "flat":
		var points []string
		for _, point := range a.FanPoints {
			points = append(points, fmt.Sprintf("%d:%d", point.Celsius, point.Percent))
		}
		line("fan curve", "%s (%s, as applied)", strings.Join(points, " "), a.FanMode)
	case "factory":
		line("fan curve", "factory (as applied)")
	default:
		line("fan curve", "unknown (the firmware cannot report it)")
	}
	if status.IccMax != nil {
		line("iccmax", "%.2f A", *status.IccMax)
	}
	unlocked := "unknown (not readable)"
	if a.Unlocked != nil {
		unlocked = map[bool]string{true: "unlocked", false: "locked"}[*a.Unlocked] + " (as applied)"
	}
	line("overclock", "%s", unlocked)
	vf := status.Overclock.VoltageFrequency
	line("overclock point", "voltage %d mV, offset %d mV, %s, frequency target %d MHz (word 0x%08x)",
		vf.Millivolts, vf.Offset, vf.Mode, status.Overclock.Megahertz, vf.Raw)
	if a.Boost != 0 || a.Undervolt != 0 {
		line("overclock applied", "boost %d MHz, undervolt %d mV", a.Boost, a.Undervolt)
	}
	line("reference", "%d MHz at %d mV (factory)", status.Reference.Megahertz, status.Reference.Millivolts)
	if c := status.Capabilities; c != nil {
		line("capabilities", "0x%08x (maximum %d MHz, voltage override %v, voltage offset %v)", c.Raw, c.MaximumMegahertz, c.VoltageOverride, c.VoltageOffset)
	}
	k := status.Clock
	line("clock", "RP0 %d, RP1 %d, RPn %d MHz; driver range %d..%d MHz", k.RP0, k.RP1, k.RPn, k.Minimum, k.Maximum)
	f, units := status.Factory, gpu.Units{Power: 8, Time: 1024}
	line("factory values", "built in: %.2f/%.2f W, window %.0f s, %d C, %.0f A",
		gpu.DecodeLimit(f.RaplLow, units).Watts, gpu.DecodeLimit(f.RaplHigh, units).Watts,
		gpu.DecodeLimit(f.RaplLow, units).WindowSeconds, f.Temperature, f.IccMax)
	if a.ConfigPath != "" {
		result := "ok"
		if !a.ApplySuccess {
			result = "errors: " + strings.Join(a.ApplyErrors, "; ")
		}
		line("configuration", "%s applied %s (%s)", a.ConfigPath, a.LastApply.Format(time.RFC3339), result)
	}
	var resets []string
	for group, active := range a.ResetActive {
		if active {
			resets = append(resets, group)
		}
	}
	if len(resets) > 0 {
		sort.Strings(resets)
		line("factory reset active", "%s", strings.Join(resets, ", "))
	}
	for _, err := range status.Errors {
		line("error", "%s", err)
	}
}

func throttleText(throttle map[string]bool) string {
	var active []string
	for _, name := range []string{"power", "current_voltage", "thermal", "utilization"} {
		if throttle[name] {
			active = append(active, name)
		}
	}
	if len(active) == 0 {
		return "none"
	}
	return strings.Join(active, ",")
}

// PrintSensors renders one snapshot.
func PrintSensors(out io.Writer, s gpu.Sensors) {
	line := func(name, format string, arguments ...any) {
		fmt.Fprintf(out, "%-22s %s\n", name, fmt.Sprintf(format, arguments...))
	}
	if !s.Available {
		line("card", "not responding (reads all ones: resetting, resuming or off the bus)")
		return
	}
	line("gpu temperature", "%d C", s.GPUTemperature)
	if s.VramTemperature >= 0 {
		line("vram temperature", "%d C", s.VramTemperature)
	}
	line("power", "%.1f W", s.PowerWatts)
	line("gpu voltage", "%.1f mV", s.VoltageMillivolt)
	line("gpu clock", "%d MHz (requested %d MHz)", s.ActualMegahertz, s.RequestMegahertz)
	line("busy", "global %.1f%%, render %.1f%%, media %.1f%%", s.Busy["global"], s.Busy["render"], s.Busy["media"])
	line("vram bandwidth", "read %.2f GB/s, write %.2f GB/s (%.1f%% of peak)", s.VramReadGBs, s.VramWriteGBs, s.VramUtilization)
	line("vram", "%d MT/s (%d MHz), %d MB used", s.VramRate, s.VramMegahertz, s.VramUsedBytes>>20)
	line("fan", "%d RPM (%d%%)", s.FanRPM, s.FanPercent)
	line("throttle", "%s (0x%08x)", throttleText(s.Throttle), s.ThrottleRaw)
}

// Watch prints a live table until the context is cancelled (Ctrl+C).
func Watch(ctx context.Context, out io.Writer, backend Backend) error {
	fmt.Fprintf(out, "%-8s %5s %5s %6s %7s %5s %6s %8s %8s %5s %s\n",
		"time", "gpuC", "vramC", "watts", "mV", "MHz", "busy%", "read GB", "write GB", "rpm", "throttle")
	for ctx.Err() == nil {
		sensors, err := backend.Sensors(time.Second)
		if err != nil {
			return err
		}
		if !sensors.Available {
			fmt.Fprintf(out, "%-8s card not responding\n", time.Now().Format("15:04:05"))
			continue
		}
		vram := "-"
		if sensors.VramTemperature >= 0 {
			vram = fmt.Sprint(sensors.VramTemperature)
		}
		fmt.Fprintf(out, "%-8s %5d %5s %6.1f %7.1f %5d %6.1f %8.2f %8.2f %5d %s\n",
			time.Now().Format("15:04:05"), sensors.GPUTemperature, vram, sensors.PowerWatts,
			sensors.VoltageMillivolt, sensors.ActualMegahertz, sensors.Busy["global"],
			sensors.VramReadGBs, sensors.VramWriteGBs, sensors.FanRPM, throttleText(sensors.Throttle))
	}
	return nil
}

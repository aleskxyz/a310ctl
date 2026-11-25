package daemon

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type collector struct{ daemon *Daemon }

func description(name, help string, labels ...string) *prometheus.Desc {
	return prometheus.NewDesc("a310_"+name, help, labels, nil)
}

var (
	descUp              = description("up", "1 when the card answered in the latest sample")
	descSampleAge       = description("sample_age_seconds", "Age of the latest telemetry sample")
	descInfo            = description("info", "Device and software information", "pci_address", "driver", "subsystem", "version")
	descGpuTemp         = description("gpu_temperature_celsius", "GPU package temperature")
	descVramTemp        = description("vram_temperature_celsius", "VRAM temperature")
	descPower           = description("power_watts", "GPU package power")
	descEnergy          = description("energy_joules_total", "GPU package energy counted by the daemon")
	descVoltage         = description("gpu_voltage_volts", "GPU voltage (0 while idle)")
	descFrequency       = description("gpu_frequency_hertz", "GPU clock", "kind")
	descClockLimit      = description("clock_limit_hertz", "Clock limits", "limit")
	descBusy            = description("engine_busy_ratio", "Engine busy ratio", "engine")
	descVramBandwidth   = description("vram_bandwidth_bytes_per_second", "VRAM bandwidth", "direction")
	descVramBytes       = description("vram_transferred_bytes_total", "VRAM bytes counted by the daemon", "direction")
	descVramUsed        = description("vram_used_bytes", "VRAM resident in DRM clients")
	descVramRate        = description("vram_data_rate_megatransfers", "VRAM data rate")
	descVramUtilization = description("vram_bandwidth_utilization_ratio", "VRAM bandwidth used vs peak")
	descFanRPM          = description("fan_rpm", "Fan speed")
	descFanRatio        = description("fan_ratio", "Fan speed vs measured full speed")
	descThrottle        = description("throttle_active", "Active limit reason", "reason")
	descThrottleRaw     = description("throttle_raw", "Raw limit-reason register")
	descPcieSpeed       = description("pcie_link_speed_gigatransfers", "PCIe link speed (switch upstream port)")
	descPcieWidth       = description("pcie_link_width", "PCIe link width")

	descPowerLimit       = description("power_limit_watts", "Power limit read back from hardware", "limit")
	descPowerLimitOn     = description("power_limit_enabled", "Power limit enable bit", "limit")
	descPowerWindow      = description("power_limit_window_seconds", "Power limit time window", "limit")
	descPowerLocked      = description("power_limit_locked", "RAPL lock bit")
	descTemperatureLimit = description("temperature_limit_celsius", "GPU temperature (throttle) limit")
	descIccMax           = description("iccmax_amperes", "Maximum current")
	descVfTarget         = description("overclock_voltage_target_volts", "Programmed voltage target (0 = stock)")
	descVfOffset         = description("overclock_voltage_offset_volts", "Programmed voltage offset")
	descVfMode           = description("overclock_voltage_mode", "Voltage mode", "mode")
	descVfFrequency      = description("overclock_frequency_target_hertz", "Programmed frequency target")
	descReference        = description("overclock_reference", "Stock voltage-frequency reference (volts / hertz)", "quantity")
	descUnlocked         = description("overclock_unlocked", "Overclock unlocked (as last set by a310ctl; -1 unknown)")
	descBoost            = description("overclock_boost_hertz", "Applied boost (as last set by a310ctl)")
	descUndervolt        = description("overclock_undervolt_volts", "Applied extra undervolt (as last set by a310ctl)")
	descFanPoint         = description("fan_curve_duty_ratio", "Applied fan curve point (as last set by a310ctl)", "point", "temperature_celsius")
	descFanMode          = description("fan_curve_mode", "Applied fan curve mode", "mode")
	descConfigInfo       = description("config_info", "Applied configuration file", "path", "sha256")
	descConfigApply      = description("config_last_apply_timestamp_seconds", "Time of the last configuration apply")
	descConfigSuccess    = description("config_apply_success", "1 if the last apply succeeded")
	descConfigDrift      = description("config_drift", "1 when hardware read-back differs from the configuration", "setting")
	descResetActive      = description("factory_reset_active", "1 if the group was reset and not re-applied since", "group")
	descPowerMarker      = description("power_reset_marker", "1 while the burst limit is factory plus one step, to detect card resets")
	descRecoveries       = description("recoveries_total", "Re-applies after the card lost power", "reason", "result")
	descLastRecovery     = description("last_recovery_timestamp_seconds", "Time of the last re-apply after power loss")
	descRecoveryPaused   = description("recovery_paused", "1 when automatic recovery is paused (card kept losing its settings)")
)

// Describe sends nothing: an unchecked collector, as label sets vary with the config.
func (collector) Describe(chan<- *prometheus.Desc) {}

func boolean(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

// Collect publishes the latest sample, the runtime configuration and recovery state.
func (c collector) Collect(channel chan<- prometheus.Metric) {
	gauge := func(desc *prometheus.Desc, value float64, labels ...string) {
		channel <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
	}
	counter := func(desc *prometheus.Desc, value float64, labels ...string) {
		channel <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, value, labels...)
	}
	if c.daemon == nil {
		return
	}
	// Recovery state is published even while the card is unavailable.
	c.daemon.sampleLock.Lock()
	for reason, results := range c.daemon.recoveries {
		for result, count := range results {
			counter(descRecoveries, count, reason, result)
		}
	}
	if !c.daemon.lastRecovery.IsZero() {
		gauge(descLastRecovery, float64(c.daemon.lastRecovery.Unix()))
	}
	gauge(descRecoveryPaused, boolean(c.daemon.recoveryPaused))
	c.daemon.sampleLock.Unlock()
	sample := c.daemon.latest()
	if sample == nil {
		gauge(descUp, 0)
		return
	}
	if sample.Unavailable {
		gauge(descUp, 0)
		gauge(descSampleAge, time.Since(sample.Time).Seconds())
		return
	}
	gauge(descUp, 1)
	gauge(descSampleAge, time.Since(sample.Time).Seconds())
	s, st := sample.Sensors, sample.Status
	gauge(descInfo, 1, st.Address, st.Driver, st.Subsystem, Version)

	gauge(descGpuTemp, float64(s.GPUTemperature))
	if s.VramTemperature >= 0 {
		gauge(descVramTemp, float64(s.VramTemperature))
	}
	gauge(descPower, s.PowerWatts)
	counter(descEnergy, sample.EnergyTotal)
	gauge(descVoltage, s.VoltageMillivolt/1000)
	gauge(descFrequency, float64(s.ActualMegahertz)*1e6, "actual")
	gauge(descFrequency, float64(s.RequestMegahertz)*1e6, "requested")
	for name, value := range map[string]int{"rp0": st.Clock.RP0, "rp1": st.Clock.RP1, "rpn": st.Clock.RPn, "minimum": st.Clock.Minimum, "maximum": st.Clock.Maximum} {
		gauge(descClockLimit, float64(value)*1e6, name)
	}
	for engine, value := range s.Busy {
		gauge(descBusy, value/100, engine)
	}
	gauge(descVramBandwidth, s.VramReadGBs*1e9, "read")
	gauge(descVramBandwidth, s.VramWriteGBs*1e9, "write")
	counter(descVramBytes, sample.VramReadTotal, "read")
	counter(descVramBytes, sample.VramWriteTotal, "write")
	gauge(descVramUsed, float64(s.VramUsedBytes))
	gauge(descVramRate, float64(s.VramRate))
	gauge(descVramUtilization, s.VramUtilization/100)
	gauge(descFanRPM, float64(s.FanRPM))
	gauge(descFanRatio, float64(s.FanPercent)/100)
	for reason, active := range s.Throttle {
		gauge(descThrottle, boolean(active), reason)
	}
	gauge(descThrottleRaw, float64(s.ThrottleRaw))
	gauge(descPcieSpeed, st.Pcie.SpeedGT)
	gauge(descPcieWidth, float64(st.Pcie.Width))

	for name, limit := range map[string]struct {
		watts, window float64
		on            bool
	}{"sustained": {st.Power.Sustained.Watts, st.Power.Sustained.WindowSeconds, st.Power.Sustained.Enabled},
		"burst": {st.Power.Burst.Watts, st.Power.Burst.WindowSeconds, st.Power.Burst.Enabled}} {
		gauge(descPowerLimit, limit.watts, name)
		gauge(descPowerLimitOn, boolean(limit.on), name)
		gauge(descPowerWindow, limit.window, name)
	}
	gauge(descPowerLocked, boolean(st.Power.Locked))
	gauge(descTemperatureLimit, float64(st.TemperatureLimit))
	if st.IccMax != nil {
		gauge(descIccMax, *st.IccMax)
	}
	vf := st.Overclock.VoltageFrequency
	gauge(descVfTarget, float64(vf.Millivolts)/1000)
	gauge(descVfOffset, float64(vf.Offset)/1000)
	gauge(descVfMode, boolean(vf.Mode == "adaptive"), "adaptive")
	gauge(descVfMode, boolean(vf.Mode == "fixed"), "fixed")
	gauge(descVfFrequency, float64(st.Overclock.Megahertz)*1e6)
	gauge(descReference, float64(st.Reference.Millivolts)/1000, "volts")
	gauge(descReference, float64(st.Reference.Megahertz)*1e6, "hertz")

	applied := st.Applied
	unlocked := -1.0
	if applied.Unlocked != nil {
		unlocked = boolean(*applied.Unlocked)
	}
	gauge(descUnlocked, unlocked)
	gauge(descBoost, float64(applied.Boost)*1e6)
	gauge(descUndervolt, float64(applied.Undervolt)/1000)
	for index, point := range applied.FanPoints {
		gauge(descFanPoint, float64(point.Percent)/100, strconv.Itoa(index+1), strconv.Itoa(point.Celsius))
	}
	for _, mode := range []string{"points", "flat", "factory", "unknown"} {
		gauge(descFanMode, boolean(applied.FanMode == mode), mode)
	}
	if applied.ConfigPath != "" {
		gauge(descConfigInfo, 1, applied.ConfigPath, applied.ConfigHash)
		gauge(descConfigApply, float64(applied.LastApply.Unix()))
		gauge(descConfigSuccess, boolean(applied.ApplySuccess))
	}
	for setting, drift := range sample.Drift {
		gauge(descConfigDrift, boolean(drift), setting)
	}
	for _, group := range []string{"power", "temperature", "overclock"} {
		gauge(descResetActive, boolean(applied.ResetActive[group]), group)
	}
	gauge(descPowerMarker, boolean(st.MarkerActive()))
}

func (daemon *Daemon) metricsHandler() http.Handler {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector{daemon: daemon})
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Write([]byte("a310ctl exporter: see /metrics\n"))
	})
	return mux
}

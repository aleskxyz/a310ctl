// Package daemon implements `a310ctl serve`: apply and watch the YAML, serve the control
// socket and the optional Prometheus exporter.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"a310ctl/internal/config"
	"a310ctl/internal/controller"
	"a310ctl/internal/gpu"
	"a310ctl/internal/hardware"
)

// Version is set by main.
var Version = "dev"

// Sample is the latest telemetry and the accumulated counters.
type Sample struct {
	Time           time.Time
	Sensors        gpu.Sensors
	Status         controller.Status
	EnergyTotal    float64
	VramReadTotal  float64
	VramWriteTotal float64
	Drift          map[string]bool
	Unavailable    bool // card read all ones: publish up 0 only
}

// Daemon state: the first group is guarded by sampleLock, "main loop only" fields are
// used by the Serve goroutine alone.
type Daemon struct {
	path    string
	control *controller.Controller
	log     *slog.Logger

	sampleLock     sync.Mutex
	sample         *Sample
	recoveries     map[string]map[string]float64 // reason -> result -> count
	lastRecovery   time.Time
	resetDetected  time.Time // last suspend or power loss detected
	recoveryPaused bool      // stopped until the next config change or reload
	energyTotal    float64   // kept across exporter restarts
	vramReadTotal  float64
	vramWriteTotal float64

	// main loop only
	current         *config.Config // last valid configuration
	pendingRecovery string         // recovery still to do ("resume" or "power_loss")
	nextRecovery    time.Time      // earliest next recovery attempt
	recoveryDelay   time.Duration
	powerLosses     []time.Time // power-loss recoveries in the last minute
	nextRetry       time.Time   // earliest next retry of failed groups
	retryDelay      time.Duration
	exporter        *http.Server
	exporterOn      config.Exporter
	stopSample      chan struct{}
	samplerDone     sync.WaitGroup
	exporterFailed  chan error
}

const (
	// recoverySettle lets the kernel driver finish its own resume first.
	recoverySettle = 3 * time.Second
	// recoveryWait is how long one attempt waits for the card.
	recoveryWait = 10 * time.Second
	// maximumBackoff caps recovery and retry delays.
	maximumBackoff = 2 * time.Minute
	// recoveryLoopLimit pauses recovery after this many power losses within a minute
	// (e.g. repeated GPU hangs).
	// startupWait is how long serve waits for the card at boot (driver not bound yet).
	startupWait       = 30 * time.Second
	recoveryLoopLimit = 5
)

// wait sleeps for duration; false if ctx was cancelled first.
func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// recordRecovery counts a recovery attempt for the exporter.
func (daemon *Daemon) recordRecovery(reason, result string) {
	daemon.sampleLock.Lock()
	daemon.recoveries[reason][result]++
	daemon.lastRecovery = time.Now()
	daemon.sampleLock.Unlock()
}

// recover re-applies the configuration after a power loss; false (retry with backoff) if
// the card did not answer, the wait was cancelled or the re-apply failed.
func (daemon *Daemon) recover(ctx context.Context, reason string) bool {
	daemon.log.Warn("card lost its settings, re-applying configuration", "reason", reason)
	deadline := time.Now().Add(recoveryWait)
	for !daemon.control.Ready() {
		if time.Now().After(deadline) {
			daemon.log.Error("card does not answer; retrying later", "retry_in", daemon.recoveryDelay.String())
			return false
		}
		if !wait(ctx, 500*time.Millisecond) {
			return false
		}
	}
	if !wait(ctx, recoverySettle) {
		return false
	}
	errs := daemon.control.ApplyAfterPowerLoss(daemon.current)
	for _, err := range errs {
		daemon.log.Error("re-apply failed", "error", err)
	}
	if len(errs) > 0 {
		daemon.recordRecovery(reason, "error")
		return false
	}
	daemon.log.Info("configuration re-applied", "reason", reason)
	daemon.recordRecovery(reason, "success")
	return true
}

// Serve runs until SIGINT/SIGTERM.
func Serve(path string) error {
	// Catch stop signals first: a reload during startup must not kill the daemon.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	defer signal.Stop(sighup)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	lastStat, _ := os.Stat(path) // before Load, so an edit during startup is noticed
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	device, err := openDevice(ctx, cfg.Device, logger)
	if ctx.Err() != nil {
		logger.Info("stopping")
		return nil
	}
	if err != nil {
		return err
	}
	control := controller.New(device)
	zeros := func() map[string]float64 { return map[string]float64{"success": 0, "error": 0} }
	daemon := &Daemon{path: path, control: control, log: logger,
		recoveries:     map[string]map[string]float64{"resume": zeros(), "power_loss": zeros()},
		exporterFailed: make(chan error, 1)}
	logger.Info("device opened", "address", device.Address, "driver", device.Driver(), "version", Version)

	listener, err := daemon.listenSocket(cfg.Socket)
	if err != nil {
		control.Close()
		return err
	}
	socketServer := &http.Server{Handler: daemon.socketHandler(), ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout: 30 * time.Second}
	go socketServer.Serve(listener)

	// Stop all device users before closing it, so nothing touches unmapped registers.
	defer func() {
		daemon.stopExporter()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		socketServer.Shutdown(ctx)
		cancel()
		os.Remove(cfg.Socket)
		control.Close()
	}()

	daemon.apply(cfg, true)
	daemon.configureExporter(cfg.Exporter)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	sleeper := newSleepDetector()
	lastHash := cfg.Hash
	for {
		select {
		case <-ctx.Done():
			logger.Info("stopping")
			return nil
		case <-sighup:
			logger.Info("SIGHUP: re-applying configuration")
			if next, err := config.Load(path); err != nil {
				logger.Error("configuration invalid, keeping current", "error", err)
			} else {
				daemon.warnRestartNeeded(cfg, next)
				daemon.apply(next, true)
				daemon.configureExporter(next.Exporter)
				daemon.pauseRecovery(false)
				lastHash = next.Hash
			}
		case err := <-daemon.exporterFailed:
			logger.Error("exporter failed; it is retried on the next configuration change or SIGHUP", "error", err)
			daemon.stopExporter()
		case <-ticker.C:
			daemon.checkRecovery(ctx, sleeper)
			if ctx.Err() != nil {
				logger.Info("stopping")
				return nil
			}
			daemon.retryFailed()
			stat, err := os.Stat(path)
			if err != nil || (lastStat != nil && stat.ModTime().Equal(lastStat.ModTime()) && stat.Size() == lastStat.Size()) {
				continue
			}
			lastStat = stat
			next, err := config.Load(path)
			if err != nil {
				logger.Error("configuration invalid, keeping current", "error", err)
				continue
			}
			if next.Hash == lastHash {
				continue
			}
			lastHash = next.Hash
			logger.Info("configuration changed, applying")
			daemon.warnRestartNeeded(cfg, next)
			daemon.apply(next, false)
			daemon.configureExporter(next.Exporter)
			daemon.pauseRecovery(false)
		}
	}
}

// countPowerLoss adds a recovery at now, drops those older than a minute, and reports
// whether recoveryLoopLimit is reached.
func countPowerLoss(times []time.Time, now time.Time) ([]time.Time, bool) {
	recent := times[:0]
	for _, at := range times {
		if now.Sub(at) < time.Minute {
			recent = append(recent, at)
		}
	}
	recent = append(recent, now)
	return recent, len(recent) >= recoveryLoopLimit
}

// openDevice opens the card, waiting up to startupWait; a stop signal ends the wait.
func openDevice(ctx context.Context, address string, logger *slog.Logger) (*hardware.Device, error) {
	deadline := time.Now().Add(startupWait)
	for {
		device, err := hardware.Open(address)
		if err == nil || errors.Is(err, hardware.ErrBusy) || time.Now().After(deadline) {
			return device, err
		}
		logger.Info("waiting for the card", "error", err)
		if !wait(ctx, 2*time.Second) {
			return nil, ctx.Err()
		}
	}
}

// pauseRecovery sets or clears the recovery pause (exported as a metric).
func (daemon *Daemon) pauseRecovery(paused bool) {
	daemon.sampleLock.Lock()
	daemon.recoveryPaused = paused
	daemon.sampleLock.Unlock()
	if !paused {
		daemon.powerLosses = nil
	}
}

func (daemon *Daemon) paused() bool {
	daemon.sampleLock.Lock()
	defer daemon.sampleLock.Unlock()
	return daemon.recoveryPaused
}

// markResetDetected tells the sampler that counters restarted.
func (daemon *Daemon) markResetDetected() {
	daemon.sampleLock.Lock()
	daemon.resetDetected = time.Now()
	daemon.sampleLock.Unlock()
}

// checkRecovery re-applies the configuration after a suspend or power loss, with
// backoff and a pause when the card keeps losing its settings.
func (daemon *Daemon) checkRecovery(ctx context.Context, sleeper *sleepDetector) {
	if slept := sleeper.check(); slept > 2*time.Second {
		daemon.log.Info("system was suspended", "duration", slept.Round(time.Second).String())
		daemon.pendingRecovery, daemon.nextRecovery, daemon.recoveryDelay = "resume", time.Time{}, 0
		daemon.markResetDetected()
	} else if daemon.pendingRecovery == "" && !daemon.paused() && daemon.control.PowerLost() {
		daemon.pendingRecovery, daemon.nextRecovery, daemon.recoveryDelay = "power_loss", time.Time{}, 0
		daemon.markResetDetected()
	}
	reason := daemon.pendingRecovery
	if reason == "" || time.Now().Before(daemon.nextRecovery) {
		return
	}
	daemon.recoveryDelay = min(max(2*daemon.recoveryDelay, 2*time.Second), maximumBackoff)
	if !daemon.recover(ctx, reason) {
		if ctx.Err() != nil {
			return
		}
		daemon.nextRecovery = time.Now().Add(daemon.recoveryDelay)
		return
	}
	daemon.pendingRecovery = ""
	if reason != "power_loss" {
		return
	}
	var loop bool
	daemon.powerLosses, loop = countPowerLoss(daemon.powerLosses, time.Now())
	if loop {
		daemon.pauseRecovery(true)
		daemon.log.Error("the card keeps losing its settings; automatic recovery paused until the configuration changes or a reload",
			"recoveries_last_minute", recoveryLoopLimit)
	}
}

// retryFailed re-applies failed groups, with backoff.
func (daemon *Daemon) retryFailed() {
	if daemon.current == nil || !daemon.control.HasFailures() {
		daemon.retryDelay = 0
		return
	}
	if daemon.pendingRecovery != "" || time.Now().Before(daemon.nextRetry) {
		return
	}
	daemon.retryDelay = min(max(2*daemon.retryDelay, 10*time.Second), maximumBackoff)
	daemon.nextRetry = time.Now().Add(daemon.retryDelay)
	errs := daemon.control.RetryFailed(daemon.current)
	for _, err := range errs {
		daemon.log.Error("retry failed", "error", err, "next_retry", daemon.retryDelay.String())
	}
	if len(errs) == 0 {
		daemon.log.Info("failed groups applied on retry")
	}
}

// warnRestartNeeded logs settings that only take effect after a restart.
func (daemon *Daemon) warnRestartNeeded(started, next *config.Config) {
	if next.Socket != started.Socket {
		daemon.log.Warn("socket path change needs a restart", "socket", next.Socket)
	}
	if next.Device != started.Device {
		daemon.log.Warn("device change needs a restart", "device", next.Device)
	}
}

// apply applies cfg and logs the result.
func (daemon *Daemon) apply(cfg *config.Config, full bool) {
	daemon.current = cfg
	daemon.nextRetry, daemon.retryDelay = time.Now().Add(10*time.Second), 0
	errs := daemon.control.Apply(cfg, full)
	for _, err := range errs {
		daemon.log.Error("apply failed", "error", err)
	}
	if len(errs) == 0 {
		daemon.log.Info("configuration applied", "path", cfg.Path, "full", full)
	}
}

// listenSocket creates the control socket (root only), refusing one in use.
func (daemon *Daemon) listenSocket(path string) (net.Listener, error) {
	if connection, err := net.DialTimeout("unix", path, 300*time.Millisecond); err == nil {
		connection.Close()
		return nil, fmt.Errorf("socket %s is in use by another daemon", path)
	}
	os.Remove(path)
	old := unix.Umask(0o077)
	listener, err := net.Listen("unix", path)
	unix.Umask(old)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, err
	}
	return listener, nil
}

func writeJSON(writer http.ResponseWriter, code int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(code)
	json.NewEncoder(writer).Encode(value)
}

// socketHandler serves GET /v1/status, GET /v1/sensors and POST /v1/reset.
func (daemon *Daemon) socketHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", func(writer http.ResponseWriter, _ *http.Request) {
		status := daemon.control.Status()
		status.Daemon = true
		writeJSON(writer, http.StatusOK, status)
	})
	mux.HandleFunc("GET /v1/sensors", func(writer http.ResponseWriter, request *http.Request) {
		window, err := strconv.ParseFloat(request.URL.Query().Get("window"), 64)
		if err != nil || !(window > 0) {
			window = 1
		}
		window = min(max(window, controller.SensorsWindowMin.Seconds()), controller.SensorsWindowMax.Seconds())
		sensors, err := daemon.control.Sensors(time.Duration(window * float64(time.Second)))
		if err != nil {
			writeJSON(writer, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(writer, http.StatusOK, sensors)
	})
	mux.HandleFunc("POST /v1/reset", func(writer http.ResponseWriter, request *http.Request) {
		group := request.URL.Query().Get("group")
		if group == "" {
			group = "all"
		}
		if !slices.Contains(controller.Groups, group) {
			writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "unknown group " + strconv.Quote(group)})
			return
		}
		if err := daemon.control.Reset(group); err != nil {
			daemon.log.Error("factory reset failed", "group", group, "error", err)
			writeJSON(writer, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		daemon.log.Info("factory reset", "group", group)
		writeJSON(writer, http.StatusOK, map[string]string{"result": "reset " + group})
	})
	return mux
}

// configureExporter starts, restarts or stops the exporter as configured.
func (daemon *Daemon) configureExporter(exporter config.Exporter) {
	if daemon.exporter != nil && exporter == daemon.exporterOn {
		return
	}
	running := daemon.exporter != nil
	daemon.stopExporter()
	daemon.exporterOn = exporter
	if !exporter.Enabled {
		if running {
			daemon.log.Info("exporter stopped")
		}
		return
	}
	listener, err := net.Listen("tcp", exporter.Listen)
	if err != nil {
		daemon.log.Error("exporter cannot listen; retried on the next configuration change or SIGHUP", "address", exporter.Listen, "error", err)
		return
	}
	stop := make(chan struct{})
	daemon.stopSample = stop
	daemon.samplerDone.Add(1)
	go func() {
		defer daemon.samplerDone.Done()
		daemon.sampler(time.Duration(exporter.Interval*float64(time.Second)), stop)
	}()
	server := &http.Server{Handler: daemon.metricsHandler(), ReadHeaderTimeout: 5 * time.Second}
	daemon.exporter = server
	daemon.log.Info("exporter listening", "address", exporter.Listen)
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			select {
			case daemon.exporterFailed <- err:
			default:
			}
		}
	}()
}

// stopExporter stops the server and waits for the sampler.
func (daemon *Daemon) stopExporter() {
	if daemon.stopSample != nil {
		close(daemon.stopSample)
		daemon.stopSample = nil
	}
	daemon.samplerDone.Wait()
	if daemon.exporter != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		daemon.exporter.Shutdown(ctx)
		cancel()
		daemon.exporter = nil
	}
}

// sampler collects telemetry for the exporter.
func (daemon *Daemon) sampler(interval time.Duration, stop chan struct{}) {
	previous, err := daemon.control.Snapshot()
	if err != nil {
		return
	}
	sleeper := newSleepDetector()
	skipNext := false // the interval after an unavailable sample starts from garbage
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		current, err := daemon.control.Snapshot()
		if err != nil {
			return
		}
		sensors, err := daemon.control.Compute(previous, current)
		if err != nil {
			return
		}
		intervalStart := previous.Time
		previous = current
		if !sensors.Available {
			daemon.sampleLock.Lock()
			daemon.sample = &Sample{Time: time.Now(), Unavailable: true}
			daemon.sampleLock.Unlock()
			skipNext = true
			continue
		}
		// Counters restart after a suspend or power loss: skip such intervals.
		daemon.sampleLock.Lock()
		reset := daemon.resetDetected.After(intervalStart.Add(-interval)) || daemon.lastRecovery.After(intervalStart)
		daemon.sampleLock.Unlock()
		skip := skipNext
		skipNext = false
		if sleeper.check() > time.Second || reset || skip ||
			sensors.PowerWatts > 500 || sensors.VramReadGBs > 1000 || sensors.VramWriteGBs > 1000 {
			daemon.log.Info("skipping telemetry interval after a counter reset")
			continue
		}
		status := daemon.control.Status()
		status.Daemon = true
		drift := controller.Drift(daemon.control.Current(), status)
		daemon.sampleLock.Lock()
		daemon.energyTotal += sensors.EnergyJoules
		daemon.vramReadTotal += sensors.VramReadBytes
		daemon.vramWriteTotal += sensors.VramWriteBytes
		daemon.sample = &Sample{
			Time: time.Now(), Sensors: sensors, Status: status,
			EnergyTotal: daemon.energyTotal, VramReadTotal: daemon.vramReadTotal,
			VramWriteTotal: daemon.vramWriteTotal, Drift: drift,
		}
		daemon.sampleLock.Unlock()
	}
}

// latest returns the newest sample, or nil.
func (daemon *Daemon) latest() *Sample {
	daemon.sampleLock.Lock()
	defer daemon.sampleLock.Unlock()
	return daemon.sample
}

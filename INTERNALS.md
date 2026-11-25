# a310ctl internals

How a310ctl reaches the hardware, what the registers mean and how the parts fit together.
User documentation is in the [README](README.md); test results are in
[BENCHMARKS.md](BENCHMARKS.md) and the driver comparison in [DRIVERS.md](DRIVERS.md).

The register meanings were reverse-engineered from the Windows driver 32.0.101.9175
(`igdkmdnd64.sys`, `IntelControlLib.dll`, Intel Graphics Software) and verified on a
Sparkle A310 (`8086:56a6`, subsystem `172f:4019`).

## Layout

```
cmd/a310ctl/          command line: serve | check | status | sensors [watch] | reset | version
internal/hardware/    device discovery, BAR0 mapping, MMIO, PCODE mailbox, lock, runtime PM
internal/gpu/         register encodings and operations (stateless)
  power.go            power limits, units, SKU; temperature limit
  overclock.go        capabilities, voltage-frequency word, lock, boost model, IccMax
  fan.go              fan table
  clock.go            RP0/RP1/RPn and the driver's min/max (read-only)
  sensors.go          telemetry, VRAM used, PCIe link
  factory.go          built-in factory values
internal/config/      YAML schema and validation
internal/controller/  serialized hardware access: apply, reset, status
internal/daemon/      serve loop, socket API, exporter, sleep/power-loss recovery
internal/client/      CLI backends (socket or direct) and output
packaging/            systemd unit, default configuration, nFPM definition, package scripts
```

Dependencies: `gopkg.in/yaml.v3`, `github.com/prometheus/client_golang`,
`golang.org/x/sys/unix`. Static build (`CGO_ENABLED=0`).

## Hardware access

- **Device:** the first `8086:56a6` in `/sys/bus/pci/devices`, or the configured address
  (any other device is refused).
- **BAR0:** 4 MiB `mmap` of `resource0` (covers everything up to `0x36C5C0`). 32-bit atomic
  reads and writes; 64-bit counters are read high, low, high so they never tear.
- **Runtime PM:** `power/control` is `on` while the device is open, restored on close.
- **Lock:** `flock(/run/a310ctl.lock)`, held by the daemon for its lifetime; a second
  daemon or direct CLI gets `ErrBusy`.
- **PCODE mailbox:** `0x138124` (mailbox), `0x138128` / `0x13812C` (DATA0/1). Mailbox dword
  = `READY[31] | command[7:0] | subcommand[15:8] | parameter[23:16]`. Wait for READY to
  clear, write DATA, write the mailbox, wait again; status is `[7:0]` (0 = ok). The kernel
  driver uses the same mailbox, so a rare collision is possible.

`internal/gpu` uses the `Register` interface (Read32, Write32, PcodeRead, PcodeWrite).

## Registers

MCHBAR registers are mirrored in BAR0 at `0x140000` (the Windows driver uses offsets such as
`0x59A0`).

| What | Access | Encoding |
|---|---|---|
| Power units | MMIO `0x145938` | power `1/2^[3:0]` W, energy `1/2^[12:8]` J, time `1/2^[19:16]` s (A310: 1/8 W, 1/16384 J, 1/1024 s) |
| Power SKU | MMIO `0x145930` (64-bit) | TDP `[14:0]`, min `[30:16]`, max `[46:32]` (all 0 on the A310) |
| Power limits | MMIO `0x1459A0` (sustained) / `0x1459A4` (burst) | value `[14:0]`, enable `[15]`, window `[23:17]` = `2^Y·(1+Z/4)` time units (Y `[4:0]`, Z `[6:5]`); high dword bit 31 = lock |
| Energy | MMIO `0x14593C` | 32-bit counter, energy units |
| GPU temperature | MMIO `0x145978` | `[7:0]` °C |
| VRAM temperature | MMIO `0x1382C0` | `[7:0]` °C, `0x18` = invalid |
| Voltage / clock | MMIO `0x1381B4` | voltage `[10:0]`·2.5 mV, clock `[19:11]`·50/3 MHz (0 in RC6) |
| Requested clock | MMIO `0xA008` | `round([31:23]/3)`·50 MHz |
| Limit reasons | MMIO `0x145900` | power `0xC00`, current/voltage bit 8, thermal `0xFB`, utilization bit 12 |
| RP capabilities | MMIO `0x145998` | RP0 `[7:0]`, RP1 `[15:8]`, RPn `[23:16]` ·50 MHz |
| Busy counters | MMIO `0xDB80` / `0xDBDC` / `0xDBA0` | global / render / media, 19.2 MHz / 16 |
| VRAM traffic | MMIO `0x36C448..0x36C4C0`, `0x36C548..0x36C5C0` (read), `0x36C4C8..0x36C540` (write) | 64-bit counters ·32 bytes |
| Fan tach | MMIO `0x138140` | 2 pulses per revolution |
| Fan table | PCODE `0x7D` sub 1 / 2 | sub 1: point count (0 = firmware curve); sub 2: two points, point = `celsius \| percent<<8` |
| Temperature limit | PCODE `0x72` / `0x73` | write / read, °C |
| Overclock capabilities | PCODE `0x7A` sub 1 | max ratio `[7:0]`·50 MHz, V override `[9]`, V offset `[10]`, new interface `[13]` (A310: `0x3f55`) |
| Reference point | PCODE `0x79` sub 1 | ratio `[7:0]`·50 MHz, voltage `[19:8]` 1/1024 V; drifts while unlocked (910 → 941–960 mV) |
| Voltage-frequency | PCODE `0x6C` sub 1 | param 0/1 read/write the word, 2/3 read/write the frequency target (MHz), 4 lock (0 = unlock, 1 = lock) |
| VF word | — | target voltage `[19:8]` 1/1024 V, fixed mode `[20]`, offset `[31:21]` signed mV |
| IccMax | PCODE `0x75` | `[9:0]`/4 A |
| VRAM data rate | PCODE `0x6F` sub 1 | MT/s (15494 on the A310) |

Not available on DG2: PL4 and I1 reads (`0x7C` sub 3/4 return error `0x04`), V/F curve
editing, VRAM speed and offsets, card/VRAM energy, VR temperatures.

Behaviour worth knowing:

- While overclocking is locked the firmware caps power at ~31.25 W whatever the limits say.
  Unlocking (`0x6C` param 4 = 0 on domains 0 and 1) makes them apply.
- `i915`/`xe` never request more than RP0 (2450 MHz), so on Linux a boost only lowers the
  voltage.
- Offsets on the test card: −68 mV stable; −88 mV hung after minutes of load at 86–89 °C
  (once leaving the GPU wedged until reboot); −112 mV hung at once
  ([BENCHMARKS.md](BENCHMARKS.md#stability-and-limits)).

## Encodings

- **Boost** (Intel's model): `V(f) = 1.8684285714286e-7·f² − 4.91815769230768e-4·f` (V,
  MHz). For boost Δf over the reference f0/V0: `ΔV = V(f0+Δf) − V(f0)`; frequency `f0+Δf`,
  voltage `V0+ΔV`, offset `−ΔV − undervolt`. With boost 0 only the offset is written.
- **Write order** (as Windows): unlock, VF word, frequency, read back (±5).
- **Limits:** power 10–75 W, temperature 60–95 °C, frequency ≤ 2450+300 MHz, voltage
  ≤ 1120 mV, |offset| ≤ 150 mV, fixed voltage ≥ V0−250 mV.

## Factory values

`internal/gpu/factory.go`, measured after power cycles: power limits `0x00dc80fa` (31.25 W,
enabled, 28 s), 90 °C, 100 A, reference 2450 MHz / 910 mV, frequency target 0. Power and
temperature have no firmware reset, so these values are written. The fan (count 0), the VF
word (0) and the lock use the firmware's own resets.

## Configuration

- `Setting[T]` and `FanCurve` are absent, `factory` or a value. Groups accept `factory` or
  keys; unknown keys are errors.
- `Validate` checks ranges (NaN rejected), mode combinations, the effective boost offset
  and the fixed-voltage minimum. An empty file is valid and changes nothing.

## Controller

- One mutex serializes all hardware access. After `Close` calls return `ErrClosed` or do
  nothing. `Status` returns copies; capabilities and IccMax are read once and cached
  (omitted from JSON and metrics until a read succeeds). All-ones BAR0 sets
  `available: false` and skips MMIO/mailbox, matching sensors.
- **Apply order:** unlock/lock → power → temperature and fan → voltage-frequency.
- **What is applied:** groups that changed, failed last time, or were reset since; all
  groups on a full apply (start, reload, recovery). `RetryFailed` re-applies only failed
  groups and never touches reset ones. Omitted groups are never touched.
- **Write-only state** (`Applied`): unlock, fan curve, boost/undervolt, resets, last apply
  result.
- **Drift:** power and temperature read-back vs. the configuration (reset groups skipped).
- **Power-loss detection:** the card reads all ones, or the power register (a configured
  value or the reset marker) reads factory again. With the power group omitted, the
  temperature limit is checked instead, every 5th check (a mailbox read). After a failed
  write the expected value is the read-back. The VF word is not used: the firmware may
  clear it after a GPU hang, and re-applying that undervolt would be wrong.
- **Reset marker:** when the power group is factory (in the file or reset) and unlock, the
  fan curve or the temperature limit is not, the burst limit is written as factory + 1
  step (31.375 W), so a reset is visible. No effect while locked (~31.25 W firmware cap),
  +0.125 W unlocked; removed when not needed. Reserved RAPL bits cannot be used: the
  firmware drops them. A failed marker write is retried as failure `reset marker`.
- **Failed reset:** reported (`ApplySuccess` false, logged, socket 500), not retried:
  `RetryFailed` re-applies the configuration and would undo the reset.
- **Fan errors:** a half-written fan table falls back to the firmware curve.

## Daemon

- **Start:** catch signals, load the config, open the device (waits up to 30 s for the
  driver; a stop signal ends the wait), socket, full apply, exporter. The unit gives up
  after 5 failed starts in 2 minutes.
- **Every 2 s:**
  1. Suspend: `CLOCK_BOOTTIME` advanced over 2 s more than the monotonic clock.
  2. Otherwise power-loss detection.
  3. Recovery: wait for the card (≤ 10 s, cancelled by a stop signal) plus 3 s for the
     driver, then a full apply that keeps reset groups. A failed re-apply is retried with
     backoff (2 s to 2 min). After 5 successful power-loss recoveries within a minute
     (e.g. repeated GPU hangs), paused until the config changes or a reload.
  4. Retry failed groups with `RetryFailed` (backoff 10 s to 2 min; not while a recovery
     is pending).
  5. Config watch: on a change, apply the changed groups; invalid files are logged and
     ignored.
- **Signals:** SIGHUP (`systemctl reload`) re-applies everything including the exporter.
  SIGINT/SIGTERM stop the sampler, the socket (in-flight requests finish), then close the
  device. Settings stay on the card.
- **Socket API** (HTTP on the unix socket, mode 0600):

  | Request | Response |
  |---|---|
  | `GET /v1/status` | status JSON |
  | `GET /v1/sensors?window=SECONDS` | sensors over the window (default 1, clamped to 0.5–10); 503 if the device is closed |
  | `POST /v1/reset?group=all\|power\|temperature\|overclock` | result, or error with 400 (bad group) / 500 |

- **Exporter:** a sampler takes a snapshot every `interval` (≤ 60 s, so 32-bit counters
  wrap at most once), derives rates and reads the status (about 4 mailbox calls per
  sample); `/metrics` serves the latest sample. Totals survive exporter restarts. Skipped
  intervals: a suspend (the sampler's own check), a detected power loss, the interval after
  an unavailable sample, implausible values. While the card reads all ones only `up`,
  `sample_age_seconds` and the recovery metrics are published. A listener error stops
  the exporter until the next change or reload.

## Command line

`status`, `sensors` and `reset` use the socket when it answers, otherwise the card directly
(device from `/etc/a310ctl/config.yaml`; a warning if that file is invalid). The first
Ctrl+C / SIGTERM stops `sensors watch` at the next sample (exit 130 / 143); other commands
finish. A second signal kills the process. An unavailable card (all ones) prints "card not
responding" instead of values.

## Telemetry

- Rates come from two snapshots: power from energy, load from busy ticks over wall-clock
  time (the GT timestamp stops in RC6), bandwidth from the byte counters, RPM from the
  tach.
- VRAM used is summed from DRM `fdinfo` (`drm-resident-local0`/`vram0`); 0 without a driver.
- The PCIe link is read from the DG2 switch upstream port (`8086:4fa0/4fa1`); the GPU
  itself reports a virtual 2.5 GT/s ×1.
- Readable but unused: GT core status `0x138060` `[2:0]` (3 = RC6), RC6 residency
  `0x138108`, total VRAM `0x4900` `[14:8]` GB (when awake).

## Testing

`go vet ./...` and `go test ./...`. The tests cover the encoders with values verified on
the card, the factory constants, configuration parsing, the sleep detector, the recovery
pause, CLI output, and the controller (apply, reset, power-loss detection, reset marker)
against a fake device. Real hardware paths need the card and root.

## Toolchain and releases

- `go.mod` pins Go 1.26 (`toolchain go1.26.11`), the older supported Go release and so the
  longest-maintained. Move to the next version when 1.26 leaves support.
- `.github/workflows/release.yml` (on a pushed `v*` tag): gofmt, vet and tests; static
  binaries for amd64 and arm64; `.deb` and `.rpm` via nFPM (`packaging/nfpm.yaml`, version
  = tag without `v`); a GitHub release with `SHA256SUMS`. Manual runs only build artifacts.
- Packages install `/usr/sbin/a310ctl`, the unit, `/etc/a310ctl/config.yaml` (kept on
  upgrade) and the docs. A fresh install enables and starts the service (the default
  configuration sets factory values) and warns if it did not start; an upgrade restarts
  it if running; removal disables it. A reinstall after `apt remove` looks like an upgrade
  to dpkg, so the service stays disabled. `make install` enables and (re)starts it, with
  `/usr/local/sbin` and `CONFDIR` written into the unit.

## Known limitations

- Factory values are constants for the Sparkle A310; other boards may differ.
- Tested with `i915` and `xe` (load tests on both; the reset and recovery tests on `i915`).
  Driver differences: [DRIVERS.md](DRIVERS.md).
- Kernel lockdown blocks BAR0 access.
- A card reset is not detected when the power group is omitted and only the fan curve or
  overclock is changed; system sleep is.
- A change made by the kernel driver (e.g. `power1_max`) shows as drift and is corrected
  only by the next file change, reload or recovery.

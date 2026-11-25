# a310ctl

Tune and monitor an **Intel Arc A310** graphics card on Linux: fan curve, power limits,
temperature limit, overclocking and undervolting, with an optional Prometheus exporter.

You describe the settings in one YAML file. The `a310ctl` service applies them at boot,
re-applies them when you edit the file, and restores them after the computer sleeps.

> [!WARNING]
> a310ctl changes the card's firmware settings through undocumented interfaces. It is not
> an Intel product and comes without any warranty. Read [Risks](#risks) first.

## Contents

- [Risks](#risks)
- [Requirements](#requirements)
- [Install](#install)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [Commands](#commands)
- [Restoring factory settings](#restoring-factory-settings)
- [Sleep and resume](#sleep-and-resume)
- [Prometheus exporter](#prometheus-exporter)
- [Recommended configuration](#recommended-configuration)
- [Test results](#test-results)
- [Troubleshooting](#troubleshooting)
- [Uninstall](#uninstall)
- [License](#license)

## Risks

- **Heat and power:** higher limits and a quiet fan curve make the card hotter and draw
  more power. The A310 is powered by the PCIe slot (75 W for the whole card); limits are
  capped at 75 W, but your board or power supply may tolerate less.
- **Crashes:** too little voltage (undervolting, overclocking) causes screen glitches, GPU
  hangs or a frozen system. A setting can pass a short test and still hang once the card
  is hot. A severe hang can leave the GPU unusable until the computer is restarted. Every
  card is different.
- **Warranty:** changing power, voltage or clock settings may void it.
- **Reverse-engineered:** the interfaces were worked out from the Windows driver and tested
  on one card (Sparkle A310). Other boards or firmware may behave differently.

Test new settings with a long game or benchmark (at least 15 minutes, so the card is fully
hot) and watch `dmesg` for GPU hangs. To undo
everything, run `sudo a310ctl reset` or power-cycle the computer: the card always starts
with factory settings.

## Requirements

- An Intel Arc A310 (`8086:56a6`).
- Linux with root access, with the `i915` or `xe` driver loaded (tested with both; see
  [i915 or xe](DRIVERS.md) for what each driver supports).
- Kernel lockdown (Secure Boot) off: it blocks direct hardware access.

## Install

Download a package from the releases page and check it against `SHA256SUMS`.

```sh
sudo apt install ./a310ctl_<version>_amd64.deb        # Debian / Ubuntu (amd64, arm64)
sudo dnf install ./a310ctl-<version>-1.x86_64.rpm     # Fedora / RHEL (x86_64, aarch64); openSUSE: zypper
```

The package installs `/usr/sbin/a310ctl`, the `a310ctl` service (enabled and started) and
`/etc/a310ctl/config.yaml`. The default file sets every setting to its factory value and
keeps the exporter off; your edits are kept on upgrade. After `apt remove` and a reinstall,
enable the service again with `sudo systemctl enable --now a310ctl`.

**From source** (Go 1.26 and `make`):

```sh
make
sudo make install     # binary in /usr/local/sbin, default config (kept if present), (re)starts the service
```

**Other distributions:** use the plain binary from the releases page as `/usr/sbin/a310ctl`
and install `packaging/a310ctl.service` and `packaging/config.example.yaml` (as
`/etc/a310ctl/config.yaml`) yourself.

## Quick start

1. Look at the card:

   ```sh
   sudo a310ctl status         # current settings and limits
   sudo a310ctl sensors watch  # live temperature, power, clock, fan (Ctrl+C to stop)
   ```

2. Edit `/etc/a310ctl/config.yaml`, for example a quieter fan:

   ```yaml
   temperature:
     fan_curve: "40:20 55:30 70:60 85:100"
   ```

3. Save. The service applies the file within about 2 seconds; check with
   `sudo a310ctl status` or `journalctl -u a310ctl`.

## Configuration

The file is `/etc/a310ctl/config.yaml`. Only what you write is changed: a setting you leave
out keeps its current value. Write `factory` instead of a value, or instead of a whole
group, to return to the factory setting.

Every group, with factory values (the raw voltage keys are listed further down):

```yaml
device: automatic
socket: /run/a310ctl.sock

exporter:
  enabled: false
  listen: 127.0.0.1:9310
  interval: 2

temperature:
  limit: 90
  fan_curve: factory

power:
  sustained: 31.25
  burst: 31.25
  window: 28

overclock:
  unlock: false
  # boost: 150
  # undervolt: 10
```

### General

| Key | Default | Description |
|---|---|---|
| `device` | `automatic` | The card: `automatic`, or a PCI address such as `0000:03:00.0`. Takes effect after a restart. |
| `socket` | `/run/a310ctl.sock` | Control socket for `status`, `sensors` and `reset`. Takes effect after a restart. |

### `exporter`

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Serve Prometheus metrics. |
| `listen` | `127.0.0.1:9310` | Address of `/metrics`; `0.0.0.0:9310` allows other hosts. |
| `interval` | `2` | Seconds between measurements (0.5 to 60). |

### `temperature`

| Key | Range | Factory | Description |
|---|---|---|---|
| `limit` | 60 to 95 °C | 90 | The GPU slows down to stay below this temperature. Above the limit the card drops to its minimum clock and applications nearly freeze, so keep it above the card's normal load temperature (80 or more). |
| `fan_curve` | see below | `factory` | Fan speed by temperature. |

Fan curve formats:

- **Points:** `"40:20 55:30 70:60 85:100"` is 20 % at 40 °C, 30 % at 55 °C and so on.
  2 to 10 points (even count), rising temperatures, speeds that never go down, ending with
  100 % at 95 °C or lower.
- **Flat:** a number such as `60` keeps the fan at 60 % (20 to 100).
- **`factory`:** the card's own fan control.

The card keeps the fan at about 3000 RPM or more.

### `power`

| Key | Range | Factory | Description |
|---|---|---|---|
| `sustained` | 10 to 75 W | 31.25 | Long-term power limit. |
| `burst` | 10 to 75 W | 31.25 | Short-term power limit. |
| `window` | 0.001 to 300 s | 28 | Averaging time of the sustained limit. |

The A310 ignores limits above about 31 W unless `overclock.unlock` is `true`. Lower limits
always work.

### `overclock`

| Key | Range | Factory | Description |
|---|---|---|---|
| `unlock` | `true` / `false` | `false` | Lifts the ~31 W cap so higher `power` limits apply. |
| `boost` | 0 to 300 MHz | 0 | Intel's "Performance Boost". The Linux driver keeps the clock at 2450 MHz or below, so this acts as an undervolt. |
| `undervolt` | 0 to 150 mV | 0 | Extra voltage reduction on top of `boost`. |

Instead of `boost`/`undervolt`, an exact point can be set:

| Key | Range | Description |
|---|---|---|
| `frequency` | up to 2750 MHz | Target clock. |
| `voltage` | 0 to 1120 mV | Target voltage (`0` = stock). |
| `voltage_offset` | −150 to +150 mV | Negative values undervolt. |
| `voltage_mode` | `adaptive` / `fixed` | `fixed` holds one voltage at all clocks. |

Voltage settings unlock overclocking automatically; they cannot be combined with
`unlock: false` or `unlock: factory`.

See [How the undervolt is calculated](BENCHMARKS.md#how-the-undervolt-is-calculated) for the offset
that `boost` and `undervolt` produce.

### Applying changes

- Save the file: the running service applies it within about 2 seconds. An invalid file is
  rejected and the current settings stay (see `journalctl -u a310ctl`).
- `sudo a310ctl check` validates the file.
- `sudo systemctl reload a310ctl` re-applies everything.

## Commands

| Command | What it does |
|---|---|
| `a310ctl serve [--config PATH]` | Runs the service (used by systemd). |
| `a310ctl check [--config PATH]` | Validates a configuration file. |
| `a310ctl status [--socket PATH]` | Current settings, limits, PCIe link, factory values. |
| `a310ctl sensors [watch] [--socket PATH]` | One reading of all sensors, or a live table. |
| `a310ctl reset [all\|power\|temperature\|overclock] [--socket PATH]` | Restores factory settings (default `all`). |
| `a310ctl version` | Prints the version. |

Commands that access the card need root (`check` and `version` do not). Without a running
service, `status`, `sensors` and `reset` use the card directly. Ctrl+C stops
`sensors watch`; the other commands finish first.

`sensors` shows GPU and memory temperature, power, GPU voltage and clock, GPU load, memory
bandwidth, usage and speed, fan speed, and what is limiting the GPU (`power`, `thermal`,
`current_voltage`, `utilization`).

## Restoring factory settings

- **For now:** `sudo a310ctl reset` (or `reset power`, `reset temperature`,
  `reset overclock`). It lasts until the file is edited or the service is reloaded or
  restarted.
- **Permanently:** set a key or group to `factory` in the file.
- **Everything:** power-cycle the computer.

A310 factory settings: 31.25 W sustained and burst, 28 s window, 90 °C, automatic fan, no
overclock.

## Sleep and resume

The card loses its settings when the computer sleeps or the card resets (for example
after a GPU hang). The service notices and re-applies the configuration within a few
seconds; groups reset with `a310ctl reset` stay at factory.

- A card reset without sleep is noticed only if the file has a `power` group. Keep it,
  even as `power: factory`, when you change other settings.
- With `power` at factory and other settings changed, the burst limit reads 31.375 W
  instead of 31.25 W. This one-step marker is how a reset is noticed; it has no effect
  while overclocking is locked.
- If the card resets five times within a minute, re-applying pauses until the file is
  edited or the service is reloaded.

## Prometheus exporter

```yaml
exporter:
  enabled: true
  listen: 0.0.0.0:9310   # reachable from the Prometheus host
```

```yaml
# prometheus.yml
scrape_configs:
  - job_name: a310
    static_configs:
      - targets: ["your-host:9310"]
```

Main metrics (prefix `a310_`):

| Metric | Meaning |
|---|---|
| `gpu_temperature_celsius`, `vram_temperature_celsius` | Temperatures |
| `power_watts`, `energy_joules_total` | Power and energy |
| `gpu_frequency_hertz{kind="actual"}`, `gpu_voltage_volts` | Clock and voltage |
| `engine_busy_ratio{engine}` | GPU load (0 to 1) |
| `vram_bandwidth_bytes_per_second{direction}`, `vram_used_bytes` | Memory traffic and usage |
| `fan_rpm`, `fan_ratio` | Fan speed |
| `throttle_active{reason}` | What is limiting the GPU |
| `power_limit_watts{limit}`, `temperature_limit_celsius` | Limits on the card |
| `overclock_unlocked`, `overclock_voltage_offset_volts` | Overclock state |
| `config_apply_success`, `config_drift{setting}` | Whether the configuration is applied |
| `recoveries_total{reason,result}` | Re-applies after sleep (`resume`) or a card reset (`power_loss`) |
| `recovery_paused` | 1 when automatic re-applying is paused |
| `power_reset_marker` | 1 while the burst limit carries the reset marker |

```promql
rate(a310_energy_joules_total[5m])            # average watts
max_over_time(a310_gpu_temperature_celsius[1h])
a310_config_drift == 1                        # a setting differs from the file
```

## Recommended configuration

The best result on the test card: the speed of maximum power, cooler, without thermal
throttling (see [BENCHMARKS.md](BENCHMARKS.md)).

```yaml
temperature:
  limit: 95
  fan_curve: "25:30 33:30 41:30 49:30 57:30 65:30 73:60 81:90 89:100 95:100"

power:
  sustained: 50
  burst: 50

overclock:
  unlock: true
  boost: 150
```

- `unlock` lifts the ~31 W cap: about 7 % more sustained performance.
- `boost: 150` acts as a 68 mV undervolt: the same speed with about 5 W less. Test it with a
  30-minute game or benchmark; on glitches or GPU hangs use `boost: 100` or remove it.
- Do not add `undervolt` on top: on the test card 20 mV more (88 mV in total) hung the GPU
  once it was hot.
- With the `xe` driver the same settings give about 5 % more FPS than with `i915`
  ([BENCHMARKS.md](BENCHMARKS.md)), but `xe` lacks hardware video encoding and compute on
  the A310 ([i915 or xe](DRIVERS.md)).
- The fan stays at 30 % up to 65 °C and reaches 100 % at 89 °C. Lower the upper points
  for a quieter card, at the cost of some throttling when hot.

## Test results

Benchmarks for every setting, with both drivers, stability limits and how the undervolt is
calculated: [BENCHMARKS.md](BENCHMARKS.md).

## Troubleshooting

| Problem | What to do |
|---|---|
| `no Intel Arc A310 (8086:56a6) found` | Check `lspci -nn \| grep 56a6`; set `device:` to the card's address. |
| `open BAR0: permission denied` | Run as root; kernel lockdown (Secure Boot) blocks access. |
| `card not responding` or `GPU registers read 0xFFFFFFFF` | The card is resetting, asleep or off; wait, or load the `i915` or `xe` driver. |
| `another a310ctl process ... is using the GPU` | The service or another command is running; for the service check `--socket`. |
| Power limit above 31 W has no effect | Set `overclock.unlock: true`. |
| Screen glitches or `GPU HANG` in `dmesg` | Lower `boost`/`undervolt`, or `sudo a310ctl reset overclock`. After `device wedged` in `dmesg`, restart the computer. |
| Applications nearly freeze under load | The temperature limit is too low; raise `temperature.limit`. |
| `config_drift` is 1 | Something else (e.g. the driver's `power1_max`) changed the card; `sudo systemctl reload a310ctl`. |

## Uninstall

```sh
sudo a310ctl reset          # optional: factory settings now
sudo apt remove a310ctl     # or dnf remove a310ctl, or make uninstall
sudo rm -r /etc/a310ctl     # optional: the configuration
```

Developers: see [INTERNALS.md](INTERNALS.md).

## License

a310ctl is free software: you can redistribute it and/or modify it under the terms of the
GNU General Public License as published by the Free Software Foundation, either version 3
of the License, or (at your option) any later version. It is distributed WITHOUT ANY
WARRANTY; see [LICENSE](LICENSE).

Intel and Arc are trademarks of Intel Corporation. This project is not affiliated with or
endorsed by Intel.

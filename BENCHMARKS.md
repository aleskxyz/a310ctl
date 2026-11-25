# a310ctl benchmarks

Sparkle A310 (`172f:4019`), Mesa 26.1, glmark2 `terrain` looped off-screen at 2560×1440.
The [README](README.md) explains the settings; [i915 or xe](DRIVERS.md) compares the
drivers.

## i915 and xe

5 minutes per row, each starting at 60 °C. Every row uses this configuration plus the
`overclock` keys in its columns:

```yaml
temperature:
  limit: 95
  fan_curve: "25:30 33:30 41:30 49:30 57:30 65:30 73:60 81:90 89:100 95:100"
power:
  sustained: 50
  burst: 50
```

**i915**

| `unlock` | `boost` (MHz) | `undervolt` (mV) | Offset (mV) | FPS avg | FPS first min | FPS last min | Power avg (W) | Power max (W) | Clock avg (MHz) | Max temp (°C) | Reached 95 °C (s) | Thermal throttling (%) |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| `false` | 0 | 0 | 0 | 202 | 203 | 200 | 31.1 | 31.2 | 2065 | 84 | — | 0 |
| `true` | 0 | 0 | 0 | 212 | 216 | 209 | 39.4 | 44.0 | 2321 | 95 | 86 | 71 |
| `true` | 75 | 0 | −33 | 213 | 216 | 210 | 38.4 | 41.7 | 2364 | 95 | 106 | 64 |
| **`true`** | **150** | **0** | **−68** | **215** | **216** | **213** | **37.9** | **39.5** | **2427** | **95** | **177** | **36** |
| `true` | 150 | 10 | −78 | 216 | 217 | 214 | 37.5 | 39.0 | 2436 | 95 | 203 | 28 |

**xe**

| `unlock` | `boost` (MHz) | `undervolt` (mV) | Offset (mV) | FPS avg | FPS first min | FPS last min | Power avg (W) | Power max (W) | Clock avg (MHz) | Max temp (°C) | Reached 95 °C (s) | Thermal throttling (%) |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| `false` | 0 | 0 | 0 | 212 | 213 | 211 | 31.1 | 31.2 | 2034 | 85 | — | 0 |
| `true` | 0 | 0 | 0 | 223 | 229 | 220 | 39.2 | 44.8 | 2290 | 95 | 76 | 76 |
| `true` | 75 | 0 | −33 | 225 | 229 | 222 | 38.8 | 42.5 | 2353 | 95 | 97 | 66 |
| **`true`** | **150** | **0** | **−68** | **227** | **229** | **224** | **37.7** | **40.0** | **2392** | **95** | **132** | **53** |
| `true` | 150 | 10 | −78 | 227 | 230 | 226 | 37.8 | 39.5 | 2422 | 95 | 168 | 40 |

Thermal throttling is the share of the run spent at the temperature limit. The [recommended configuration](README.md#recommended-configuration) is in bold.

- `xe` is about 5 % faster than `i915` with every setting, at the same power. For other
  differences between the drivers see [i915 or xe](DRIVERS.md).
- `unlock` adds about 5 %; it lifts the ~31 W cap, so the card reaches 95 °C within 1.5
  minutes.
- Each undervolt step delays the temperature limit and reduces throttling. The FPS gain is
  small (2 to 4 FPS) because the clock is already at its 2450 MHz maximum until the card
  gets hot.
- `undervolt: 10` on top of `boost: 150` adds at most 1 FPS and runs closer to the
  −88 mV setting that hung the GPU (see below).

## Stability and limits

- −68 mV (`boost: 150`) was stable in every test. −88 mV (`boost: 150`, `undervolt: 20`)
  passed short runs but hung the GPU twice after several minutes at 86 to 89 °C, once so
  badly that the computer had to be restarted. `boost: 200` with `undervolt: 20` hung at
  once. Yours may differ.
- Without `unlock` the card stays at about 31 W whatever `power` says.

## How the undervolt is calculated

`boost` uses Intel's voltage-frequency model, V(f) in volts for a clock f in MHz:

```
V(f) = 1.8684285714286e-7 · f² − 4.91815769230768e-4 · f
ΔV   = V(2450 + boost) − V(2450)
offset = −ΔV − undervolt
```

The card gets the target clock 2450 + `boost` MHz, the target voltage 910 mV + ΔV and the
offset. The Linux driver never runs above 2450 MHz, so only the offset has an effect: it
is the undervolt. ΔV is about 0.42 mV per MHz of `boost`.

| `boost` (MHz) | Offset (mV), `undervolt: 0` | `undervolt: 10` | `undervolt: 20` |
|---|---|---|---|
| 0 | 0 | −10 | −20 |
| 75 | −33 | −43 | −53 |
| 150 | −68 | −78 | −88 |
| 200 | −92 | −102 | −112 |
| 300 | −144 | rejected | rejected |

The offset may not exceed −150 mV. `voltage_offset: -68` gives the same undervolt as
`boost: 150` without the model. On the test card −78 mV was the deepest stable offset;
−88 mV hung when hot ([Stability and limits](#stability-and-limits)).

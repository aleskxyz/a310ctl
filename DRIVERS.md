# i915 or xe for the Arc A310

Linux has two kernel drivers for the Arc A310 (DG2, "Alchemist"): `i915`, the default, and
`xe`, Intel's newer driver. a310ctl works with both. This page compares them as of
November 2026; [BENCHMARKS.md](BENCHMARKS.md) has the benchmark results.

## Comparison

| Feature | `i915` | `xe` |
|---|---|---|
| Support for Alchemist | Default driver, officially supported | Not the default; needs `force_probe` to bind. Intel makes `xe` the default from Battlemage on |
| 3D (OpenGL, Vulkan through Mesa) | Yes | Yes, about 5 % faster in the a310ctl tests |
| Vulkan VM_BIND (sparse resources, DX12 games through vkd3d) | No | Yes |
| HuC firmware (media) | Yes | No on DG2; patches posted in August 2026, not merged |
| Hardware video encode with bitrate control (CBR, VBR, ICQ; AV1, HEVC) | Works | Fails without HuC |
| Hardware video decode | Works | Works |
| Compute (OpenCL, Level Zero, oneAPI) | Supported | Reported not detected on DG2 |
| `intel_gpu_top`, engine load in system monitors | Works | Newer interface; older reports of missing engines; KDE System Monitor reads only `i915` |
| hwmon sensors | Power limit (`power1_max`), energy, fan, voltage, GPU temperature | Power limit (`power2_max`), energy, voltage, GPU and VRAM temperature, fan (`fan2` always reads 0 on DG2) |
| Driver minimum clock | 300 MHz | 600 MHz |
| a310ctl | Fully tested: apply, reset, recovery, load, stability | Load tests, status and sensors tested; reset and recovery not tested |

Both drivers write the same power-limit register as a310ctl. A change through their hwmon
`power*_max` file shows as `config_drift` until the next reload.

## Which one to use

- **Games and 3D only:** `xe` gives about 5 % more FPS and VM_BIND.
- **Video encoding** (OBS, Jellyfin, Plex, ffmpeg with VA-API or QSV), **compute** (OpenCL,
  oneAPI, AI) or **GPU monitoring tools:** `i915`, at least until DG2 HuC support lands in
  `xe`.

To use `xe`, add `xe.force_probe=56a6 i915.force_probe=!56a6` to the kernel command line
and reboot; remove them to return to `i915`.

## Sources

- [compute-runtime issue #905: DG2 not detected with xe](https://github.com/intel/compute-runtime/issues/905)
- [media-driver issue #2046: HuC rate control fails on DG2 with xe](https://github.com/intel/media-driver/issues/2046)
- [Phoronix: Xe driver lacking HuC media support for DG2](https://www.phoronix.com/news/Intel-Xe-DG2-No-HuC)
- [Phoronix: patches to improve media acceleration on Alchemist](https://www.phoronix.com/news/Intel-HuC-DG2-Alchemist-Xe)
- [drm/xe: add DG2 HuC GSC support (v4 patch)](https://ratatoskr.run/intel-xe/2026/08/17355490/t)
- [Phoronix forums: Xe vs i915 on Linux 6.19 for Arc Alchemist](https://www.phoronix.com/forums/forum/linux-graphics-x-org-drivers/intel-linux/1602306-intel-xe-vs-i915-driver-performance-on-linux-6-19-for-arc-alchemist-gpus)
- [Kernel docs: xe hwmon sysfs ABI](https://kernel.org/doc/Documentation/ABI/testing/sysfs-driver-intel-xe-hwmon)
- [drm/xe/hwmon: document DG2 fan speed quirk](https://lkml.iu.edu/2606.0/04020.html)
- [drm/xe/pmu: PMU support for engine busyness](https://lists.freedesktop.org/archives/intel-xe/2024-December/062768.html)
- [Arch forum: intel_gpu_top "Failed to detect engines" with xe](https://bbs.archlinux.org/viewtopic.php?id=295732)
- [KDE bug 517334: System Monitor uses the i915 PMU path](https://bugs.kde.org/show_bug.cgi?id=517334)

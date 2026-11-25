// Package hardware gives raw access to an Intel Arc A310 (DG2) through PCI BAR0:
// 32-bit MMIO reads/writes and the PCODE (firmware) mailbox.
package hardware

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	VendorIntel   = 0x8086
	DeviceA310    = 0x56A6
	mapSize       = 0x400000 // 4 MiB: covers VRAM bandwidth counters at 0x36C4xx
	LockFile      = "/run/a310ctl.lock"
	pcodeMailbox  = 0x138124
	pcodeData0    = 0x138128
	pcodeData1    = 0x13812C
	pcodeReady    = 1 << 31
	pcodeErrorMax = 0xFF
)

// ErrBusy is returned when another a310ctl process holds the GPU lock.
var ErrBusy = errors.New("GPU is in use by another a310ctl process (is the daemon running?)")

// ErrUnavailable is returned when BAR0 reads as all ones (asleep or gone).
var ErrUnavailable = errors.New("GPU registers read 0xFFFFFFFF (device asleep or gone)")

// Register is the access surface used by the gpu package (mockable in tests).
type Register interface {
	Read32(offset uint32) uint32
	Write32(offset uint32, value uint32)
	PcodeRead(command, subcommand, parameter uint32) (uint32, uint32, error)
	PcodeWrite(command, subcommand, parameter, data0, data1 uint32) error
}

// Access is Register plus identity and Close. *Device implements it; tests use fakes.
type Access interface {
	Register
	Close()
	PCIAddress() string
	Sysfs() string
	Driver() string
	Subsystem() string
}

// Device is an opened A310.
type Device struct {
	Address string // PCI address, e.g. 0000:03:00.0
	SysPath string // /sys/bus/pci/devices/<address>

	memory      []byte
	lock        *os.File
	powerSaved  string
	mailboxLock sync.Mutex
}

func readHex(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(string(data)), "0x"), 16, 32)
}

// Find returns the first A310 for "automatic"/"", or validates an explicit address.
func Find(address string) (string, error) {
	if address != "" && address != "automatic" {
		base := filepath.Join("/sys/bus/pci/devices", address)
		vendor, err := readHex(filepath.Join(base, "vendor"))
		if err != nil {
			return "", fmt.Errorf("device %s: %w", address, err)
		}
		device, err := readHex(filepath.Join(base, "device"))
		if err != nil {
			return "", fmt.Errorf("device %s: %w", address, err)
		}
		// The registers are A310-specific: refuse any other device (e.g. the iGPU).
		if vendor != VendorIntel || device != DeviceA310 {
			return "", fmt.Errorf("device %s is %04x:%04x, not an Intel Arc A310 (8086:56a6)", address, vendor, device)
		}
		return address, nil
	}
	entries, err := os.ReadDir("/sys/bus/pci/devices")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		base := filepath.Join("/sys/bus/pci/devices", entry.Name())
		vendor, err1 := readHex(filepath.Join(base, "vendor"))
		device, err2 := readHex(filepath.Join(base, "device"))
		if err1 == nil && err2 == nil && vendor == VendorIntel && device == DeviceA310 {
			return entry.Name(), nil
		}
	}
	return "", errors.New("no Intel Arc A310 (8086:56a6) found")
}

// Open maps BAR0, takes the lock (ErrBusy if held) and keeps the GPU out of runtime suspend.
func Open(address string) (*Device, error) {
	address, err := Find(address)
	if err != nil {
		return nil, err
	}
	device := &Device{Address: address, SysPath: filepath.Join("/sys/bus/pci/devices", address)}

	device.lock, err = os.OpenFile(LockFile, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w (run as root)", LockFile, err)
	}
	if err := syscall.Flock(int(device.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		device.lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}

	device.holdPower()

	file, err := os.OpenFile(filepath.Join(device.SysPath, "resource0"), os.O_RDWR|os.O_SYNC, 0)
	if err != nil {
		device.Close()
		return nil, fmt.Errorf("open BAR0: %w (run as root)", err)
	}
	device.memory, err = syscall.Mmap(int(file.Fd()), 0, mapSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	file.Close()
	if err != nil {
		device.Close()
		return nil, fmt.Errorf("map BAR0: %w", err)
	}
	if device.Read32(pcodeMailbox) == 0xFFFFFFFF {
		device.Close()
		return nil, ErrUnavailable
	}
	return device, nil
}

func (device *Device) PCIAddress() string { return device.Address }
func (device *Device) Sysfs() string      { return device.SysPath }

// holdPower keeps the GPU out of runtime suspend while it is open.
func (device *Device) holdPower() {
	path := filepath.Join(device.SysPath, "power", "control")
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	device.powerSaved = strings.TrimSpace(string(data))
	if device.powerSaved != "on" {
		if os.WriteFile(path, []byte("on"), 0) == nil {
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// Close unmaps BAR0, restores runtime power management and releases the lock.
func (device *Device) Close() {
	if device.memory != nil {
		syscall.Munmap(device.memory)
		device.memory = nil
	}
	if device.powerSaved != "" && device.powerSaved != "on" {
		os.WriteFile(filepath.Join(device.SysPath, "power", "control"), []byte(device.powerSaved), 0)
	}
	if device.lock != nil {
		syscall.Flock(int(device.lock.Fd()), syscall.LOCK_UN)
		device.lock.Close()
		device.lock = nil
	}
}

// pointer returns the mapped register at offset.
func (device *Device) pointer(offset uint32) *uint32 {
	return (*uint32)(unsafe.Pointer(&device.memory[offset]))
}

// Read32 reads a 32-bit register.
func (device *Device) Read32(offset uint32) uint32 { return atomic.LoadUint32(device.pointer(offset)) }

// Write32 writes a 32-bit register.
func (device *Device) Write32(offset uint32, value uint32) {
	atomic.StoreUint32(device.pointer(offset), value)
}

// waitIdle waits for the mailbox READY bit to clear.
func (device *Device) waitIdle(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for device.Read32(pcodeMailbox)&pcodeReady != 0 {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Microsecond)
	}
	return true
}

// pcode runs one mailbox transaction and returns DATA0/DATA1.
func (device *Device) pcode(command, subcommand, parameter, data0, data1 uint32) (uint32, uint32, error) {
	device.mailboxLock.Lock()
	defer device.mailboxLock.Unlock()
	mailbox := pcodeReady | command | subcommand<<8 | parameter<<16
	if !device.waitIdle(time.Second) {
		return 0, 0, errors.New("PCODE mailbox busy")
	}
	atomic.StoreUint32(device.pointer(pcodeData0), data0)
	atomic.StoreUint32(device.pointer(pcodeData1), data1)
	atomic.StoreUint32(device.pointer(pcodeMailbox), mailbox)
	if !device.waitIdle(500 * time.Millisecond) {
		return 0, 0, fmt.Errorf("PCODE timeout (command 0x%x subcommand %d parameter %d)", command, subcommand, parameter)
	}
	if status := device.Read32(pcodeMailbox) & pcodeErrorMax; status != 0 {
		return 0, 0, fmt.Errorf("PCODE error 0x%02x (command 0x%x subcommand %d parameter %d)", status, command, subcommand, parameter)
	}
	return device.Read32(pcodeData0), device.Read32(pcodeData1), nil
}

// PcodeRead performs a firmware mailbox read and returns DATA0/DATA1.
func (device *Device) PcodeRead(command, subcommand, parameter uint32) (uint32, uint32, error) {
	return device.pcode(command, subcommand, parameter, 0, 0)
}

// PcodeWrite performs a firmware mailbox write.
func (device *Device) PcodeWrite(command, subcommand, parameter, data0, data1 uint32) error {
	_, _, err := device.pcode(command, subcommand, parameter, data0, data1)
	return err
}

// Driver returns the bound kernel driver name or "none".
func (device *Device) Driver() string {
	target, err := os.Readlink(filepath.Join(device.SysPath, "driver"))
	if err != nil {
		return "none"
	}
	return filepath.Base(target)
}

// Subsystem returns "vendor:device" of the PCI subsystem (board identity).
func (device *Device) Subsystem() string {
	vendor, _ := readHex(filepath.Join(device.SysPath, "subsystem_vendor"))
	sub, _ := readHex(filepath.Join(device.SysPath, "subsystem_device"))
	return fmt.Sprintf("%04x:%04x", vendor, sub)
}

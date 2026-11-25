package gpu

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"a310ctl/internal/hardware"
)

const (
	pcodeFanControl     = 0x7D // fan 0 (parameter 0)
	fanWriteTable       = 1    // data0 = point count (0 = factory control)
	fanWriteTableDual   = 2    // data0 = two points, point = temperature | percent<<8
	fanMaximumPoints    = 10
	FanFlatMinimumDuty  = 20
	flatLowTemperature  = 20
	flatHighTemperature = 105
)

// FanPoint is one curve point.
type FanPoint struct {
	Celsius int `json:"celsius"`
	Percent int `json:"percent"`
}

// ParseFanCurve parses "40:20 55:30 ..." (spaces or commas).
func ParseFanCurve(text string) ([]FanPoint, error) {
	fields := strings.FieldsFunc(text, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' })
	points := make([]FanPoint, 0, len(fields))
	for _, field := range fields {
		parts := strings.Split(field, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("bad fan point %q (use temperature:percent, e.g. 60:50)", field)
		}
		celsius, err1 := strconv.Atoi(parts[0])
		percent, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("bad fan point %q", field)
		}
		points = append(points, FanPoint{celsius, percent})
	}
	return points, ValidateFanCurve(points)
}

// ValidateFanCurve enforces the firmware and safety rules for a point curve.
func ValidateFanCurve(points []FanPoint) error {
	if len(points) < 2 || len(points) > fanMaximumPoints || len(points)%2 != 0 {
		return errors.New("fan curve needs an even number of points, 2..10")
	}
	for index, point := range points {
		if point.Celsius < 20 || point.Celsius > 105 {
			return errors.New("fan curve temperatures must be 20..105")
		}
		if point.Percent < 0 || point.Percent > 100 {
			return errors.New("fan curve percent must be 0..100")
		}
		if index > 0 && point.Celsius <= points[index-1].Celsius {
			return errors.New("fan curve temperatures must strictly increase")
		}
		if index > 0 && point.Percent < points[index-1].Percent {
			return errors.New("fan curve percent must not decrease")
		}
	}
	last := points[len(points)-1]
	if last.Percent != 100 || last.Celsius > 95 {
		return errors.New("fan curve last point must be 100 percent at <= 95 celsius")
	}
	return nil
}

// FlatFanCurve returns the two-point table for a constant duty.
func FlatFanCurve(percent int) ([]FanPoint, error) {
	if percent < FanFlatMinimumDuty || percent > 100 {
		return nil, fmt.Errorf("flat fan duty %d outside %d..100", percent, FanFlatMinimumDuty)
	}
	return []FanPoint{{flatLowTemperature, percent}, {flatHighTemperature, percent}}, nil
}

func encodePoint(point FanPoint) uint32 {
	return uint32(point.Celsius&0xFF) | uint32(point.Percent&0xFF)<<8
}

// EncodeFanPair packs two points into one mailbox dword.
func EncodeFanPair(first, second FanPoint) uint32 {
	return encodePoint(first) | encodePoint(second)<<16
}

// WriteFanCurve programs the firmware fan table (count, then point pairs).
func WriteFanCurve(register hardware.Register, points []FanPoint) error {
	if err := register.PcodeWrite(pcodeFanControl, fanWriteTable, 0, uint32(len(points)), 0); err != nil {
		return err
	}
	for index := 0; index < len(points); index += 2 {
		if err := register.PcodeWrite(pcodeFanControl, fanWriteTableDual, 0, EncodeFanPair(points[index], points[index+1]), 0); err != nil {
			return err
		}
	}
	return nil
}

// FactoryFan returns the fan to firmware (factory) control.
func FactoryFan(register hardware.Register) error {
	return register.PcodeWrite(pcodeFanControl, fanWriteTable, 0, 0, 0)
}

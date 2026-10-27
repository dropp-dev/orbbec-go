package orbbec

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// FrameMetadata 是每帧 108 字节元数据里的常用字段。
type FrameMetadata struct {
	FrameCounter      uint32
	SensorTimestampUs int64 // 曝光起始时间
	Exposure          uint32
	DigitalGain       uint16
	AnalogGain        uint16
	AutoExposure      bool

	// 仅深度帧
	Width, Height   uint16
	GainLevel       uint16
	LaserPower      uint16
	LaserPowerLevel uint8
	LaserStatus     uint8
	ActualFPS       uint8
}

var metadataMagic = []byte("ORBBEC")

// ParseFrameMetadata 解析帧元数据，depth 为 true 时按深度帧布局解析。
func ParseFrameMetadata(b []byte, depth bool) (FrameMetadata, error) {
	var m FrameMetadata
	if len(b) < 108 || !bytes.HasPrefix(b, metadataMagic) {
		return m, errors.New("not a frame metadata block")
	}
	u32 := func(o int) uint32 { return binary.LittleEndian.Uint32(b[o:]) }
	u16 := func(o int) uint16 { return binary.LittleEndian.Uint16(b[o:]) }
	m.FrameCounter = u32(12)
	m.SensorTimestampUs = int64(u32(24))*1_000_000 + int64(u32(28))/1000 - int64(u32(68))
	m.Exposure = u32(32)
	m.DigitalGain, m.AnalogGain = u16(60), u16(62)
	m.AutoExposure = u32(64)>>10&1 == 1
	if depth {
		m.Height, m.Width = u16(72), u16(74)
		m.GainLevel = u16(76)
		m.LaserPower = u16(80)
		m.LaserPowerLevel = b[82]
		m.LaserStatus = b[94]
		m.ActualFPS = b[96]
	}
	return m, nil
}

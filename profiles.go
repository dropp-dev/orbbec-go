package orbbec

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// 传感器类型和像素格式（OBSensorType / OBFormat）。
const (
	SensorIR         = 1
	SensorColor      = 2
	SensorDepth      = 3
	SensorIRLeft     = 6
	SensorIRRight    = 7
	SensorConfidence = 9

	FormatYUYV  = 0
	FormatMJPG  = 5
	FormatY16   = 8
	FormatY8    = 9
	FormatI420  = 15
	FormatRVL   = 27
	FormatY12C4 = 34
)

var sensorNames = map[uint32]string{SensorIR: "ir", SensorColor: "color", SensorDepth: "depth", SensorIRLeft: "ir_left",
	SensorIRRight: "ir_right", SensorConfidence: "confidence"}
var formatNames = map[uint32]string{FormatYUYV: "YUYV", FormatMJPG: "MJPG", FormatY16: "Y16", FormatY8: "Y8", FormatI420: "I420",
	FormatRVL: "RVL", FormatY12C4: "Y12C4"}

func enumName(m map[uint32]string, v uint32) string {
	if s, ok := m[v]; ok {
		return s
	}
	return fmt.Sprint(v)
}

// EffectiveProfile 是相机报告的一个可用流配置。
type EffectiveProfile struct {
	Sensor, Format        uint32
	Width, Height, MaxFPS int
}

func (p EffectiveProfile) String() string {
	return fmt.Sprintf("%s %s %dx%d@%d", enumName(sensorNames, p.Sensor), enumName(formatNames, p.Format), p.Width, p.Height, p.MaxFPS)
}

func parseEffectiveProfiles(b []byte) []EffectiveProfile {
	var out []EffectiveProfile
	for off := 0; off+20 <= len(b); off += 20 {
		u := func(i int) uint32 { return binary.LittleEndian.Uint32(b[off+i*4:]) }
		out = append(out, EffectiveProfile{Sensor: u(0), Format: u(1), Width: int(u(2)), Height: int(u(3)), MaxFPS: int(u(4))})
	}
	return out
}

// checkProfile 检查配置是否可用：传感器、格式、宽高一致且帧率不超上限。列表为空时不检查。
func checkProfile(list []EffectiveProfile, sensor, format uint32, cfg StreamConfig) error {
	if len(list) == 0 {
		return nil
	}
	var avail []string
	for _, p := range list {
		if p.Sensor != sensor || p.Format != format {
			continue
		}
		if p.Width == cfg.Width && p.Height == cfg.Height && cfg.FPS <= p.MaxFPS {
			return nil
		}
		avail = append(avail, fmt.Sprintf("%dx%d@%d", p.Width, p.Height, p.MaxFPS))
	}
	return fmt.Errorf("unsupported %s %s profile %dx%d@%d, available: %s", enumName(sensorNames, sensor), enumName(formatNames, format),
		cfg.Width, cfg.Height, cfg.FPS, strings.Join(avail, ", "))
}

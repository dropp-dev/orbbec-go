package orbbec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// 以下结构体都是 1 字节对齐、小端。

// VersionInfo 是设备版本信息（OBVersionInfo）。
type VersionInfo struct {
	Firmware, Hardware, MinSDK, DepthChip, SystemChip, Serial string
	DeviceType                                                int32
	DeviceName, SubSystem                                     string
}

func parseVersionInfo(b []byte) (VersionInfo, error) {
	if len(b) < 132 {
		return VersionInfo{}, fmt.Errorf("short version info: %d bytes", len(b))
	}
	s := func(i int) string { return cString(b[i*16 : i*16+16]) }
	return VersionInfo{
		Firmware: s(0), Hardware: s(1), MinSDK: s(2), DepthChip: s(3), SystemChip: s(4), Serial: s(5),
		DeviceType: int32(binary.LittleEndian.Uint32(b[96:])),
		DeviceName: cString(b[100:116]),
		SubSystem:  cString(b[116:132]),
	}, nil
}

// cameraIntrinsic 是 OBCameraIntrinsic（20 字节）。
type cameraIntrinsic struct {
	fx, fy, cx, cy float32
	width, height  int16
}

func (ci cameraIntrinsic) toIntrinsics() Intrinsics {
	return Intrinsics{Width: int(ci.width), Height: int(ci.height),
		Fx: float64(ci.fx), Fy: float64(ci.fy), Cx: float64(ci.cx), Cy: float64(ci.cy)}
}

func f32(b []byte, off int) float32 { return math.Float32frombits(binary.LittleEndian.Uint32(b[off:])) }

func parseIntrinsic(b []byte) cameraIntrinsic {
	return cameraIntrinsic{fx: f32(b, 0), fy: f32(b, 4), cx: f32(b, 8), cy: f32(b, 12),
		width: int16(binary.LittleEndian.Uint16(b[16:])), height: int16(binary.LittleEndian.Uint16(b[18:]))}
}

// DepthCalibParam 是深度标定参数（OBDepthCalibrationParam，28 字节）。
type DepthCalibParam struct {
	DepthMode   uint32
	Baseline    float32
	Z0          float32
	FocalPix    float32
	Unit        float32
	DispOffset  float32
	InvalidDisp int32
}

func parseDepthCalibList(b []byte) []DepthCalibParam {
	var out []DepthCalibParam
	for off := 0; off+28 <= len(b); off += 28 {
		p := b[off:]
		out = append(out, DepthCalibParam{
			DepthMode: binary.LittleEndian.Uint32(p), Baseline: f32(p, 4), Z0: f32(p, 8), FocalPix: f32(p, 12),
			Unit: f32(p, 16), DispOffset: f32(p, 20), InvalidDisp: int32(binary.LittleEndian.Uint32(p[24:])),
		})
	}
	return out
}

// alignCalibParam 是 OBCameraParam_Internal_V0（152 字节）：深度内参、彩色内参、两组畸变、深度到彩色外参。
type alignCalibParam struct {
	depth, rgb cameraIntrinsic
	transform  Extrinsic
}

const alignCalibParamSize = 152

func parseAlignCalibList(b []byte) []alignCalibParam {
	var out []alignCalibParam
	for off := 0; off+alignCalibParamSize <= len(b); off += alignCalibParamSize {
		p := b[off:]
		a := alignCalibParam{depth: parseIntrinsic(p[0:]), rgb: parseIntrinsic(p[20:])}
		t := p[104:]
		for i := range 9 {
			a.transform.R[i] = float64(f32(t, i*4))
		}
		for i := range 3 {
			a.transform.T[i] = float64(f32(t, 36+i*4))
		}
		out = append(out, a)
	}
	return out
}

// d2cProfile 是 OBD2CProfile（22 字节）：分辨率组合对应的标定参数下标和对齐后的缩放、裁边。
type d2cProfile struct {
	colorW, colorH, depthW, depthH uint16
	alignType                      uint8
	paramIndex                     uint8
	depthScale                     float32
	alignLeft, alignTop            int16
	alignRight, alignBottom        int16
}

const (
	alignD2CSW       = 2
	alignD2CHWSWBoth = 3
)

func parseD2CProfileList(b []byte) []d2cProfile {
	var out []d2cProfile
	for off := 0; off+22 <= len(b); off += 22 {
		p := b[off:]
		i16 := func(o int) int16 { return int16(binary.LittleEndian.Uint16(p[o:])) }
		out = append(out, d2cProfile{
			colorW: binary.LittleEndian.Uint16(p[0:]), colorH: binary.LittleEndian.Uint16(p[2:]),
			depthW: binary.LittleEndian.Uint16(p[4:]), depthH: binary.LittleEndian.Uint16(p[6:]),
			alignType: p[8], paramIndex: p[9], depthScale: f32(p, 10),
			alignLeft: i16(14), alignTop: i16(16), alignRight: i16(18), alignBottom: i16(20),
		})
	}
	return out
}

// Calibration 是从相机读出的标定数据。
type Calibration struct {
	Depth []DepthCalibParam
	align []alignCalibParam
	d2c   []d2cProfile
}

// BaselineMM 返回双目基线（mm），未知时为 0。
func (c *Calibration) BaselineMM() float64 {
	if len(c.Depth) == 0 {
		return 0
	}
	return float64(c.Depth[0].Baseline) * float64(c.Depth[0].Unit)
}

// DepthToColor 返回深度到彩色的外参。
func (c *Calibration) DepthToColor() (Extrinsic, bool) {
	if len(c.align) == 0 {
		return Extrinsic{}, false
	}
	return c.align[0].transform, true
}

// DepthIntrinsics 返回 w×h 深度流的内参。
func (c *Calibration) DepthIntrinsics(w, h int) (Intrinsics, error) {
	return c.intrinsics(false, w, h)
}

// ColorIntrinsics 返回 w×h 彩色流的内参。
func (c *Calibration) ColorIntrinsics(w, h int) (Intrinsics, error) {
	return c.intrinsics(true, w, h)
}

// findD2CProfile 和 SDK 的匹配顺序一致：软件对齐条目里先找同分辨率再找同宽高比，然后在全部条目里重复。
func (c *Calibration) findD2CProfile(color bool, w, h int) (d2cProfile, bool) {
	size := func(p d2cProfile) (int, int) {
		if color {
			return int(p.colorW), int(p.colorH)
		}
		return int(p.depthW), int(p.depthH)
	}
	ratio := float32(w) / float32(h)
	for _, swOnly := range []bool{true, false} {
		for _, sameRatio := range []bool{false, true} {
			for _, p := range c.d2c {
				if swOnly && p.alignType != alignD2CSW && p.alignType != alignD2CHWSWBoth {
					continue
				}
				pw, ph := size(p)
				if ph == 0 {
					continue
				}
				if (!sameRatio && pw == w && ph == h) || (sameRatio && float32(pw)/float32(ph) == ratio) {
					return p, true
				}
			}
		}
	}
	return d2cProfile{}, false
}

// intrinsics 把标定内参换算到 w×h（同 SDK 的 bindIntrinsic）：先缩放到条目的深度分辨率，
// 彩色再按对齐缩放和裁边换算，最后按宽度缩放到实际分辨率。
func (c *Calibration) intrinsics(color bool, w, h int) (Intrinsics, error) {
	if len(c.align) == 0 {
		return Intrinsics{}, errors.New("no camera calibration available")
	}
	p, ok := c.findD2CProfile(color, w, h)
	if !ok {
		ci := c.align[0].depth
		if color {
			ci = c.align[0].rgb
		}
		if ci.width == 0 || ci.height == 0 || int(ci.height)*w != int(ci.width)*h {
			return Intrinsics{}, fmt.Errorf("no calibration for %dx%d (calibrated %dx%d)", w, h, ci.width, ci.height)
		}
		return scaleIntrinsic(ci, w), nil
	}
	if int(p.paramIndex) >= len(c.align) {
		return Intrinsics{}, fmt.Errorf("calibration index %d out of range (%d entries)", p.paramIndex, len(c.align))
	}
	param := c.align[p.paramIndex]
	r := float32(p.depthW) / float32(param.depth.width)
	scale := func(ci cameraIntrinsic) cameraIntrinsic {
		return cameraIntrinsic{fx: ci.fx * r, fy: ci.fy * r, cx: ci.cx * r, cy: ci.cy * r,
			width: int16(float32(ci.width) * r), height: int16(float32(ci.height) * r)}
	}
	ci := scale(param.depth)
	if color {
		ci = scale(param.rgb)
		s := p.depthScale
		ci = cameraIntrinsic{
			fx: s * ci.fx, fy: s * ci.fy,
			cx:     s*ci.cx + float32(p.alignLeft),
			cy:     s*ci.cy + float32(p.alignTop),
			width:  int16(s*float32(ci.width) + float32(p.alignLeft) + float32(p.alignRight)),
			height: int16(s*float32(ci.height) + float32(p.alignTop) + float32(p.alignBottom)),
		}
	}
	return scaleIntrinsic(ci, w), nil
}

func scaleIntrinsic(ci cameraIntrinsic, w int) Intrinsics {
	if ci.width == 0 {
		return ci.toIntrinsics()
	}
	r := float32(w) / float32(ci.width)
	return cameraIntrinsic{fx: ci.fx * r, fy: ci.fy * r, cx: ci.cx * r, cy: ci.cy * r,
		width: int16(w), height: int16(float32(ci.height) * r)}.toIntrinsics()
}

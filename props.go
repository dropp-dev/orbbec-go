package orbbec

import "fmt"

// 属性 ID（OrbbecSDK 的 Property.h / InternalProperty.hpp）。
const (
	PropLaserBool                     uint32 = 3
	PropStopDepthStreamBool           uint32 = 38
	PropDepthAlignHardwareBool        uint32 = 42 // 硬件 D2C，开着时深度被对齐到彩色视角
	PropDepthPrecisionLevelInt        uint32 = 75
	PropStopColorStreamBool           uint32 = 77
	PropDisparityToDepthBool          uint32 = 85
	PropDeviceCommunicationTypeInt    uint32 = 97 // 1 = 网口
	PropDevicePIDInt                  uint32 = 111
	PropCompatVersionInt              uint32 = 237 // 1 = 彩色 JPEG 内嵌元数据
	PropColorAutoExposureBool         uint32 = 2000
	PropIRAutoExposureBool            uint32 = 2025
	StructVersion                     uint32 = 1000
	RawDataD2CAlignSupportProfileList uint32 = 4024
	RawDataDepthCalibParam            uint32 = 4026
	RawDataAlignCalibParam            uint32 = 4027
	RawDataEffectiveStreamProfileList uint32 = 4035
)

const (
	commTypeNet   = 1
	compatVersion = 1
)

// DepthPrecision 是深度精度档位，决定一个深度值代表多少 mm。
type DepthPrecision int32

const (
	Precision1mm   DepthPrecision = 0
	Precision0mm8  DepthPrecision = 1
	Precision0mm4  DepthPrecision = 2
	Precision0mm1  DepthPrecision = 3
	Precision0mm2  DepthPrecision = 4
	Precision0mm5  DepthPrecision = 5
	Precision0mm05 DepthPrecision = 6

	// PrecisionKeep 表示不改相机当前档位。
	PrecisionKeep DepthPrecision = -1
)

// Unit 返回该档位的深度单位（mm）。
func (p DepthPrecision) Unit() (float32, error) {
	switch p {
	case Precision1mm:
		return 1.0, nil
	case Precision0mm8:
		return 0.8, nil
	case Precision0mm4:
		return 0.4, nil
	case Precision0mm1:
		return 0.1, nil
	case Precision0mm2:
		return 0.2, nil
	case Precision0mm5:
		return 0.5, nil
	case Precision0mm05:
		return 0.05, nil
	}
	return 0, fmt.Errorf("unknown depth precision level %d", int32(p))
}

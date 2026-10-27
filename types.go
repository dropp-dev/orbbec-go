package orbbec

import "time"

// Intrinsics 是针孔相机内参。
type Intrinsics struct {
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Fx     float64 `json:"fx"`
	Fy     float64 `json:"fy"`
	Cx     float64 `json:"cx"`
	Cy     float64 `json:"cy"`
}

// Extrinsic 是刚体变换 p_dst = R·p_src + T，R 按行存储，T 单位 mm。
type Extrinsic struct {
	R [9]float64 `json:"R"`
	T [3]float64 `json:"t"`
}

// ColorParams 是彩色相机内参和深度到彩色的外参。
type ColorParams struct {
	Intrinsics Intrinsics `json:"intr"`
	DepthToRGB Extrinsic  `json:"extrinsic"`
}

// DeviceInfo 是设备信息。
type DeviceInfo struct {
	Name       string `json:"name"`
	Serial     string `json:"serial"`
	Firmware   string `json:"firmware"`
	Hardware   string `json:"hardware,omitempty"`
	Connection string `json:"connection"`
	PID        uint16 `json:"pid,omitempty"`
	IP         string `json:"ip,omitempty"`
	MAC        string `json:"mac,omitempty"`
}

// DepthFrame 是一帧深度图，Data 按行存储，单位 mm，0 表示无效。
type DepthFrame struct {
	Width, Height     int
	Data              []uint16
	Seq               uint64 // 本地序号，从 1 开始
	DeviceSeq         uint64 // 相机帧号
	DeviceTimestampUs uint64
	Time              time.Time // 主机收到的时间
	Metadata          []byte    // 原始帧元数据，用 ParseFrameMetadata 解析
}

// At 返回 (u, v) 处的深度。
func (f *DepthFrame) At(u, v int) uint16 { return f.Data[v*f.Width+u] }

// Clone 深拷贝一帧（Data 和 Metadata 都复制）。
func (f *DepthFrame) Clone() *DepthFrame {
	c := *f
	c.Data = append([]uint16(nil), f.Data...)
	c.Metadata = append([]byte(nil), f.Metadata...)
	return &c
}

// ColorFrame 是一帧彩色图，保留相机发来的 JPEG 字节。
type ColorFrame struct {
	Width, Height int
	JPEG          []byte
	Seq           uint64
	Time          time.Time
	Metadata      []byte // JPEG COM 段里的帧元数据
}

// Clone 深拷贝一帧（JPEG 和 Metadata 都复制）。
func (f *ColorFrame) Clone() *ColorFrame {
	c := *f
	c.JPEG = append([]byte(nil), f.JPEG...)
	c.Metadata = append([]byte(nil), f.Metadata...)
	return &c
}

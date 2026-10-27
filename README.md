# orbbec-go

[![CI](https://github.com/dropp-dev/orbbec-go/actions/workflows/ci.yml/badge.svg)](https://github.com/dropp-dev/orbbec-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/dropp-dev/orbbec-go.svg)](https://pkg.go.dev/github.com/dropp-dev/orbbec-go)

**Pure Go driver for the Orbbec Gemini 435Le Ethernet depth camera** — no OrbbecSDK native libraries, no cgo.
`CGO_ENABLED=0` builds static binaries for Linux (amd64/arm64), Windows and macOS.

It speaks the camera's network protocols directly (GVCP control channel privilege, the vendor control protocol on
TCP 8090, RTSP/RTP depth and color streams, on-device calibration), ported from the MIT-licensed
[OrbbecSDK v2](https://github.com/orbbec/OrbbecSDK_v2) and checked against the official SDK on real hardware:
intrinsics, extrinsics, baseline, frame metadata and the command sequence match, and depth pixels match the SDK's
output with its noise filter off.

---

奥比中光 **Gemini 435Le 网口深度相机的纯 Go 驱动**：不依赖 OrbbecSDK 原生库，不用 cgo，一个静态二进制就能跑。

## 功能

- 设备发现（GVCP 广播）、控制权（CCP）申请 / 保活 / 释放：别的程序占着相机时给出明确错误，不会像原生 SDK 那样把进程带崩
- 深度流 Y16（1280×800 @ 5/10 fps），单位换算成 mm；彩色流 MJPEG（RFC 2435），保留原始 JPEG 字节方便直接转发
- 从相机读标定：各分辨率的深度/彩色内参、深度→彩色外参、双目基线；相机报告的有效分辨率列表
- 每帧元数据：曝光、增益、激光状态、传感器时间戳、帧号
- 常驻收帧 + 最近 N 帧缓存（多帧取中值不必等相机出帧）、掉线检测、打开重试
- `obprobe` 命令行：发现、只读查看、取帧存盘、长时间运行监控

## 安装

```bash
go get github.com/dropp-dev/orbbec-go
go install github.com/dropp-dev/orbbec-go/cmd/obprobe@latest
```

## 用法

```go
import "github.com/dropp-dev/orbbec-go"

cam := orbbec.New(orbbec.Config{IP: "192.168.1.10"})
if err := cam.Open(); err != nil { // 拿控制权、读标定、起深度+彩色流
	log.Fatal(err)
}
defer cam.Close() // 关流并释放控制权，别的程序马上能打开

intr := cam.Intrinsics()                                    // 1280x800 深度内参
frames, err := cam.RecentDepths(5, 2*time.Second, time.Second) // 最近 5 帧，Data 单位 mm，0 = 无效
f, err := cam.GrabDepth(time.Second)                        // 下一帧新的深度
color, _ := cam.GrabColor(time.Second)                      // 最新彩色帧（JPEG 字节）
cp := cam.ColorParams()                                     // 彩色内参 + 深度->彩色外参
if !cam.IsAlive(0) {                                        // 断电、拔网线后变 false
	log.Print(cam.LastError())
}
```

更底层的 `orbbec.OpenDevice` 可以只读打开（不占控制权）、读写属性（如 `PropLaserBool`）、单独起深度或彩色流。

## obprobe

```bash
obprobe discover                          # 广播找相机
obprobe info  -ip 192.168.1.10            # 只读：设备信息、有效分辨率、深度精度、标定和内参（不占控制权）
obprobe grab  -ip 192.168.1.10 -out /tmp  # 取 5 秒流，报帧率，存 depth.png（16 位，mm）和 color.jpg
obprobe watch -ip 192.168.1.10            # 长时间运行，每 10 秒报一次帧率，掉线自动重连
```

`-precision keep|default|0~6`：深度精度档位。`default` 和官方 SDK 一样设成相机默认档（435Le 是 1 mm），
`keep` 不动相机当前设置（obprobe 默认），库默认是 `default`。

## 支持的设备

| 设备 | 固件 | 状态 |
|---|---|---|
| Gemini 435Le | 1.3.19 | 已在真机上对照官方 SDK 核实 |

其他 Gemini 330 系列网口相机（335Le 等）走的是另一套 RTP 推流协议，暂不支持。

## 部署注意

- 持有控制权期间，OrbbecViewer 等基于官方 SDK 的程序打不开相机（ACCESS_DENIED）；`Close()` 后立即释放，进程被强杀时相机在心跳超时（约 6 s）后自动释放。
- 不要同时用 VLC、ffmpeg 等直接拉 `rtsp://相机IP:8554/...`：相机会把帧轮流分给各客户端，后来的客户端断开时还会让先来的停帧约 5 s。
- 防火墙放行到相机的 TCP 8090/8554/8888 和 UDP 3956。RTSP 走 TCP 交织，不需要调大 UDP 接收缓冲区。

## 协议概要

| 通道 | 端口 | 用途 |
|---|---|---|
| GVCP | UDP 3956 | 发现；CCP 寄存器 0x0A00 控制权；心跳超时 0x0938 |
| 控制协议 | TCP 8090 | 8 字节头（魔数 0x4D47）+ 属性 ID；响应魔数 0x4252，补零到 1022 字节 |
| RTSP | TCP 8554 / 8888 | `rtsp://IP:8554/depth/1280_800_10_y16`、`rtsp://IP:8888/color/1280_800_10_mjpg`，RTP 交织在同一 TCP 连接 |

## 与官方 SDK 的一致性

在 435Le（固件 1.3.19）上对照 OrbbecSDK v2.10.6 核实过：

- 内参、外参、基线、设备信息、帧时间戳和元数据与 SDK 完全一致；
- 打开、起流、关流的命令序列与 SDK 一致；
- 深度像素与关闭去噪的 SDK 输出一致（静止场景中值图 99% 以上像素相差不超过 1 mm，无像素错位）。SDK 默认开着的去噪只额外去掉极少量孤立小斑点，本驱动不做；
- 彩色 JPEG 的量化表、Huffman 表与 SDK 逐字节相同。

## 测试

```bash
go test ./...   # 内置模拟相机（GVCP + 控制协议 + RTSP），标定数据来自真机
```

## 许可证

[MIT](LICENSE)。协议实现移植自 OrbbecSDK v2（MIT，Copyright (c) 2024-2026 Orbbec Inc），见 [NOTICE](NOTICE)。
本项目与奥比中光无关，"Orbbec" 是奥比中光的商标。

// Package orbbec 是奥比中光 Gemini 435Le 网口深度相机的纯 Go 驱动，不依赖 OrbbecSDK 原生库，不用 cgo。
//
// 协议移植自 OrbbecSDK v2（MIT，Copyright (c) Orbbec Inc.）：
//
//   - GVCP（UDP 3956）：设备发现；CCP 寄存器 0x0A00 控制权的申请、保活和释放。
//   - 控制协议（TCP 8090）：属性、结构体、V1.1 结构体列表。
//   - RTSP（TCP 8554 深度、8888 彩色），RTP 交织在同一连接上。
//   - 标定：深度/彩色内参、深度到彩色外参、基线、有效分辨率。
//
// 用法：
//
//	cam := orbbec.New(orbbec.Config{IP: "192.168.1.10"})
//	if err := cam.Open(); err != nil {
//		log.Fatal(err)
//	}
//	defer cam.Close()
//	frames, err := cam.RecentDepths(5, 2*time.Second, time.Second)
package orbbec

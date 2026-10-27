package orbbec

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestFirmwareVersionInt(t *testing.T) {
	for in, want := range map[string]int{"1.3.19": 10319, "1.3.12": 10312, "V1.6.07": 10607, "1.3": 0, "": 0, "x": 0} {
		if got := firmwareVersionInt(in); got != want {
			t.Errorf("firmwareVersionInt(%q) = %d，期望 %d", in, got, want)
		}
	}
}

func TestDepthPrecisionUnit(t *testing.T) {
	u, err := Precision0mm2.Unit()
	if err != nil || u != 0.2 {
		t.Fatalf("0.2mm 档：%v %v", u, err)
	}
	if _, err := DepthPrecision(9).Unit(); err == nil {
		t.Fatal("未知档位应报错")
	}
}

func TestVendorPadding(t *testing.T) {
	for _, pad := range []int{1022, 0} {
		f := newFake435(t)
		f.padTo = pad
		o := f.options()
		vc, err := dialVendor(o.IP, o.ControlPort, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		// 连续多条命令：补零没读干净的话第二条就会错位
		for range 3 {
			r, err := vc.GetPropertyRange(PropDepthPrecisionLevelInt)
			if err != nil || r != (PropertyRange{Cur: 4, Max: 4, Min: 0, Def: 0, Step: 1}) {
				t.Fatalf("pad=%d 读属性：%+v %v", pad, r, err)
			}
		}
		if err := vc.SetProperty(PropDepthPrecisionLevelInt, 0); err != nil {
			t.Fatal(err)
		}
		if v, _ := vc.GetProperty(PropDepthPrecisionLevelInt); v != 0 {
			t.Fatalf("写后读回 %d", v)
		}
		b, err := vc.GetStructList(RawDataAlignCalibParam, 0)
		if err != nil || !bytes.Equal(b, fixtureAlignCalib()) {
			t.Fatalf("pad=%d 读结构体列表：%d 字节 %v", pad, len(b), err)
		}
		if _, err := vc.GetStructList(RawDataAlignCalibParam, 1); err == nil {
			t.Fatal("命令版本不符应报错")
		}
		_, err = vc.GetProperty(12345)
		var de *DeviceError
		if !errors.As(err, &de) || de.Code != 2 {
			t.Fatalf("不支持的属性应返回 DeviceError，得到 %v", err)
		}
		// 出错后连接仍然可用
		if _, err := vc.GetStructure(StructVersion); err != nil {
			t.Fatal(err)
		}
		vc.Close()
		f.Close()
	}
}

func TestParseVersionInfo(t *testing.T) {
	v, err := parseVersionInfo(fakeVersionInfo())
	if err != nil || v.Firmware != "1.3.19" || v.Serial != "FAKE435LE001" || v.DeviceName != "Orbbec Gemini435" || v.SubSystem != "1.0.9" {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestGVCPControl(t *testing.T) {
	f := newFake435(t)
	o := f.options()
	a, _ := dialGVCP(o.IP, o.GVCPPort)
	defer a.Close()
	d, err := a.Discover()
	if err != nil || d.Model != "Gemini 435Le" || d.Firmware != "1.3.19" || d.MAC != "54:14:FD:25:A4:44" || d.IP != "127.0.0.1" || d.PID != 0x0815 {
		t.Fatalf("发现：%+v %v", d, err)
	}
	ccp, err := acquireCCP(a, firmwareVersionInt(d.Firmware), d.Serial, quietLogger())
	if err != nil || ccp == nil {
		t.Fatalf("申请控制权：%v", err)
	}
	if f.ccpValue() != ccpControlAccess {
		t.Fatalf("CCP = %d", f.ccpValue())
	}
	b, _ := dialGVCP(o.IP, o.GVCPPort)
	defer b.Close()
	if _, err := acquireCCP(b, 10319, "", quietLogger()); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("第二个程序申请应被拒绝，得到 %v", err)
	}
	// 老固件不支持 CCP：直接放行
	if c, err := acquireCCP(b, 10300, "", quietLogger()); c != nil || err != nil {
		t.Fatalf("老固件应跳过 CCP：%v %v", c, err)
	}
	ccp.release()
	if f.ccpValue() != 0 {
		t.Fatalf("释放后 CCP = %d", f.ccpValue())
	}
}

func TestCCPKeepaliveReacquire(t *testing.T) {
	f := newFake435(t)
	o := f.options()
	g, _ := dialGVCP(o.IP, o.GVCPPort)
	defer g.Close()
	ccp, err := acquireCCP(g, 10319, "", slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	// 换成短周期的保活方便测试
	close(ccp.stop)
	<-ccp.done
	ccp.stop, ccp.done = make(chan struct{}), make(chan struct{})
	go ccp.keepalive(20 * time.Millisecond)
	defer ccp.release()

	f.clearCCP()
	waitFor(t, time.Second, func() bool { return f.ccpValue() == ccpControlAccess })
	if ccp.lost.Load() {
		t.Fatal("被相机清空后应能要回来")
	}
	// 被别人抢走
	other, _ := dialGVCP(o.IP, o.GVCPPort)
	defer other.Close()
	f.clearCCP()
	if st, _ := other.WriteRegister(regCCP, ccpControlAccess); st != GevStatusSuccess {
		t.Fatalf("其他程序抢控制权：%#x", st)
	}
	waitFor(t, time.Second, ccp.lost.Load)
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("等待超时")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestParseRTP(t *testing.T) {
	pkt := []byte{0xA1, 0x80 | 97, 0, 7, 0, 0, 0, 1, 0, 0, 0, 2, // V=2 P=1 CC=1
		0, 0, 0, 9, // CSRC
		'a', 'b', 'c', 0, 0, 2} // 负载 abc，2 字节填充... 最后一字节是填充长度
	p, err := parseRTP(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if !p.marker || p.seq != 7 || string(p.payload) != "abc\x00" {
		t.Fatalf("%+v %q", p, p.payload)
	}
	if _, err := parseRTP([]byte{0x40, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}); err == nil {
		t.Fatal("版本 1 应报错")
	}
}

func TestFrameAssembler(t *testing.T) {
	a := &frameAssembler{maxBytes: 100}
	pk := func(seq uint16, marker bool, s string) rtpPacket {
		return rtpPacket{seq: seq, marker: marker, payload: []byte(s)}
	}
	if a.push(pk(1, false, "ab")) != nil || string(a.push(pk(2, true, "cd"))) != "abcd" {
		t.Fatal("正常拼帧失败")
	}
	// 丢了序号 4：这一帧丢掉，下一帧正常
	a.push(pk(3, false, "xx"))
	if a.push(pk(5, true, "yy")) != nil || a.dropped != 1 {
		t.Fatal("丢包的帧应丢弃")
	}
	if string(a.push(pk(6, true, "ok"))) != "ok" {
		t.Fatal("丢包后下一帧应恢复")
	}
	// 超长丢弃
	a.push(pk(7, false, strings.Repeat("z", 90)))
	if a.push(pk(8, true, strings.Repeat("z", 20))) != nil {
		t.Fatal("超长帧应丢弃")
	}
	// 序号回绕
	a.push(pk(65535, false, "w"))
	if string(a.push(pk(0, true, "r"))) != "wr" {
		t.Fatal("序号回绕应连续")
	}
}

func TestVendorFrameAndY16(t *testing.T) {
	frame := make([]byte, vendorFrameHeaderSize+4+3*2*2)
	binary.LittleEndian.PutUint64(frame, 42)
	binary.LittleEndian.PutUint16(frame[8:], 4)
	binary.LittleEndian.PutUint64(frame[10:], 1234)
	pix := frame[vendorFrameHeaderSize+4:]
	for i, v := range []uint16{0, 5, 6400, 65535, 7, 9} {
		binary.LittleEndian.PutUint16(pix[i*2:], v)
	}
	h, md, data, err := splitVendorFrame(frame)
	if err != nil || h.FrameCounter != 42 || h.Timestamp != 1234 || len(md) != 4 || len(data) != 12 {
		t.Fatalf("%+v %d %d %v", h, len(md), len(data), err)
	}
	got, err := decodeY16(data, 3, 2, 1)
	if err != nil || got[2] != 6400 || got[3] != 65535 {
		t.Fatalf("单位 1：%v %v", got, err)
	}
	got, _ = decodeY16(data, 3, 2, 0.2)
	// 和 Python 的 (data*scale).astype(uint16) 一样截断：5*0.2=1、6400*0.2=1280、7*0.2=1
	if got[1] != 1 || got[2] != 1280 || got[4] != 1 || got[3] != uint16(math.Floor(65535*0.2)) {
		t.Fatalf("单位 0.2：%v", got)
	}
	if _, err := decodeY16(data, 4, 2, 1); err == nil {
		t.Fatal("数据不够应报错")
	}
	if _, _, _, err := splitVendorFrame(frame[:10]); err == nil {
		t.Fatal("帧太短应报错")
	}
}

func TestParseSDP(t *testing.T) {
	codec, ctl, err := parseSDP("v=0\r\na=control:*\r\nm=VIDEO 0 RTP/AVP 97\r\na=rtpmap:97 OB_FMT_Y16/90000\r\na=control:track1\r\n")
	if err != nil || codec != "OB_FMT_Y16" || ctl != "track1" {
		t.Fatalf("%q %q %v", codec, ctl, err)
	}
	codec, _, err = parseSDP("v=0\r\nm=video 0 RTP/AVP 26\r\na=control:track1\r\n")
	if err != nil || codec != "JPEG" {
		t.Fatalf("静态 JPEG：%q %v", codec, err)
	}
	if got := resolveControl("rtsp://h:8554/depth/x/", "track1"); got != "rtsp://h:8554/depth/x/track1" {
		t.Fatal(got)
	}
	if got := resolveControl("rtsp://h:8554/depth/x", "rtsp://o/t"); got != "rtsp://o/t" {
		t.Fatal(got)
	}
}

// TestRFC2435RoundTrip：标准库编的 JPEG 拆成 RFC 2435 包，再按 RFC 合成头，解出来的像素要和原图完全一样。
func TestRFC2435RoundTrip(t *testing.T) {
	for _, q := range []int{30, 50, 80, 95} {
		img := image.NewYCbCr(image.Rect(0, 0, 48, 32), image.YCbCrSubsampleRatio420)
		for i := range img.Y {
			img.Y[i] = uint8(i * 7)
		}
		for i := range img.Cb {
			img.Cb[i], img.Cr[i] = uint8(i*3), uint8(255-i*5)
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			t.Fatal(err)
		}
		want, _ := jpeg.Decode(bytes.NewReader(buf.Bytes()))

		jd := &jpegDepay{}
		a := &frameAssembler{maxBytes: 1 << 20, depay: jd.depay}
		var frame []byte
		pkts := rfc2435Packets(jpegScanData(t, buf.Bytes()), 1, uint8(q), 48, 32, 100)
		for i, pl := range pkts {
			frame = a.push(rtpPacket{seq: uint16(i), marker: i == len(pkts)-1, payload: pl})
		}
		if frame == nil {
			t.Fatalf("q=%d 没拼出帧", q)
		}
		got, err := jpeg.Decode(bytes.NewReader(jd.finish(frame)))
		if err != nil {
			t.Fatalf("q=%d 解码：%v", q, err)
		}
		for y := range 32 {
			for x := range 48 {
				if got.At(x, y) != want.At(x, y) {
					t.Fatalf("q=%d (%d,%d) 像素不同：%v vs %v", q, x, y, got.At(x, y), want.At(x, y))
				}
			}
		}
	}
	// 首包分片偏移不为 0（中途加入）要丢弃
	jd := &jpegDepay{}
	if _, err := jd.depay(rtpPacket{payload: []byte{0, 0, 0, 8, 1, 50, 6, 4, 1}}, true); err == nil {
		t.Fatal("首包偏移非 0 应报错")
	}
}

// TestEmbeddedJPEG：负载本身就是完整 JPEG（兼容版本 1 的彩色流）时直接用，缺 Huffman 表时补上。
func TestEmbeddedJPEG(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 16, 16))
	for i := range img.Pix {
		img.Pix[i] = uint8(i)
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	full := buf.Bytes()
	jd := &jpegDepay{}
	if !bytes.Equal(jd.finish(full), full) {
		t.Fatal("完整 JPEG 应原样返回")
	}
	// 去掉 DHT 段
	var stripped []byte
	i := 2
	stripped = append(stripped, full[:2]...)
	for i+4 <= len(full) {
		m := full[i+1]
		l := int(binary.BigEndian.Uint16(full[i+2:]))
		if m == 0xDA {
			stripped = append(stripped, full[i:]...)
			break
		}
		if m != 0xC4 {
			stripped = append(stripped, full[i:i+2+l]...)
		}
		i += 2 + l
	}
	if _, err := jpeg.Decode(bytes.NewReader(stripped)); err == nil {
		t.Fatal("去掉 DHT 后标准库应解不了（测试前提）")
	}
	fixed := jd.finish(stripped)
	got, err := jpeg.Decode(bytes.NewReader(fixed))
	if err != nil {
		t.Fatalf("补表后解码：%v", err)
	}
	want, _ := jpeg.Decode(bytes.NewReader(full))
	if grayAt(got, 5, 5) != grayAt(want, 5, 5) {
		t.Fatal("补表后像素不同")
	}
}

func TestCalibrationFixture(t *testing.T) {
	c := Calibration{Depth: parseDepthCalibList(fixtureDepthCalib()), align: parseAlignCalibList(fixtureAlignCalib()),
		d2c: parseD2CProfileList(fixtureD2CList())}
	if len(c.align) != 4 || len(c.d2c) != 8 {
		t.Fatalf("对齐标定 %d 组，D2C %d 条", len(c.align), len(c.d2c))
	}
	if b := c.BaselineMM(); math.Abs(b-95.231) > 1e-3 {
		t.Fatalf("基线 %v", b)
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-3 }
	di, _ := c.DepthIntrinsics(1280, 800)
	if di.Width != 1280 || di.Height != 800 || !near(di.Fx, 626.76514) || !near(di.Cx, 638.5) || !near(di.Cy, 405) {
		t.Fatalf("深度 1280x800：%+v", di)
	}
	ci, _ := c.ColorIntrinsics(1280, 800)
	if !near(ci.Fx, 611.02631) || !near(ci.Cy, 405.855) {
		t.Fatalf("彩色 1280x800：%+v", ci)
	}
	// 1280x720 彩色：上下各裁 40 行，cy 减 40
	ci, _ = c.ColorIntrinsics(1280, 720)
	if ci.Height != 720 || !near(ci.Cy, 405.855-40) || !near(ci.Fx, 611.02631) {
		t.Fatalf("彩色 1280x720：%+v", ci)
	}
	di, _ = c.DepthIntrinsics(640, 400)
	if di.Width != 640 || !near(di.Fx, 626.76514/2) {
		t.Fatalf("深度 640x400：%+v", di)
	}
	ext, ok := c.DepthToColor()
	if !ok || !near(ext.T[0], -23.795103) {
		t.Fatalf("外参：%+v", ext)
	}
}

func TestDeviceOpenAgainstFake(t *testing.T) {
	f := newFake435(t)
	o := f.options()
	o.Logger = quietLogger()
	d, err := OpenDevice(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	info := d.Info()
	if info.Serial != "FAKE435LE001" || info.Firmware != "1.3.19" || info.PID != 0x0815 || info.Name != "Orbbec Gemini 435Le" {
		t.Fatalf("%+v", info)
	}
	// 和 SDK 一样：通信方式=1、兼容版本=1、深度精度设成默认档 0、开视差转深度
	if p := f.prop(PropDeviceCommunicationTypeInt); p.Cur != 1 {
		t.Fatalf("通信方式 %d", p.Cur)
	}
	if p := f.prop(PropCompatVersionInt); p.Cur != 1 {
		t.Fatalf("兼容版本 %d", p.Cur)
	}
	if p := f.prop(PropDepthPrecisionLevelInt); p.Cur != 0 {
		t.Fatalf("深度精度 %d", p.Cur)
	}
	if d.DepthUnitMM() != 1 {
		t.Fatalf("深度单位 %v", d.DepthUnitMM())
	}
	if f.ccpValue() != ccpControlAccess {
		t.Fatal("应拿到控制权")
	}
	d.Close()
	if f.ccpValue() != 0 {
		t.Fatal("关闭后应释放控制权")
	}

	// 保持当前档位
	f.props[PropDepthPrecisionLevelInt] = PropertyRange{Cur: 4, Max: 4, Def: 0, Step: 1}
	keep := PrecisionKeep
	o.DepthPrecision = &keep
	d2, err := OpenDevice(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if f.prop(PropDepthPrecisionLevelInt).Cur != 4 || d2.DepthUnitMM() != 0.2 {
		t.Fatalf("keep：档位 %d 单位 %v", f.prop(PropDepthPrecisionLevelInt).Cur, d2.DepthUnitMM())
	}
	// 只读打开不写属性、不占控制权
	ro := f.options()
	ro.ReadOnly, ro.Logger = true, quietLogger()
	d3, err := OpenDevice(context.Background(), ro)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d3.StartDepth(context.Background(), StreamConfig{1280, 800, 10}, nil); err == nil {
		t.Fatal("只读模式不能取流")
	}
	d3.Close()
	if f.ccpValue() != ccpControlAccess {
		t.Fatal("只读设备关闭不应影响别人的控制权")
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func TestEffectiveProfiles(t *testing.T) {
	list := parseEffectiveProfiles(fixtureEffectiveProfiles())
	if len(list) != 16 {
		t.Fatalf("%d 条", len(list))
	}
	if err := checkProfile(list, SensorDepth, FormatY16, StreamConfig{1280, 800, 10}); err != nil {
		t.Fatal(err)
	}
	if err := checkProfile(list, SensorDepth, FormatY16, StreamConfig{1280, 800, 5}); err != nil {
		t.Fatal("低于上限的帧率应可用：", err)
	}
	err := checkProfile(list, SensorDepth, FormatY16, StreamConfig{640, 400, 10})
	if err == nil || !strings.Contains(err.Error(), "available: 1280x800@10") {
		t.Fatalf("640x400 深度应不支持并列出可用分辨率：%v", err)
	}
	if err := checkProfile(list, SensorDepth, FormatY16, StreamConfig{1280, 800, 15}); err == nil {
		t.Fatal("超过帧率上限应报错")
	}
	if err := checkProfile(list, SensorColor, FormatMJPG, StreamConfig{640, 360, 10}); err != nil {
		t.Fatal(err)
	}
	if err := checkProfile(nil, SensorDepth, FormatY16, StreamConfig{1, 1, 1}); err != nil {
		t.Fatal("列表为空时不拦")
	}

	f := newFake435(t)
	o := f.options()
	o.Logger = quietLogger()
	d, err := OpenDevice(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.StartDepth(context.Background(), StreamConfig{640, 400, 10}, nil); err == nil {
		t.Fatal("不支持的分辨率应在起流前就报错")
	}
}

func TestFrameMetadata(t *testing.T) {
	// 真机深度帧元数据（官方 SDK 对同一帧报：帧号 1、曝光 21207、增益 16、自动曝光 1、激光功率档 4、激光状态 1）
	md := make([]byte, 108)
	copy(md, "ORBBEC")
	binary.LittleEndian.PutUint32(md[8:], 108)
	binary.LittleEndian.PutUint32(md[12:], 1)
	binary.LittleEndian.PutUint32(md[24:], 1999)      // sof_sec
	binary.LittleEndian.PutUint32(md[28:], 592707000) // sof_nsec
	binary.LittleEndian.PutUint32(md[32:], 21207)     // exposure
	binary.LittleEndian.PutUint32(md[64:], 1<<10)     // auto_exposure 位
	binary.LittleEndian.PutUint16(md[76:], 16)        // gain_level
	md[82], md[94] = 4, 1
	m, err := ParseFrameMetadata(md, true)
	if err != nil || m.FrameCounter != 1 || m.Exposure != 21207 || m.GainLevel != 16 || !m.AutoExposure ||
		m.LaserPowerLevel != 4 || m.LaserStatus != 1 || m.SensorTimestampUs != 1999592707 {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := ParseFrameMetadata(md[:50], true); err == nil {
		t.Fatal("太短应报错")
	}

	// 彩色：元数据在 JPEG 的 COM 段里
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 8, 8)), nil)
	j := buf.Bytes()
	com := append([]byte{0xFF, 0xFE, 0, 110}, md...)
	withMeta := append(append(append([]byte{}, j[:2]...), com...), j[2:]...)
	if got := jpegMetadata(withMeta); !bytes.Equal(got, md) {
		t.Fatalf("COM 段元数据没取出来：%d 字节", len(got))
	}
	if jpegMetadata(j) != nil {
		t.Fatal("没有 COM 段应返回 nil")
	}
	if _, err := jpeg.Decode(bytes.NewReader(withMeta)); err != nil {
		t.Fatal("带 COM 段的 JPEG 应能直接解码：", err)
	}
}

package orbbec

import (
	"bytes"
	"errors"
	"image/jpeg"
	"strings"
	"testing"
	"time"
)

func fakeCamera(f *fake435, cfg Config) *Camera {
	o := f.options()
	cfg.IP, cfg.Logger = o.IP, quietLogger()
	cfg.GVCPPort, cfg.ControlPort, cfg.DepthRTSPPort, cfg.ColorRTSPPort = o.GVCPPort, o.ControlPort, o.DepthRTSPPort, o.ColorRTSPPort
	if cfg.RetryWait == 0 {
		cfg.RetryWait = 50 * time.Millisecond
	}
	return New(cfg)
}

func TestCameraEndToEnd(t *testing.T) {
	f := newFake435(t)
	cam := fakeCamera(f, Config{AliveTimeout: 300 * time.Millisecond})
	if err := cam.Open(); err != nil {
		t.Fatal(err)
	}
	defer cam.Close()

	intr := cam.Intrinsics()
	if intr.Width != 1280 || intr.Height != 800 || intr.Fx < 626 || intr.Fx > 627 {
		t.Fatalf("内参 %+v", intr)
	}
	if cp := cam.ColorParams(); cp == nil || cp.Intrinsics.Fx < 611 || cp.DepthToRGB.T[0] > -23 {
		t.Fatalf("彩色参数 %+v", cp)
	}
	if b := cam.BaselineMM(); b < 95 || b > 96 {
		t.Fatalf("基线 %v", b)
	}
	if !cam.IsAlive(0) || cam.LastError() != "" {
		t.Fatalf("应在线：%q", cam.LastError())
	}

	// GrabDepth 拿到的是下一帧新的；默认档位 1 mm，原始值 6400 -> 6400 mm，每行首像素无效
	a, err := cam.GrabDepth(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	b, err := cam.GrabDepth(time.Second)
	if err != nil || b.Seq <= a.Seq {
		t.Fatalf("第二帧序号 %d <= %d，err=%v", b.Seq, a.Seq, err)
	}
	if b.At(0, 0) != 0 || b.At(640, 400) != 6400 || b.Width != 1280 || b.Height != 800 {
		t.Fatalf("深度值 %d %d", b.At(0, 0), b.At(640, 400))
	}
	if b.DeviceTimestampUs == 0 || b.DeviceSeq == 0 {
		t.Fatalf("设备时间戳/帧号未填：%+v", b.DeviceTimestampUs)
	}

	// RecentDepths：缓存够 5 帧后直接用缓存，按时间先后
	time.Sleep(300 * time.Millisecond)
	t0 := time.Now()
	frames, err := cam.RecentDepths(5, time.Second, time.Second)
	if err != nil || len(frames) != 5 {
		t.Fatalf("%d 帧 %v", len(frames), err)
	}
	if st := cam.Stats(); st.DepthFrames < 5 || st.DepthDropped != 0 {
		t.Errorf("stats %+v", st)
	}
	for i := 1; i < len(frames); i++ {
		if frames[i].Seq != frames[i-1].Seq+1 {
			t.Fatalf("缓存帧不连续：%d %d", frames[i-1].Seq, frames[i].Seq)
		}
	}
	if time.Since(t0) > 200*time.Millisecond { // 只防"等新帧"（至少 50 ms），给慢的 CI 机器留余量
		t.Fatalf("缓存够时不应等待：%v", time.Since(t0))
	}

	// 彩色：RFC 2435 合成出来的 JPEG 能解
	c, err := cam.GrabColor(time.Second)
	if err != nil || c == nil {
		t.Fatalf("彩色 %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(c.JPEG))
	if err != nil || img.Bounds().Dx() != 64 {
		t.Fatalf("彩色解码 %v", err)
	}
	c2, seq := cam.LatestColor(c.Seq, time.Second)
	if c2 == nil || seq <= c.Seq {
		t.Fatalf("LatestColor 应等到新帧：%d <= %d", seq, c.Seq)
	}

	// 相机还开着时，第二个程序打不开（控制权被占）
	cam2 := fakeCamera(f, Config{OpenRetries: 2})
	if err := cam2.Open(); !errors.Is(err, ErrAccessDenied) {
		t.Fatalf("第二个程序应被拒绝：%v", err)
	}

	// 模拟断电：不再出帧 -> IsAlive 变 false，取帧报错
	f.paused.Store(true)
	// 暂停前已经在路上的那一帧（2 MB）可能晚到，不能用固定等待
	waitFor(t, 2*time.Second, func() bool { return !cam.IsAlive(0) })
	if e := cam.LastError(); !strings.HasPrefix(e, "no frames for") {
		t.Fatalf("LastError = %q", e)
	}
	if _, err := cam.RecentDepths(5, 200*time.Millisecond, 100*time.Millisecond); err == nil {
		t.Fatal("离线时不应拿缓存里的旧帧")
	}
	if c, _ := cam.GrabColor(100 * time.Millisecond); c != nil {
		t.Fatal("离线时不应返回旧彩色帧")
	}
	f.paused.Store(false)
	if _, err := cam.GrabDepth(time.Second); err != nil {
		t.Fatalf("恢复后取帧：%v", err)
	}

	// 关闭：TEARDOWN 两路、释放控制权；之后可以重新打开
	cam.Close()
	waitFor(t, time.Second, func() bool { return f.teardowns.Load() == 2 })
	if f.ccpValue() != 0 {
		t.Fatal("关闭后应释放控制权")
	}
	if _, err := cam.GrabDepth(100 * time.Millisecond); err == nil {
		t.Fatal("关闭后取帧应报错")
	}
	if err := cam.Open(); err != nil {
		t.Fatalf("重新打开：%v", err)
	}
	if _, err := cam.GrabDepth(time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestCameraStreamDropDetected(t *testing.T) {
	f := newFake435(t)
	cam := fakeCamera(f, Config{NoColor: true, AliveTimeout: 300 * time.Millisecond})
	if err := cam.Open(); err != nil {
		t.Fatal(err)
	}
	defer cam.Close()
	// 相机那头把连接断了（拔网线后 RST、相机重启）
	f.Close()
	waitFor(t, 2*time.Second, func() bool { return !cam.IsAlive(0) })
	if e := cam.LastError(); !strings.Contains(e, "depth stream stopped") {
		t.Fatalf("LastError 应带流中断原因：%q", e)
	}
}

func TestCameraRTCPKeepalive(t *testing.T) {
	f := newFake435(t)
	cam := fakeCamera(f, Config{NoColor: true})
	if err := cam.Open(); err != nil {
		t.Fatal(err)
	}
	defer cam.Close()
	cam.mu.Lock()
	s := cam.depthStream
	cam.mu.Unlock()
	if err := s.sess.sendReceiverReport(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return f.rtcpSeen.Load() > 0 })
}

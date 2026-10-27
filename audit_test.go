package orbbec

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestJPEGMetadataMalformed(t *testing.T) {
	for _, b := range [][]byte{
		{0xFF, 0xD8, 0xFF, 0xFE, 0, 0},
		{0xFF, 0xD8, 0xFF, 0xFE, 0, 1},
		{0xFF, 0xD8, 0xFF, 0xFE, 0xFF, 0xFF, 'O'},
	} {
		if m := jpegMetadata(b); m != nil {
			t.Errorf("%x: metadata %x", b, m)
		}
		_ = ensureHuffmanTables(b)
	}
	good := append([]byte{0xFF, 0xD8, 0xFF, 0xFE, 0, 10}, []byte("ORBBEC12")...)
	if m := jpegMetadata(good); string(m) != "ORBBEC12" {
		t.Errorf("metadata %q", m)
	}
}

func TestSessionTimeoutClamp(t *testing.T) {
	cases := map[string]time.Duration{
		"TEST;timeout=9223372037": rtspMaxSessionTTL,
		"TEST;timeout=20":         20 * time.Second,
		"TEST;timeout=1":          rtspMinSessionTTL,
		"TEST":                    60 * time.Second,
		"TEST;timeout=-5":         60 * time.Second,
	}
	for h, want := range cases {
		id, got := parseSessionHeader(h, 60*time.Second)
		if id != "TEST" || got != want {
			t.Errorf("%q: %q %v want %v", h, id, got, want)
		}
	}
}

func TestDeviceCloseSafety(t *testing.T) {
	f := newFake435(t)
	d, err := OpenDevice(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	s, err := d.StartDepth(context.Background(), StreamConfig{1280, 800, 10}, func(*DepthFrame) {})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() { defer wg.Done(); d.Close() }()
	}
	wg.Wait()
	s.Close() // 设备先关、流后关：不能 panic
	select {
	case <-s.Done():
	default:
		t.Fatal("device close should stop its streams")
	}
	if _, err := d.GetPropertyRange(PropLaserBool); !errors.Is(err, ErrClosed) {
		t.Errorf("GetPropertyRange after close: %v", err)
	}
	if err := d.SetProperty(PropLaserBool, 1); !errors.Is(err, ErrClosed) {
		t.Errorf("SetProperty after close: %v", err)
	}
	if _, err := d.StartDepth(context.Background(), StreamConfig{1280, 800, 10}, func(*DepthFrame) {}); !errors.Is(err, ErrClosed) {
		t.Errorf("StartDepth after close: %v", err)
	}
	if f.ccpValue() != 0 {
		t.Error("control not released")
	}
}

func TestStartStreamValidation(t *testing.T) {
	f := newFake435(t)
	d, err := OpenDevice(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.StartDepth(context.Background(), StreamConfig{1280, 800, 10}, nil); err == nil {
		t.Error("nil callback accepted")
	}
	for _, cfg := range []StreamConfig{{0, 800, 10}, {1280, -1, 10}, {1280, 800, 0}} {
		if _, err := d.StartDepth(context.Background(), cfg, func(*DepthFrame) {}); err == nil {
			t.Errorf("%v accepted", cfg)
		}
	}
}

func TestHandlerPanicFailsStream(t *testing.T) {
	f := newFake435(t)
	d, err := OpenDevice(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s, err := d.StartDepth(context.Background(), StreamConfig{1280, 800, 10}, func(*DepthFrame) { panic("boom") })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("stream should end after handler panic")
	}
	if e := s.Err(); e == nil || !strings.Contains(e.Error(), "panic") {
		t.Errorf("Err = %v", e)
	}
	s.Close()
}

func TestPrecisionChangeUpdatesUnit(t *testing.T) {
	f := newFake435(t)
	d, err := OpenDevice(context.Background(), f.options())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.DepthUnitMM() != 1 {
		t.Fatalf("unit %v", d.DepthUnitMM())
	}
	if err := d.SetProperty(PropDepthPrecisionLevelInt, int32(Precision0mm2)); err != nil {
		t.Fatal(err)
	}
	if d.DepthUnitMM() != 0.2 {
		t.Errorf("unit after change %v", d.DepthUnitMM())
	}
}

func TestCameraLifecycle(t *testing.T) {
	f := newFake435(t)
	cam := fakeCamera(f, Config{NoColor: true, Color: &StreamConfig{1280, 800, 10}})
	if err := cam.Open(); err != nil {
		t.Fatal(err)
	}
	if err := cam.Open(); !errors.Is(err, ErrAlreadyOpen) {
		t.Errorf("second Open: %v", err)
	}
	if !cam.IsAlive(0) {
		t.Error("second Open broke the session")
	}
	time.Sleep(300 * time.Millisecond)
	if cam.HasColor() || cam.ColorParams() != nil {
		t.Error("NoColor should win over Color")
	}
	cam.Close()

	// Open 进行中 Close：Open 被打断，不能 panic，之后状态一致
	for range 3 {
		done := make(chan error, 1)
		go func() { done <- cam.Open() }()
		time.Sleep(30 * time.Millisecond)
		cam.Close()
		<-done
		if cam.IsAlive(0) {
			t.Fatal("alive after Close")
		}
	}
	if err := cam.Open(); err != nil {
		t.Fatal(err)
	}
	defer cam.Close()
	if _, err := cam.GrabDepth(time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := cam.RecentDepths(3, time.Second, 0); err != nil {
		t.Errorf("RecentDepths with maxAge 0: %v", err)
	}
}

func TestOpenCancel(t *testing.T) {
	f := newFake435(t)
	cam := fakeCamera(f, Config{NoColor: true})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cam.OpenContext(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled Open: %v", err)
	}
	if cam.IsAlive(0) {
		t.Error("alive after cancelled open")
	}
}

func TestDepthOnlyFailure(t *testing.T) {
	f := newFake435(t)
	cam := fakeCamera(f, Config{AliveTimeout: 5 * time.Second})
	if err := cam.Open(); err != nil {
		t.Fatal(err)
	}
	defer cam.Close()
	if _, err := cam.GrabColor(time.Second); err != nil {
		t.Fatal(err)
	}
	f.dropConns("depth")
	waitFor(t, 2*time.Second, func() bool { return !cam.IsAlive(0) })
	if e := cam.LastError(); !strings.Contains(e, "depth stream stopped") {
		t.Errorf("LastError = %q", e)
	}
	t0 := time.Now()
	if _, err := cam.GrabDepth(5 * time.Second); err == nil || time.Since(t0) > time.Second {
		t.Errorf("GrabDepth should fail fast: %v after %v", err, time.Since(t0))
	}
	if !cam.HasColor() {
		t.Error("color should still be flowing")
	}
}

func TestCloneIndependent(t *testing.T) {
	d := &DepthFrame{Data: []uint16{1, 2}, Metadata: []byte("ORBBEC")}
	c := d.Clone()
	c.Data[0], c.Metadata[0] = 9, 'X'
	if d.Data[0] != 1 || d.Metadata[0] != 'O' {
		t.Error("DepthFrame.Clone shares memory")
	}
	cf := &ColorFrame{JPEG: []byte{1}, Metadata: []byte{2}}
	cc := cf.Clone()
	cc.JPEG[0], cc.Metadata[0] = 8, 9
	if cf.JPEG[0] != 1 || cf.Metadata[0] != 2 {
		t.Error("ColorFrame.Clone shares memory")
	}
}

func TestJPEGFragmentOffset(t *testing.T) {
	d := &jpegDepay{}
	pk := func(off int, data string) rtpPacket {
		h := []byte{0, byte(off >> 16), byte(off >> 8), byte(off), 1, 80, 8, 5}
		return rtpPacket{payload: append(h, data...)}
	}
	if _, err := d.depay(pk(0, "abc"), true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.depay(pk(100, "def"), false); err == nil {
		t.Error("wrong fragment offset accepted")
	}
	if b, err := d.depay(pk(3, "def"), false); err != nil || string(b) != "def" {
		t.Errorf("%q %v", b, err)
	}
}

func TestIntrinsicsFallbackAspect(t *testing.T) {
	c := Calibration{align: parseAlignCalibList(fixtureAlignCalib())}
	if _, err := c.DepthIntrinsics(640, 360); err == nil {
		t.Error("mismatched aspect ratio accepted")
	}
	if i, err := c.DepthIntrinsics(640, 400); err != nil || i.Width != 640 || i.Height != 400 {
		t.Errorf("%+v %v", i, err)
	}
}

func TestVendorPaddingSplit(t *testing.T) {
	f := newFake435(t)
	f.padDelay = 300 * time.Millisecond
	o := f.options()
	vc, err := dialVendor(context.Background(), o.IP, o.ControlPort, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer vc.Close()
	for range 3 {
		if r, err := vc.GetPropertyRange(PropDepthPrecisionLevelInt); err != nil || r.Cur != 4 {
			t.Fatalf("%+v %v", r, err)
		}
	}
}

func TestVendorDrainBounded(t *testing.T) {
	f := newFake435(t)
	o := f.options()
	vc, err := dialVendor(context.Background(), o.IP, o.ControlPort, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vc.GetPropertyRange(PropDepthPrecisionLevelInt); err != nil {
		t.Fatal(err)
	}
	f.garbage.Store(true)
	t0 := time.Now()
	if _, err := vc.GetPropertyRange(PropDepthPrecisionLevelInt); err == nil {
		t.Fatal("garbage accepted")
	}
	if time.Since(t0) > 2*time.Second {
		t.Errorf("command blocked for %v", time.Since(t0))
	}
	t0 = time.Now()
	vc.Close()
	if time.Since(t0) > time.Second {
		t.Errorf("Close blocked for %v", time.Since(t0))
	}
}

func TestGVCPUnreachableFails(t *testing.T) {
	f := newFake435(t)
	f.noGVCPRead.Store(true)
	o := f.options()
	if _, err := OpenDevice(context.Background(), o); err == nil {
		t.Error("open without control channel should fail")
	}
	o.ReadOnly = true
	d, err := OpenDevice(context.Background(), o)
	if err != nil {
		t.Fatalf("read-only open: %v", err)
	}
	d.Close()
}

func TestCloseIsFast(t *testing.T) {
	f := newFake435(t)
	cam := fakeCamera(f, Config{})
	if err := cam.Open(); err != nil {
		t.Fatal(err)
	}
	if _, err := cam.GrabColor(time.Second); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	cam.Close()
	if d := time.Since(t0); d > time.Second {
		t.Errorf("Close took %v (TEARDOWN response not matched?)", d)
	}
	if f.teardowns.Load() != 2 {
		t.Errorf("teardowns %d", f.teardowns.Load())
	}
}

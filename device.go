package orbbec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	depthRTSPPort = 8554
	colorRTSPPort = 8888

	defaultFrameIdleTimeout = 10 * time.Second
)

// ErrClosed 表示设备或相机已经关闭。
var ErrClosed = errors.New("device is closed")

// DeviceOptions 是打开设备的参数。
type DeviceOptions struct {
	IP     string
	Logger *slog.Logger
	// ReadOnly 不申请控制权、不写属性，可以和别的程序同时连着，但不能取流。
	ReadOnly bool
	// DepthPrecision 为 nil 时设成相机默认档（和官方 SDK 一致），PrecisionKeep 保持当前档位。
	DepthPrecision *DepthPrecision

	// 端口，0 用默认值。
	GVCPPort, ControlPort, DepthRTSPPort, ColorRTSPPort int

	// FrameIdleTimeout 内收不到流数据就认为流断了，默认 10 s。
	// 另一个客户端断开同一路流时相机会停发约 5 s，所以别设得太短。
	FrameIdleTimeout time.Duration
}

func defaultPort(p *int, def int) {
	if *p == 0 {
		*p = def
	}
}

// Device 是一台已连接的相机。方法可以并发调用；Close 后其它方法返回 ErrClosed。
type Device struct {
	opts     DeviceOptions
	log      *slog.Logger
	gvcp     *gvcpConn
	ccp      *ccpController
	ctrl     *vendorClient
	info     DeviceInfo
	ver      VersionInfo
	calib    Calibration
	profiles []EffectiveProfile

	mu      sync.Mutex
	unit    float32
	closed  bool
	streams map[*Stream]struct{}
}

// OpenDevice 连接相机并初始化，顺序和官方 SDK 一致：申请控制权 → 设通信方式和兼容版本 →
// 读版本、标定和有效分辨率 → 开相机端视差转深度、设深度精度。ctx 只管打开过程。
func OpenDevice(ctx context.Context, opts DeviceOptions) (*Device, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	defaultPort(&opts.GVCPPort, gvcpPort)
	defaultPort(&opts.ControlPort, vendorPort)
	defaultPort(&opts.DepthRTSPPort, depthRTSPPort)
	defaultPort(&opts.ColorRTSPPort, colorRTSPPort)
	if opts.FrameIdleTimeout <= 0 {
		opts.FrameIdleTimeout = defaultFrameIdleTimeout
	}
	d := &Device{opts: opts, log: log, unit: 1, streams: map[*Stream]struct{}{}}
	ok := false
	defer func() {
		if !ok {
			d.Close()
		}
	}()
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}

	var err error
	if d.gvcp, err = dialGVCP(opts.IP, opts.GVCPPort); err != nil {
		return nil, err
	}
	disc, err := d.gvcp.Discover()
	if err != nil {
		if !opts.ReadOnly {
			return nil, fmt.Errorf("GVCP discovery (UDP %d): %w", opts.GVCPPort, err)
		}
		log.Warn("GVCP discovery failed, skipping firmware check", "ip", opts.IP, "err", err)
	}
	name := disc.Model
	if disc.Manufacturer != "" && !strings.HasPrefix(name, disc.Manufacturer) {
		name = disc.Manufacturer + " " + name
	}
	d.info = DeviceInfo{Name: name, Serial: disc.Serial, Firmware: disc.Firmware, Connection: "Ethernet",
		PID: disc.PID, IP: opts.IP, MAC: disc.MAC}

	if !opts.ReadOnly {
		if d.ccp, err = acquireCCP(d.gvcp, firmwareVersionInt(disc.Firmware), disc.Serial, log, 0); err != nil {
			return nil, err
		}
	}
	if err := check(); err != nil {
		return nil, err
	}
	if d.ctrl, err = dialVendor(ctx, opts.IP, opts.ControlPort, 3*time.Second); err != nil {
		return nil, err
	}
	if !opts.ReadOnly {
		d.trySet(PropDeviceCommunicationTypeInt, commTypeNet)
		d.trySet(PropCompatVersionInt, compatVersion)
	}
	if err := check(); err != nil {
		return nil, err
	}

	raw, err := d.ctrl.GetStructure(StructVersion)
	if err != nil {
		return nil, fmt.Errorf("read version info: %w", err)
	}
	if d.ver, err = parseVersionInfo(raw); err != nil {
		return nil, err
	}
	d.info.Serial, d.info.Firmware, d.info.Hardware = d.ver.Serial, d.ver.Firmware, d.ver.Hardware
	if d.info.Name == "" {
		d.info.Name = d.ver.DeviceName
	}
	if d.info.PID == 0 {
		if pid, err := d.ctrl.GetProperty(PropDevicePIDInt); err == nil {
			d.info.PID = uint16(pid)
		}
	}

	if err := d.loadCalibration(); err != nil {
		return nil, err
	}
	if b, err := d.ctrl.GetStructList(RawDataEffectiveStreamProfileList, 0); err != nil {
		log.Warn("read stream profiles failed", "err", err)
	} else {
		d.profiles = parseEffectiveProfiles(b)
	}
	if err := check(); err != nil {
		return nil, err
	}
	if err := d.setupDepth(); err != nil {
		return nil, err
	}
	ok = true
	log.Info("camera connected", "name", d.info.Name, "serial", d.info.Serial, "firmware", d.info.Firmware,
		"ip", opts.IP, "control", d.ccp != nil, "depth_unit_mm", d.unit, "baseline_mm", d.calib.BaselineMM())
	return d, nil
}

// trySet 写一个不影响数据含义的属性，失败只记日志。
func (d *Device) trySet(id uint32, v int32) {
	if err := d.ctrl.SetProperty(id, v); err != nil && !errors.Is(err, ErrClosed) {
		d.log.Warn("set property failed", "id", id, "value", v, "err", err)
	}
}

func (d *Device) loadCalibration() error {
	b, err := d.ctrl.GetStructList(RawDataDepthCalibParam, 1)
	if err != nil {
		d.log.Warn("read depth calibration failed, baseline unknown", "err", err)
	} else {
		d.calib.Depth = parseDepthCalibList(b)
	}
	if b, err = d.ctrl.GetStructList(RawDataAlignCalibParam, 0); err != nil {
		return fmt.Errorf("read camera calibration: %w", err)
	}
	if d.calib.align = parseAlignCalibList(b); len(d.calib.align) == 0 {
		return errors.New("camera returned no calibration")
	}
	if b, err = d.ctrl.GetStructList(RawDataD2CAlignSupportProfileList, 0); err != nil {
		d.log.Warn("read D2C profile list failed", "err", err)
	} else {
		d.calib.d2c = parseD2CProfileList(b)
	}
	return nil
}

// setupDepth 开相机端视差转深度、设精度档位，并按读回的档位确定深度单位；硬件 D2C 开着时关掉。
// 视差转深度和硬件 D2C 决定深度数据的含义，写失败算打开失败。
func (d *Device) setupDepth() error {
	r, err := d.ctrl.GetPropertyRange(PropDepthPrecisionLevelInt)
	if err != nil {
		return fmt.Errorf("read depth precision: %w", err)
	}
	if !d.opts.ReadOnly {
		if err := d.ctrl.SetProperty(PropDisparityToDepthBool, 1); err != nil {
			return fmt.Errorf("enable disparity to depth: %w", err)
		}
		want := r.Def
		if d.opts.DepthPrecision != nil {
			want = int32(*d.opts.DepthPrecision)
		}
		if want != int32(PrecisionKeep) && want != r.Cur {
			d.trySet(PropDepthPrecisionLevelInt, want)
		}
		if v, err := d.ctrl.GetProperty(PropDepthAlignHardwareBool); err == nil && v != 0 {
			d.log.Warn("hardware D2C is enabled, disabling it")
			if err := d.ctrl.SetProperty(PropDepthAlignHardwareBool, 0); err != nil {
				return fmt.Errorf("disable hardware D2C: %w", err)
			}
		}
	}
	return d.refreshUnit()
}

// refreshUnit 读回精度档位并更新深度单位。
func (d *Device) refreshUnit() error {
	cur, err := d.ctrl.GetProperty(PropDepthPrecisionLevelInt)
	if err != nil {
		return fmt.Errorf("read depth precision: %w", err)
	}
	unit, err := DepthPrecision(cur).Unit()
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.unit = unit
	d.mu.Unlock()
	return nil
}

func (d *Device) Info() DeviceInfo     { return d.info }
func (d *Device) Version() VersionInfo { return d.ver }

// Profiles 返回相机报告的有效流配置（副本）。
func (d *Device) Profiles() []EffectiveProfile { return append([]EffectiveProfile(nil), d.profiles...) }

// Calibration 返回从相机读出的标定（只读）。
func (d *Device) Calibration() *Calibration { return &d.calib }

// DepthUnitMM 返回当前深度单位（mm）。
func (d *Device) DepthUnitMM() float32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.unit
}

// GetPropertyRange 读一个属性的当前值和范围。
func (d *Device) GetPropertyRange(id uint32) (PropertyRange, error) {
	if d.isClosed() {
		return PropertyRange{}, ErrClosed
	}
	return d.ctrl.GetPropertyRange(id)
}

// GetProperty 读一个整型/布尔属性的当前值。
func (d *Device) GetProperty(id uint32) (int32, error) {
	r, err := d.GetPropertyRange(id)
	return r.Cur, err
}

// SetProperty 写一个整型/布尔属性。改深度精度会更新 DepthUnitMM，但已在收的深度流仍按旧单位换算，
// 要先关流再改。
func (d *Device) SetProperty(id uint32, v int32) error {
	if d.isClosed() {
		return ErrClosed
	}
	if d.opts.ReadOnly {
		return errors.New("device is opened read-only")
	}
	if err := d.ctrl.SetProperty(id, v); err != nil {
		return err
	}
	if id == PropDepthPrecisionLevelInt {
		return d.refreshUnit()
	}
	return nil
}

// ControlLost 表示控制权被别的程序拿走了。
func (d *Device) ControlLost() bool { return d.ccp != nil && d.ccp.lost.Load() }

func (d *Device) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// Close 关掉这台设备起的所有流、释放控制权并断开，可重复调用。
func (d *Device) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	streams := make([]*Stream, 0, len(d.streams))
	for s := range d.streams {
		streams = append(streams, s)
	}
	d.mu.Unlock()
	for _, s := range streams {
		s.Close()
	}
	if d.ccp != nil {
		d.ccp.release()
	}
	if d.ctrl != nil {
		_ = d.ctrl.Close()
	}
	if d.gvcp != nil {
		_ = d.gvcp.Close()
	}
	return nil
}

// StreamConfig 是一路流的分辨率和帧率。
type StreamConfig struct {
	Width, Height, FPS int
}

func (c StreamConfig) validate() error {
	if c.Width <= 0 || c.Height <= 0 || c.FPS <= 0 || c.Width > 1<<14 || c.Height > 1<<14 {
		return fmt.Errorf("invalid stream config %dx%d@%d", c.Width, c.Height, c.FPS)
	}
	return nil
}

// Stream 是一路正在接收的流。
type Stream struct {
	sess       *rtspSession
	log        *slog.Logger
	done       chan struct{}
	stop       chan struct{}
	err        atomic.Pointer[error]
	once       sync.Once
	dropped    atomic.Int64
	frames     atomic.Int64
	afterClose func(failed bool)
}

// Done 在读循环退出后关闭；此时连接已关，Err 给出原因（正常 Close 时为 nil）。
func (s *Stream) Done() <-chan struct{} { return s.done }

// Err 返回流异常结束的原因。
func (s *Stream) Err() error {
	if p := s.err.Load(); p != nil {
		return *p
	}
	return nil
}

// Stats 返回收到的帧数和丢弃的帧数。
func (s *Stream) Stats() (frames, dropped int64) { return s.frames.Load(), s.dropped.Load() }

// Close 结束会话并等读循环退出，可重复调用。不能在 onFrame 回调里调用（回调在读循环上执行，会死锁）。
func (s *Stream) Close() {
	s.once.Do(func() {
		close(s.stop)
		select {
		case <-s.done:
		default:
			s.sess.teardown(2 * time.Second)
		}
		<-s.done
		if s.afterClose != nil {
			s.afterClose(s.Err() != nil)
		}
	})
}

func (s *Stream) fail(err error) {
	s.err.CompareAndSwap(nil, &err)
}

// StartDepth 开深度流（Y16），onFrame 在读循环里调用：不能阻塞，也不能调用 Close。ctx 只管起流过程。
func (d *Device) StartDepth(ctx context.Context, cfg StreamConfig, onFrame func(*DepthFrame)) (*Stream, error) {
	if onFrame == nil {
		return nil, errors.New("StartDepth: onFrame is nil")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := checkProfile(d.profiles, SensorDepth, FormatY16, cfg); err != nil {
		return nil, err
	}
	url := rtspStreamURL(d.opts.IP, d.opts.DepthRTSPPort, "depth", cfg.Width, cfg.Height, cfg.FPS, "y16")
	w, h, unit := cfg.Width, cfg.Height, d.DepthUnitMM()
	var seq uint64
	handle := func(frame []byte, s *Stream) {
		hdr, md, data, err := splitVendorFrame(frame)
		if err != nil || (hdr.Width != 0 && int(hdr.Width) != w) || (hdr.Height != 0 && int(hdr.Height) != h) {
			s.dropped.Add(1)
			return
		}
		pix, err := decodeY16(data, w, h, unit)
		if err != nil {
			s.dropped.Add(1)
			return
		}
		seq++
		s.frames.Add(1)
		onFrame(&DepthFrame{Width: w, Height: h, Data: pix, Seq: seq, DeviceSeq: hdr.FrameCounter,
			DeviceTimestampUs: hdr.Timestamp * 1000, Time: time.Now(), Metadata: append([]byte(nil), md...)})
	}
	return d.startStream(ctx, url, "OB_FMT_Y16", w*h*2+64<<10, nil, handle, PropStopDepthStreamBool)
}

// StartColor 开彩色流（MJPEG），onFrame 收到完整 JPEG。约束同 StartDepth。
func (d *Device) StartColor(ctx context.Context, cfg StreamConfig, onFrame func(*ColorFrame)) (*Stream, error) {
	if onFrame == nil {
		return nil, errors.New("StartColor: onFrame is nil")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := checkProfile(d.profiles, SensorColor, FormatMJPG, cfg); err != nil {
		return nil, err
	}
	url := rtspStreamURL(d.opts.IP, d.opts.ColorRTSPPort, "color", cfg.Width, cfg.Height, cfg.FPS, "mjpg")
	jd := &jpegDepay{}
	var seq uint64
	handle := func(frame []byte, s *Stream) {
		j := jd.finish(frame)
		seq++
		s.frames.Add(1)
		onFrame(&ColorFrame{Width: cfg.Width, Height: cfg.Height, JPEG: j, Seq: seq, Time: time.Now(),
			Metadata: jpegMetadata(j)})
	}
	return d.startStream(ctx, url, "", 8<<20, jd, handle, PropStopColorStreamBool)
}

// startStream 起一路 RTSP。和官方 SDK 一样，起流前和关流后各发一次 STOP_*_STREAM。
func (d *Device) startStream(ctx context.Context, url, wantCodec string, maxBytes int, jd *jpegDepay,
	handle func([]byte, *Stream), stopProp uint32) (*Stream, error) {
	if d.isClosed() {
		return nil, ErrClosed
	}
	if d.opts.ReadOnly {
		return nil, errors.New("cannot stream on a read-only device")
	}
	d.trySet(stopProp, 1)
	sess, err := dialRTSP(ctx, url)
	if err != nil {
		return nil, err
	}
	asm := &frameAssembler{maxBytes: maxBytes}
	switch {
	case jd != nil:
		switch {
		case sess.codec == "JPEG":
			asm.depay = jd.depay
		case strings.HasPrefix(sess.codec, "OB_FMT_MJPEG"):
			inner := handle
			handle = func(frame []byte, s *Stream) {
				_, _, data, err := splitVendorFrame(frame)
				if err != nil {
					s.dropped.Add(1)
					return
				}
				inner(data, s)
			}
		default:
			sess.teardown(0)
			return nil, fmt.Errorf("%s: unsupported color codec %q", url, sess.codec)
		}
	case sess.codec != wantCodec:
		sess.teardown(0)
		return nil, fmt.Errorf("%s: codec %q, want %q", url, sess.codec, wantCodec)
	}
	s := &Stream{sess: sess, log: d.log, done: make(chan struct{}), stop: make(chan struct{})}
	s.afterClose = func(failed bool) {
		d.mu.Lock()
		delete(d.streams, s)
		closed := d.closed
		d.mu.Unlock()
		// 流异常结束多半是相机不可达，不再花时间等 STOP 命令超时；下次起流前还会再发
		if !closed && !failed {
			d.trySet(stopProp, 1)
		}
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		sess.teardown(0)
		return nil, ErrClosed
	}
	d.streams[s] = struct{}{}
	d.mu.Unlock()
	go s.keepalive()
	go s.readLoop(asm, handle, d.opts.FrameIdleTimeout)
	return s, nil
}

// keepalive 定期发 RTCP，避免服务端按会话超时回收。
func (s *Stream) keepalive() {
	t := time.NewTicker(min(s.sess.timeout/3, 10*time.Second))
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			if err := s.sess.sendReceiverReport(); err != nil {
				s.log.Debug("send RTCP failed", "err", err)
			}
		}
	}
}

func (s *Stream) readLoop(asm *frameAssembler, handle func([]byte, *Stream), idle time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			s.fail(fmt.Errorf("stream handler panic: %v", r))
		}
		_ = s.sess.conn.Close()
		close(s.done)
	}()
	for {
		_ = s.sess.conn.SetReadDeadline(time.Now().Add(idle))
		ch, data, resp, err := s.sess.readMessage()
		if err != nil {
			select {
			case <-s.stop:
			default:
				s.fail(fmt.Errorf("read RTSP stream: %w", err))
			}
			return
		}
		if resp != nil {
			if s.sess.isTeardownResponse(resp) {
				return
			}
			continue
		}
		if ch != s.sess.rtpChan {
			continue
		}
		p, err := parseRTP(data)
		if err != nil {
			continue
		}
		before := asm.dropped
		if frame := asm.push(p); frame != nil {
			handle(frame, s)
		}
		s.dropped.Add(int64(asm.dropped - before))
	}
}

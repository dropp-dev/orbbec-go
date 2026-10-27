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
)

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
}

func defaultPort(p *int, def int) {
	if *p == 0 {
		*p = def
	}
}

// Device 是一台已连接的相机。
type Device struct {
	opts     DeviceOptions
	log      *slog.Logger
	gvcp     *gvcpConn
	ccp      *ccpController
	ctrl     *vendorClient
	info     DeviceInfo
	ver      VersionInfo
	calib    Calibration
	unit     float32
	profiles []EffectiveProfile
}

// OpenDevice 连接相机并初始化，顺序和官方 SDK 一致：申请控制权 → 设通信方式和兼容版本 →
// 读版本、标定和有效分辨率 → 开相机端视差转深度、设深度精度。
func OpenDevice(ctx context.Context, opts DeviceOptions) (*Device, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	defaultPort(&opts.GVCPPort, gvcpPort)
	defaultPort(&opts.ControlPort, vendorPort)
	defaultPort(&opts.DepthRTSPPort, depthRTSPPort)
	defaultPort(&opts.ColorRTSPPort, colorRTSPPort)
	d := &Device{opts: opts, log: log, unit: 1}
	ok := false
	defer func() {
		if !ok {
			d.Close()
		}
	}()

	var err error
	if d.gvcp, err = dialGVCP(opts.IP, opts.GVCPPort); err != nil {
		return nil, err
	}
	disc, err := d.gvcp.Discover()
	if err != nil {
		log.Warn("GVCP discovery failed, skipping firmware check", "ip", opts.IP, "err", err)
	}
	name := disc.Model
	if disc.Manufacturer != "" && !strings.HasPrefix(name, disc.Manufacturer) {
		name = disc.Manufacturer + " " + name
	}
	d.info = DeviceInfo{Name: name, Serial: disc.Serial, Firmware: disc.Firmware, Connection: "Ethernet",
		PID: disc.PID, IP: opts.IP, MAC: disc.MAC}

	if !opts.ReadOnly {
		if d.ccp, err = acquireCCP(d.gvcp, firmwareVersionInt(disc.Firmware), disc.Serial, log); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.ctrl, err = dialVendor(opts.IP, opts.ControlPort, 3*time.Second); err != nil {
		return nil, err
	}
	if !opts.ReadOnly {
		d.trySet(PropDeviceCommunicationTypeInt, commTypeNet)
		d.trySet(PropCompatVersionInt, compatVersion)
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
	if err := d.setupDepth(); err != nil {
		return nil, err
	}
	ok = true
	log.Info("camera connected", "name", d.info.Name, "serial", d.info.Serial, "firmware", d.info.Firmware,
		"ip", opts.IP, "control", d.ccp != nil, "depth_unit_mm", d.unit, "baseline_mm", d.calib.BaselineMM())
	return d, nil
}

func (d *Device) trySet(id uint32, v int32) {
	if err := d.ctrl.SetProperty(id, v); err != nil {
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
func (d *Device) setupDepth() error {
	r, err := d.ctrl.GetPropertyRange(PropDepthPrecisionLevelInt)
	if err != nil {
		return fmt.Errorf("read depth precision: %w", err)
	}
	if !d.opts.ReadOnly {
		d.trySet(PropDisparityToDepthBool, 1)
		want := r.Def
		if d.opts.DepthPrecision != nil {
			want = int32(*d.opts.DepthPrecision)
		}
		if want != int32(PrecisionKeep) && want != r.Cur {
			d.trySet(PropDepthPrecisionLevelInt, want)
		}
		if r.Cur, err = d.ctrl.GetProperty(PropDepthPrecisionLevelInt); err != nil {
			return fmt.Errorf("read depth precision: %w", err)
		}
		if v, err := d.ctrl.GetProperty(PropDepthAlignHardwareBool); err == nil && v != 0 {
			d.log.Warn("hardware D2C is enabled, disabling it")
			d.trySet(PropDepthAlignHardwareBool, 0)
		}
	}
	d.unit, err = DepthPrecision(r.Cur).Unit()
	return err
}

func (d *Device) Info() DeviceInfo             { return d.info }
func (d *Device) Version() VersionInfo         { return d.ver }
func (d *Device) Profiles() []EffectiveProfile { return d.profiles }
func (d *Device) Calibration() *Calibration    { return &d.calib }
func (d *Device) DepthUnitMM() float32         { return d.unit }
func (d *Device) GetPropertyRange(id uint32) (PropertyRange, error) {
	return d.ctrl.GetPropertyRange(id)
}

// SetProperty 写一个整型/布尔属性。
func (d *Device) SetProperty(id uint32, v int32) error {
	if d.opts.ReadOnly {
		return errors.New("device is opened read-only")
	}
	return d.ctrl.SetProperty(id, v)
}

// ControlLost 表示控制权被别的程序拿走了。
func (d *Device) ControlLost() bool { return d.ccp != nil && d.ccp.lost.Load() }

// Close 释放控制权并断开，可重复调用。
func (d *Device) Close() error {
	if d.ccp != nil {
		d.ccp.release()
		d.ccp = nil
	}
	if d.ctrl != nil {
		_ = d.ctrl.Close()
		d.ctrl = nil
	}
	if d.gvcp != nil {
		_ = d.gvcp.Close()
		d.gvcp = nil
	}
	return nil
}

// StreamConfig 是一路流的分辨率和帧率。
type StreamConfig struct {
	Width, Height, FPS int
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
	afterClose func()
}

// Done 在流结束时关闭。
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

// Close 结束会话并等读循环退出。
func (s *Stream) Close() {
	s.once.Do(func() {
		close(s.stop)
		select {
		case <-s.done:
			_ = s.sess.conn.Close()
		default:
			s.sess.teardown(2 * time.Second)
		}
		<-s.done
		if s.afterClose != nil {
			s.afterClose()
		}
	})
}

func (s *Stream) fail(err error) {
	s.err.CompareAndSwap(nil, &err)
}

// StartDepth 开深度流（Y16），onFrame 在读循环里调用。
func (d *Device) StartDepth(ctx context.Context, cfg StreamConfig, onFrame func(*DepthFrame)) (*Stream, error) {
	if err := checkProfile(d.profiles, SensorDepth, FormatY16, cfg); err != nil {
		return nil, err
	}
	url := rtspStreamURL(d.opts.IP, d.opts.DepthRTSPPort, "depth", cfg.Width, cfg.Height, cfg.FPS, "y16")
	w, h, unit := cfg.Width, cfg.Height, d.unit
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

// StartColor 开彩色流（MJPEG），onFrame 收到完整 JPEG。
func (d *Device) StartColor(ctx context.Context, cfg StreamConfig, onFrame func(*ColorFrame)) (*Stream, error) {
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
		onFrame(&ColorFrame{Width: cfg.Width, Height: cfg.Height, JPEG: append([]byte(nil), j...), Seq: seq, Time: time.Now(),
			Metadata: jpegMetadata(j)})
	}
	return d.startStream(ctx, url, "", 8<<20, jd, handle, PropStopColorStreamBool)
}

// startStream 起一路 RTSP。和官方 SDK 一样，起流前和关流后各发一次 STOP_*_STREAM。
func (d *Device) startStream(ctx context.Context, url, wantCodec string, maxBytes int, jd *jpegDepay,
	handle func([]byte, *Stream), stopProp uint32) (*Stream, error) {
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
			sess.teardown(time.Second)
			return nil, fmt.Errorf("%s: unsupported color codec %q", url, sess.codec)
		}
	case sess.codec != wantCodec:
		sess.teardown(time.Second)
		return nil, fmt.Errorf("%s: codec %q, want %q", url, sess.codec, wantCodec)
	}
	s := &Stream{sess: sess, log: d.log, done: make(chan struct{}), stop: make(chan struct{}),
		afterClose: func() { d.trySet(stopProp, 1) }}
	go s.keepalive()
	go s.readLoop(asm, handle)
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

// frameIdleTimeout 内收不到数据就认为流断了。另一个客户端断开同一路流时相机会停发约 5 s，所以留余量。
var frameIdleTimeout = 10 * time.Second

func (s *Stream) readLoop(asm *frameAssembler, handle func([]byte, *Stream)) {
	defer close(s.done)
	for {
		_ = s.sess.conn.SetReadDeadline(time.Now().Add(frameIdleTimeout))
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
			s.sess.wmu.Lock()
			teardown, done := s.sess.teardownCSeq, s.sess.teardownDone
			s.sess.wmu.Unlock()
			if teardown != 0 && resp.header("CSeq") == fmt.Sprint(teardown) {
				close(done)
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

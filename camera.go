package orbbec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Config 是 Camera 的参数，零值字段用默认值。
type Config struct {
	IP     string
	Logger *slog.Logger
	// 端口，0 用默认值。
	GVCPPort, ControlPort, DepthRTSPPort, ColorRTSPPort int

	Depth StreamConfig // 默认 1280x800@10
	// Color 为 nil 时用 1280x800@10；NoColor 为 true 时不开彩色流（优先于 Color）。
	Color   *StreamConfig
	NoColor bool

	DepthPrecision *DepthPrecision // 见 DeviceOptions.DepthPrecision

	RingSize         int           // 缓存的深度帧数，默认 8
	AliveTimeout     time.Duration // 多久没有新深度帧算离线，默认 3 s
	OpenRetries      int           // 默认 5
	RetryWait        time.Duration // 默认 3 s
	FrameIdleTimeout time.Duration // 见 DeviceOptions.FrameIdleTimeout
}

func (c *Config) withDefaults() Config {
	out := *c
	if out.Logger == nil {
		out.Logger = slog.Default()
	}
	if out.Depth.Width == 0 {
		out.Depth = StreamConfig{1280, 800, 10}
	}
	if out.NoColor {
		out.Color = nil
	} else if out.Color == nil {
		out.Color = &StreamConfig{1280, 800, 10}
	} else {
		cc := *out.Color
		out.Color = &cc
	}
	if out.DepthPrecision != nil {
		p := *out.DepthPrecision
		out.DepthPrecision = &p
	}
	if out.RingSize <= 0 {
		out.RingSize = 8
	}
	if out.AliveTimeout <= 0 {
		out.AliveTimeout = 3 * time.Second
	}
	if out.OpenRetries <= 0 {
		out.OpenRetries = 5
	}
	if out.RetryWait <= 0 {
		out.RetryWait = 3 * time.Second
	}
	return out
}

var (
	// ErrTimeout 表示在给定时间内没有等到帧。
	ErrTimeout = errors.New("timed out waiting for a frame")
	// ErrAlreadyOpen 表示 Camera 已经打开。
	ErrAlreadyOpen = errors.New("camera is already open")
)

// Camera 持续接收深度和彩色并缓存最近几帧。深度流停了 IsAlive 返回 false，Close 再 Open 即可重连。
// 返回的帧是共享的，只读；要修改先 Clone。必须用 New 创建；Open/Close 可以和取帧方法并发调用。
type Camera struct {
	cfg Config
	log *slog.Logger

	lifecycle  sync.Mutex // 串行化 Open/Close
	openCancel context.CancelFunc

	mu          sync.Mutex
	dev         *Device
	depthStream *Stream
	colorStream *Stream
	intr        Intrinsics
	colorParams *ColorParams
	info        DeviceInfo
	baseline    float64

	ring      []*DepthFrame
	depthSeq  uint64
	depthAt   time.Time
	depthErr  error
	color     *ColorFrame
	colorSeq  uint64
	colorAt   time.Time
	notify    chan struct{} // 每来一帧关闭并换新
	streamErr error
}

// New 创建相机对象，不连接。
func New(cfg Config) *Camera {
	c := cfg.withDefaults()
	return &Camera{cfg: c, log: c.Logger, notify: make(chan struct{})}
}

// Open 连接相机并开始取流，失败时按 RetryWait 重试。已打开时返回 ErrAlreadyOpen。
func (c *Camera) Open() error { return c.OpenContext(context.Background()) }

// OpenContext 同 Open，可取消；Close 也会中断正在进行的打开。
func (c *Camera) OpenContext(ctx context.Context) error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	c.mu.Lock()
	open := c.dev != nil
	c.mu.Unlock()
	if open {
		return ErrAlreadyOpen
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.mu.Lock()
	c.openCancel = cancel
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.openCancel = nil
		c.mu.Unlock()
	}()

	var lastErr error
	for attempt := 1; attempt <= c.cfg.OpenRetries; attempt++ {
		if lastErr = c.openOnce(ctx); lastErr == nil {
			return nil
		}
		c.closeLocked()
		if ctx.Err() != nil {
			return fmt.Errorf("open camera: %w", lastErr)
		}
		if attempt == c.cfg.OpenRetries {
			return fmt.Errorf("open camera: %d attempts failed: %w", attempt, lastErr)
		}
		c.log.Warn("open camera failed, retrying", "attempt", attempt, "err", lastErr, "wait", c.cfg.RetryWait)
		select {
		case <-ctx.Done():
			return fmt.Errorf("open camera: %w", ctx.Err())
		case <-time.After(c.cfg.RetryWait):
		}
	}
	return lastErr
}

func (c *Camera) openOnce(ctx context.Context) error {
	dev, err := OpenDevice(ctx, DeviceOptions{IP: c.cfg.IP, Logger: c.log, DepthPrecision: c.cfg.DepthPrecision,
		GVCPPort: c.cfg.GVCPPort, ControlPort: c.cfg.ControlPort, DepthRTSPPort: c.cfg.DepthRTSPPort,
		ColorRTSPPort: c.cfg.ColorRTSPPort, FrameIdleTimeout: c.cfg.FrameIdleTimeout})
	if err != nil {
		return err
	}
	cal := dev.Calibration()
	intr, err := cal.DepthIntrinsics(c.cfg.Depth.Width, c.cfg.Depth.Height)
	if err != nil {
		dev.Close()
		return err
	}
	c.mu.Lock()
	c.dev, c.intr, c.colorParams, c.info, c.baseline = dev, intr, nil, dev.Info(), cal.BaselineMM()
	c.ring, c.color, c.streamErr, c.depthErr = nil, nil, nil, nil
	c.mu.Unlock()

	ds, err := dev.StartDepth(ctx, c.cfg.Depth, c.onDepth)
	if err != nil {
		return fmt.Errorf("start depth stream: %w", err)
	}
	c.mu.Lock()
	c.depthStream = ds
	c.mu.Unlock()
	go c.watch(ds, "depth")
	c.log.Info("depth stream started", "width", c.cfg.Depth.Width, "height", c.cfg.Depth.Height, "fps", c.cfg.Depth.FPS)

	if c.cfg.Color != nil {
		cs, err := dev.StartColor(ctx, *c.cfg.Color, c.onColor)
		if err != nil {
			c.log.Warn("color stream unavailable, depth only", "err", err)
		} else {
			var cp *ColorParams
			if ci, err := cal.ColorIntrinsics(c.cfg.Color.Width, c.cfg.Color.Height); err == nil {
				if ext, ok := cal.DepthToColor(); ok {
					cp = &ColorParams{Intrinsics: ci, DepthToRGB: ext}
				}
			}
			c.mu.Lock()
			c.colorStream, c.colorParams = cs, cp
			c.mu.Unlock()
			go c.watch(cs, "color")
		}
	}

	if _, err := c.grabDepth(ctx, 5*time.Second); err != nil {
		return err
	}
	return nil
}

// watch 记录流异常结束的原因并唤醒等帧的调用方。
func (c *Camera) watch(s *Stream, name string) {
	<-s.Done()
	if err := s.Err(); err != nil {
		c.log.Warn("stream stopped", "stream", name, "err", err)
		c.mu.Lock()
		if c.streamErr == nil {
			c.streamErr = fmt.Errorf("%s stream stopped: %w", name, err)
		}
		if name == "depth" && (c.depthStream == s || c.depthStream == nil) {
			c.depthErr = fmt.Errorf("depth stream stopped: %w", err)
		}
		c.broadcastLocked()
		c.mu.Unlock()
	}
}

func (c *Camera) broadcastLocked() {
	close(c.notify)
	c.notify = make(chan struct{})
}

func (c *Camera) onDepth(f *DepthFrame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.depthSeq++
	f.Seq = c.depthSeq
	c.ring = append(c.ring, f)
	if len(c.ring) > c.cfg.RingSize {
		c.ring = append(c.ring[:0], c.ring[len(c.ring)-c.cfg.RingSize:]...)
	}
	c.depthAt = f.Time
	c.broadcastLocked()
}

func (c *Camera) onColor(f *ColorFrame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.colorSeq++
	f.Seq = c.colorSeq
	c.color, c.colorAt = f, f.Time
	c.broadcastLocked()
}

// Close 关流并释放控制权，可重复调用；会中断正在进行的 Open。
func (c *Camera) Close() error {
	c.mu.Lock()
	if c.openCancel != nil {
		c.openCancel()
	}
	c.mu.Unlock()
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	c.closeLocked()
	return nil
}

// closeLocked 关掉当前会话，调用方持 lifecycle。
func (c *Camera) closeLocked() {
	c.mu.Lock()
	ds, cs, dev := c.depthStream, c.colorStream, c.dev
	c.depthStream, c.colorStream, c.dev = nil, nil, nil
	c.ring, c.color, c.colorParams = nil, nil, nil
	c.broadcastLocked()
	c.mu.Unlock()
	if cs != nil {
		cs.Close()
	}
	if ds != nil {
		ds.Close()
	}
	if dev != nil {
		dev.Close()
	}
}

// Intrinsics 返回深度流内参。
func (c *Camera) Intrinsics() Intrinsics {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.intr
}

// ColorParams 返回彩色内参和深度到彩色外参（副本），没有彩色流时为 nil。
func (c *Camera) ColorParams() *ColorParams {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.colorParams == nil {
		return nil
	}
	cp := *c.colorParams
	return &cp
}

// DeviceInfo 返回设备信息。
func (c *Camera) DeviceInfo() DeviceInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// BaselineMM 返回双目基线（mm），未知时为 0。
func (c *Camera) BaselineMM() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.baseline
}

// IsAlive 表示 maxAge（0 用 AliveTimeout）内收到过深度帧。
func (c *Camera) IsAlive(maxAge time.Duration) bool {
	if maxAge <= 0 {
		maxAge = c.cfg.AliveTimeout
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dev != nil && c.depthErr == nil && time.Since(c.depthAt) <= maxAge
}

// LastError 返回离线原因，在线时为空。
func (c *Camera) LastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dev == nil {
		return "camera not opened"
	}
	if c.dev.ControlLost() {
		return "camera control taken by another program"
	}
	if c.depthErr != nil {
		return c.depthErr.Error()
	}
	if age := time.Since(c.depthAt); age > c.cfg.AliveTimeout {
		msg := fmt.Sprintf("no frames for %.0fs", age.Seconds())
		if c.streamErr != nil {
			msg += " (" + c.streamErr.Error() + ")"
		}
		return msg
	}
	return ""
}

// waitLocked 持锁调用，等下一帧、ctx 取消或超时，返回时仍持锁。
func (c *Camera) waitLocked(ctx context.Context, deadline time.Time) bool {
	ch := c.notify
	c.mu.Unlock()
	defer c.mu.Lock()
	remain := time.Until(deadline)
	if remain <= 0 {
		return false
	}
	t := time.NewTimer(remain)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-ctx.Done():
		return false
	case <-t.C:
		return false
	}
}

// GrabDepth 返回下一帧新的深度图；超时返回 ErrTimeout，关闭返回 ErrClosed。
func (c *Camera) GrabDepth(timeout time.Duration) (*DepthFrame, error) {
	return c.grabDepth(context.Background(), timeout)
}

func (c *Camera) grabDepth(ctx context.Context, timeout time.Duration) (*DepthFrame, error) {
	deadline := time.Now().Add(timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dev == nil {
		return nil, ErrClosed
	}
	seq := c.depthSeq
	for c.depthSeq == seq {
		if c.depthErr != nil {
			return nil, c.depthErr
		}
		if !c.waitLocked(ctx, deadline) && c.depthSeq == seq {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("%w: no new depth frame within %v (%s)", ErrTimeout, timeout, c.errLocked())
		}
		if c.dev == nil {
			return nil, ErrClosed
		}
	}
	return c.ring[len(c.ring)-1], nil
}

func (c *Camera) errLocked() string {
	if c.streamErr != nil {
		return c.streamErr.Error()
	}
	return "no frames"
}

// RecentDepths 返回缓存里最近 n 帧（按时间先后）。最新一帧早于 maxAge（0 用 AliveTimeout）时等新帧，
// 避免离线后用旧帧。
func (c *Camera) RecentDepths(n int, timeout, maxAge time.Duration) ([]*DepthFrame, error) {
	n = max(1, min(n, c.cfg.RingSize))
	if maxAge <= 0 {
		maxAge = c.cfg.AliveTimeout
	}
	deadline := time.Now().Add(timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if c.dev == nil {
			return nil, ErrClosed
		}
		if c.depthErr != nil {
			return nil, c.depthErr
		}
		if time.Since(c.depthAt) <= maxAge && len(c.ring) >= n {
			return append([]*DepthFrame(nil), c.ring[len(c.ring)-n:]...), nil
		}
		if !c.waitLocked(context.Background(), deadline) && time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: no recent depth frames within %v (%s)", ErrTimeout, timeout, c.errLocked())
		}
	}
}

// HasColor 表示 AliveTimeout 内收到过彩色帧。
// Stats 是流的累计统计：收到的帧数、因 RTP 丢包而丢弃的帧数，以及最近一帧深度的相机帧号
// （相机帧号的增量减去收到的帧数 = 相机发了但没收全的帧）。
type Stats struct {
	DepthFrames, DepthDropped int64
	ColorFrames, ColorDropped int64
	DepthDeviceSeq            uint64
}

// Stats 返回当前流统计；没打开时全零。
func (c *Camera) Stats() Stats {
	c.mu.Lock()
	ds, cs := c.depthStream, c.colorStream
	var st Stats
	if n := len(c.ring); n > 0 {
		st.DepthDeviceSeq = c.ring[n-1].DeviceSeq
	}
	c.mu.Unlock()
	if ds != nil {
		st.DepthFrames, st.DepthDropped = ds.Stats()
	}
	if cs != nil {
		st.ColorFrames, st.ColorDropped = cs.Stats()
	}
	return st
}

func (c *Camera) HasColor() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hasColorLocked()
}

func (c *Camera) hasColorLocked() bool {
	return c.color != nil && time.Since(c.colorAt) <= c.cfg.AliveTimeout
}

// GrabColor 返回最新的彩色帧，没有时最多等 timeout，仍没有则返回 ErrTimeout。
func (c *Camera) GrabColor(timeout time.Duration) (*ColorFrame, error) {
	deadline := time.Now().Add(timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dev == nil {
		return nil, ErrClosed
	}
	for !c.hasColorLocked() {
		if !c.waitLocked(context.Background(), deadline) && time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: no color frame within %v", ErrTimeout, timeout)
		}
		if c.dev == nil {
			return nil, ErrClosed
		}
	}
	return c.color, nil
}

// LatestColor 等到序号大于 newerThan 的彩色帧后返回，超时则返回当前最新帧；没有彩色帧时返回 nil。
func (c *Camera) LatestColor(newerThan uint64, timeout time.Duration) (*ColorFrame, uint64) {
	deadline := time.Now().Add(timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	for c.colorSeq <= newerThan && c.dev != nil {
		if !c.waitLocked(context.Background(), deadline) && time.Now().After(deadline) {
			break
		}
	}
	if !c.hasColorLocked() {
		return nil, c.colorSeq
	}
	return c.color, c.colorSeq
}

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
	// Color 为 nil 时用 1280x800@10，NoColor 为 true 时不开彩色流。
	Color   *StreamConfig
	NoColor bool

	DepthPrecision *DepthPrecision // 见 DeviceOptions.DepthPrecision

	RingSize     int           // 缓存的深度帧数，默认 8
	AliveTimeout time.Duration // 多久没有新帧算离线，默认 3 s
	OpenRetries  int           // 默认 5
	RetryWait    time.Duration // 默认 3 s
}

func (c *Config) withDefaults() Config {
	out := *c
	if out.Logger == nil {
		out.Logger = slog.Default()
	}
	if out.Depth.Width == 0 {
		out.Depth = StreamConfig{1280, 800, 10}
	}
	if out.Color == nil && !out.NoColor {
		out.Color = &StreamConfig{1280, 800, 10}
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

// Camera 持续接收深度和彩色并缓存最近几帧。离线后 IsAlive 返回 false，Close 再 Open 即可重连。
// 返回的帧是共享的，只读；要修改先 Clone。
type Camera struct {
	cfg Config
	log *slog.Logger

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
	color     *ColorFrame
	colorSeq  uint64
	colorAt   time.Time
	frameAt   time.Time
	notify    chan struct{} // 每来一帧关闭并换新
	streamErr error
}

// New 创建相机对象，不连接。
func New(cfg Config) *Camera {
	c := cfg.withDefaults()
	return &Camera{cfg: c, log: c.Logger, notify: make(chan struct{})}
}

// Open 连接相机并开始取流，失败时按 RetryWait 重试。
func (c *Camera) Open() error { return c.OpenContext(context.Background()) }

// OpenContext 同 Open，可取消。
func (c *Camera) OpenContext(ctx context.Context) error {
	var lastErr error
	for attempt := 1; attempt <= c.cfg.OpenRetries; attempt++ {
		if lastErr = c.openOnce(ctx); lastErr == nil {
			return nil
		}
		_ = c.Close()
		if attempt == c.cfg.OpenRetries || ctx.Err() != nil {
			break
		}
		c.log.Warn("open camera failed, retrying", "attempt", attempt, "err", lastErr, "wait", c.cfg.RetryWait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.cfg.RetryWait):
		}
	}
	return fmt.Errorf("open camera: %d attempts failed: %w", c.cfg.OpenRetries, lastErr)
}

func (c *Camera) openOnce(ctx context.Context) error {
	dev, err := OpenDevice(ctx, DeviceOptions{IP: c.cfg.IP, Logger: c.log, DepthPrecision: c.cfg.DepthPrecision,
		GVCPPort: c.cfg.GVCPPort, ControlPort: c.cfg.ControlPort, DepthRTSPPort: c.cfg.DepthRTSPPort, ColorRTSPPort: c.cfg.ColorRTSPPort})
	if err != nil {
		return err
	}
	cal := dev.Calibration()
	intr, err := cal.DepthIntrinsics(c.cfg.Depth.Width, c.cfg.Depth.Height)
	if err != nil {
		dev.Close()
		return err
	}
	var cp *ColorParams
	if c.cfg.Color != nil {
		ci, err1 := cal.ColorIntrinsics(c.cfg.Color.Width, c.cfg.Color.Height)
		ext, ok := cal.DepthToColor()
		if err1 == nil && ok {
			cp = &ColorParams{Intrinsics: ci, DepthToRGB: ext}
		}
	}

	c.mu.Lock()
	c.dev, c.intr, c.colorParams, c.info, c.baseline = dev, intr, cp, dev.Info(), cal.BaselineMM()
	c.ring, c.color, c.streamErr = nil, nil, nil
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
			c.mu.Lock()
			c.colorStream = cs
			c.mu.Unlock()
			go c.watch(cs, "color")
		}
	}

	if _, err := c.GrabDepth(5 * time.Second); err != nil {
		return err
	}
	return nil
}

// watch 记录流异常结束的原因。
func (c *Camera) watch(s *Stream, name string) {
	<-s.Done()
	if err := s.Err(); err != nil {
		c.log.Warn("stream stopped", "stream", name, "err", err)
		c.mu.Lock()
		if c.streamErr == nil {
			c.streamErr = fmt.Errorf("%s stream stopped: %w", name, err)
		}
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
	c.depthAt, c.frameAt = f.Time, f.Time
	c.broadcastLocked()
}

func (c *Camera) onColor(f *ColorFrame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.colorSeq++
	f.Seq = c.colorSeq
	c.color, c.colorAt, c.frameAt = f, f.Time, f.Time
	c.broadcastLocked()
}

// Close 关流并释放控制权，可重复调用。
func (c *Camera) Close() error {
	c.mu.Lock()
	ds, cs, dev := c.depthStream, c.colorStream, c.dev
	c.depthStream, c.colorStream, c.dev = nil, nil, nil
	c.ring, c.color = nil, nil
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
	return nil
}

// Intrinsics 返回深度流内参。
func (c *Camera) Intrinsics() Intrinsics {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.intr
}

// ColorParams 返回彩色内参和深度到彩色外参，没有彩色流时为 nil。
func (c *Camera) ColorParams() *ColorParams {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.colorParams
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

// IsAlive 表示 maxAge（0 用 AliveTimeout）内收到过帧。
func (c *Camera) IsAlive(maxAge time.Duration) bool {
	if maxAge <= 0 {
		maxAge = c.cfg.AliveTimeout
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dev != nil && time.Since(c.frameAt) <= maxAge
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
	if age := time.Since(c.frameAt); age > c.cfg.AliveTimeout {
		msg := fmt.Sprintf("no frames for %.0fs", age.Seconds())
		if c.streamErr != nil {
			msg += " (" + c.streamErr.Error() + ")"
		}
		return msg
	}
	return ""
}

var errClosed = errors.New("camera is not open")

// waitLocked 持锁调用，等下一帧或超时，返回时仍持锁。
func (c *Camera) waitLocked(deadline time.Time) bool {
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
	case <-t.C:
		return false
	}
}

// GrabDepth 返回下一帧新的深度图。
func (c *Camera) GrabDepth(timeout time.Duration) (*DepthFrame, error) {
	deadline := time.Now().Add(timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dev == nil {
		return nil, errClosed
	}
	seq := c.depthSeq
	for c.depthSeq == seq {
		if !c.waitLocked(deadline) && c.depthSeq == seq {
			return nil, fmt.Errorf("no new depth frame within %v (%s)", timeout, c.errLocked())
		}
		if c.dev == nil {
			return nil, errClosed
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

// RecentDepths 返回缓存里最近 n 帧（按时间先后）。最新一帧早于 maxAge 时等新帧，避免离线后用旧帧。
func (c *Camera) RecentDepths(n int, timeout, maxAge time.Duration) ([]*DepthFrame, error) {
	n = max(1, min(n, c.cfg.RingSize))
	deadline := time.Now().Add(timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if c.dev == nil {
			return nil, errClosed
		}
		if time.Since(c.depthAt) <= maxAge && len(c.ring) >= n {
			return append([]*DepthFrame(nil), c.ring[len(c.ring)-n:]...), nil
		}
		if !c.waitLocked(deadline) && time.Now().After(deadline) {
			return nil, fmt.Errorf("no recent depth frames within %v (%s)", timeout, c.errLocked())
		}
	}
}

// HasColor 表示 AliveTimeout 内收到过彩色帧。
func (c *Camera) HasColor() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hasColorLocked()
}

func (c *Camera) hasColorLocked() bool {
	return c.color != nil && time.Since(c.colorAt) <= c.cfg.AliveTimeout
}

// GrabColor 返回最新的彩色帧，没有时最多等 timeout，仍没有则返回 nil。
func (c *Camera) GrabColor(timeout time.Duration) (*ColorFrame, error) {
	deadline := time.Now().Add(timeout)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dev == nil {
		return nil, errClosed
	}
	for !c.hasColorLocked() {
		if !c.waitLocked(deadline) && time.Now().After(deadline) {
			return nil, nil
		}
		if c.dev == nil {
			return nil, errClosed
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
		if !c.waitLocked(deadline) && time.Now().After(deadline) {
			break
		}
	}
	if !c.hasColorLocked() {
		return nil, c.colorSeq
	}
	return c.color, c.colorSeq
}

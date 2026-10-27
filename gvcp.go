package orbbec

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// GVCP（GigE Vision 控制协议），字段大端。
const (
	gvcpPort    = 3956
	gvcpKeyCode = 0x42

	gvcpDiscoveryCmd = 0x0002
	gvcpDiscoveryAck = 0x0003
	gvcpReadRegCmd   = 0x0080
	gvcpReadRegAck   = 0x0081
	gvcpWriteRegCmd  = 0x0082
	gvcpWriteRegAck  = 0x0083

	regCapability       = 0x0934
	regHeartbeatTimeout = 0x0938
	regCCP              = 0x0A00

	ccpOpenAccess    = 0x00
	ccpControlAccess = 0x02
	ccpMask          = 0x07

	discoveryAckPayloadSize = 248
	ccpMinFirmware          = 10312 // 1.3.12
)

// GVCP 状态码。
const (
	GevStatusSuccess      uint16 = 0x0000
	GevStatusAccessDenied uint16 = 0x8006
	GevStatusError        uint16 = 0x8FFF
)

// ErrAccessDenied 表示控制权在别的程序手里。持有者异常退出时，相机在心跳超时后自动释放。
var ErrAccessDenied = errors.New("camera is controlled by another application (GVCP access denied)")

// DiscoveredDevice 是 GVCP 发现应答里的设备信息。
type DiscoveredDevice struct {
	IP           string
	Mask         string
	Gateway      string
	MAC          string
	Manufacturer string
	Model        string
	PID          uint16
	Firmware     string
	Serial       string
	UserName     string
	LocalIP      string // 收到应答的本机地址
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

func parseDiscoveryAck(b []byte) (DiscoveredDevice, error) {
	var d DiscoveredDevice
	if len(b) < 8 {
		return d, errors.New("short GVCP ack")
	}
	status := binary.BigEndian.Uint16(b[0:])
	ack := binary.BigEndian.Uint16(b[2:])
	if status != GevStatusSuccess || ack != gvcpDiscoveryAck {
		return d, fmt.Errorf("not a discovery ack: status=%#04x ack=%#04x", status, ack)
	}
	p := b[8:]
	if len(p) < discoveryAckPayloadSize {
		return d, fmt.Errorf("short discovery payload: %d bytes", len(p))
	}
	// specVer(4) devMode(4) mac[8] supIp(4) curIp(4) ip[16] mask[16] gw[16]
	// manufacturer[32] model[28] pid(4) version[32] info[48] serial[16] userName[16]
	mac := p[10:16]
	d.MAC = fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", mac[0], mac[1], mac[2], mac[3], mac[4], mac[5])
	ip4 := func(off int) string { return net.IP(p[off+12 : off+16]).String() }
	d.IP, d.Mask, d.Gateway = ip4(24), ip4(40), ip4(56)
	d.Manufacturer = cString(p[72:104])
	d.Model = cString(p[104:132])
	d.PID = uint16(binary.BigEndian.Uint32(p[132:136]))
	d.Firmware = cString(p[136:168])
	d.Serial = cString(p[216:232])
	d.UserName = cString(p[232:248])
	return d, nil
}

func discoveryCmd() []byte {
	b := make([]byte, 8)
	b[0] = gvcpKeyCode
	b[1] = 0x11 // 需要应答，允许广播应答
	binary.BigEndian.PutUint16(b[2:], gvcpDiscoveryCmd)
	binary.BigEndian.PutUint16(b[6:], 1)
	return b
}

// Discover 在每块 IPv4 网卡的子网上广播发现命令，返回 wait 内收到应答的设备。
func Discover(ctx context.Context, wait time.Duration) ([]DiscoveredDevice, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	type target struct{ local, bcast net.IP }
	var targets []target
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			ip := ipn.IP.To4()
			bc := make(net.IP, 4)
			for i := range 4 {
				bc[i] = ip[i] | ^ipn.Mask[len(ipn.Mask)-4+i]
			}
			targets = append(targets, target{ip, bc})
		}
	}

	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	var (
		mu    sync.Mutex
		found = map[string]DiscoveredDevice{}
		wg    sync.WaitGroup
	)
	for _, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: t.local})
			if err != nil {
				return
			}
			defer conn.Close()
			for _, dst := range []net.IP{t.bcast, net.IPv4bcast} {
				_, _ = conn.WriteToUDP(discoveryCmd(), &net.UDPAddr{IP: dst, Port: gvcpPort})
			}
			_ = conn.SetReadDeadline(deadline)
			buf := make([]byte, 1500)
			for {
				n, _, err := conn.ReadFromUDP(buf)
				if err != nil {
					return
				}
				d, err := parseDiscoveryAck(buf[:n])
				if err != nil {
					continue
				}
				d.LocalIP = t.local.String()
				mu.Lock()
				found[d.MAC+"/"+d.IP] = d
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	out := make([]DiscoveredDevice, 0, len(found))
	for _, d := range found {
		out = append(out, d)
	}
	return out, nil
}

// gvcpConn 是到一台相机的寄存器读写通道：每条命令等 200 ms，最多重发 3 次。
type gvcpConn struct {
	mu      sync.Mutex
	conn    *net.UDPConn
	dst     *net.UDPAddr
	reqID   uint16
	timeout time.Duration
	retries int
}

func dialGVCP(ip string, port int) (*gvcpConn, error) {
	dst := &net.UDPAddr{IP: net.ParseIP(ip), Port: port}
	if dst.IP == nil {
		return nil, fmt.Errorf("invalid camera IP %q", ip)
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, err
	}
	return &gvcpConn{conn: conn, dst: dst, timeout: 200 * time.Millisecond, retries: 3}, nil
}

func (g *gvcpConn) Close() error { return g.conn.Close() }

func (g *gvcpConn) nextReqID() uint16 {
	g.reqID++
	if g.reqID == 0 {
		g.reqID = 1
	}
	return g.reqID
}

// transact 发一条命令，返回匹配 ackCode 和 reqID 的应答状态与负载。
func (g *gvcpConn) transact(cmd []byte, ackCode, reqID uint16) (uint16, []byte, error) {
	buf := make([]byte, 1500)
	_ = g.conn.SetReadDeadline(time.Now())
	for {
		if _, _, err := g.conn.ReadFromUDP(buf); err != nil {
			break
		}
	}
	var lastErr error
	for range g.retries + 1 {
		if _, err := g.conn.WriteToUDP(cmd, g.dst); err != nil {
			return GevStatusError, nil, err
		}
		_ = g.conn.SetReadDeadline(time.Now().Add(g.timeout))
		for {
			n, from, err := g.conn.ReadFromUDP(buf)
			if err != nil {
				lastErr = err
				break
			}
			if !from.IP.Equal(g.dst.IP) || n < 8 {
				continue
			}
			status := binary.BigEndian.Uint16(buf[0:])
			ack := binary.BigEndian.Uint16(buf[2:])
			length := binary.BigEndian.Uint16(buf[4:])
			id := binary.BigEndian.Uint16(buf[6:])
			if ack != ackCode || id != reqID {
				continue
			}
			return status, append([]byte(nil), buf[8:min(8+int(length), n)]...), nil
		}
	}
	return GevStatusError, nil, fmt.Errorf("no GVCP response from %s: %w", g.dst.IP, lastErr)
}

func (g *gvcpConn) ReadRegister(addr uint32) (uint16, uint32, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id := g.nextReqID()
	cmd := make([]byte, 12)
	cmd[0], cmd[1] = gvcpKeyCode, 0x01
	binary.BigEndian.PutUint16(cmd[2:], gvcpReadRegCmd)
	binary.BigEndian.PutUint16(cmd[4:], 4)
	binary.BigEndian.PutUint16(cmd[6:], id)
	binary.BigEndian.PutUint32(cmd[8:], addr)
	status, p, err := g.transact(cmd, gvcpReadRegAck, id)
	if err != nil || status != GevStatusSuccess {
		return status, 0, err
	}
	if len(p) < 4 {
		return GevStatusError, 0, fmt.Errorf("register %#04x: short ack", addr)
	}
	return status, binary.BigEndian.Uint32(p), nil
}

func (g *gvcpConn) WriteRegister(addr, value uint32) (uint16, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id := g.nextReqID()
	cmd := make([]byte, 16)
	cmd[0], cmd[1] = gvcpKeyCode, 0x01
	binary.BigEndian.PutUint16(cmd[2:], gvcpWriteRegCmd)
	binary.BigEndian.PutUint16(cmd[4:], 8)
	binary.BigEndian.PutUint16(cmd[6:], id)
	binary.BigEndian.PutUint32(cmd[8:], addr)
	binary.BigEndian.PutUint32(cmd[12:], value)
	status, _, err := g.transact(cmd, gvcpWriteRegAck, id)
	return status, err
}

// Discover 单播发现，拿固件版本、型号和序列号。
func (g *gvcpConn) Discover() (DiscoveredDevice, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	status, p, err := g.transact(discoveryCmd(), gvcpDiscoveryAck, 1)
	if err != nil {
		return DiscoveredDevice{}, err
	}
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint16(hdr[0:], status)
	binary.BigEndian.PutUint16(hdr[2:], gvcpDiscoveryAck)
	return parseDiscoveryAck(append(hdr, p...))
}

// firmwareVersionInt 把 "1.3.19" 转成 10319，解析失败返回 0。
func firmwareVersionInt(v string) int {
	v = strings.TrimSpace(v)
	start := strings.IndexFunc(v, func(r rune) bool { return r >= '0' && r <= '9' })
	if start < 0 {
		return 0
	}
	parts := strings.SplitN(v[start:], ".", 4)
	if len(parts) < 3 {
		return 0
	}
	total := 0
	for i, mul := range []int{10000, 100, 1} {
		digits := strings.TrimRightFunc(parts[i], func(r rune) bool { return r < '0' || r > '9' })
		n, err := strconv.Atoi(digits)
		if err != nil || n >= 100 {
			return 0
		}
		total += n * mul
	}
	return total
}

// ccpController 持有控制权并保活。
type ccpController struct {
	g      *gvcpConn
	log    *slog.Logger
	stop   chan struct{}
	done   chan struct{}
	serial string
	lost   atomic.Bool
}

// acquireCCP 申请控制权并开始保活。相机不支持 CCP 时返回 nil, nil。
func acquireCCP(g *gvcpConn, fwVersion int, serial string, log *slog.Logger) (*ccpController, error) {
	if fwVersion > 0 && fwVersion < ccpMinFirmware {
		log.Warn("firmware does not support CCP", "firmware", fwVersion)
		return nil, nil
	}
	status, _, err := g.ReadRegister(regCapability)
	if err != nil || (status != GevStatusSuccess && status != GevStatusAccessDenied) {
		log.Warn("CCP not supported", "err", err, "status", fmt.Sprintf("%#04x", status))
		return nil, nil
	}
	status, err = g.WriteRegister(regCCP, ccpControlAccess)
	if err != nil {
		return nil, fmt.Errorf("acquire control: %w", err)
	}
	switch status {
	case GevStatusSuccess:
	case GevStatusAccessDenied:
		return nil, ErrAccessDenied
	default:
		return nil, fmt.Errorf("acquire control: GVCP status %#04x", status)
	}

	interval := time.Second
	if st, v, err := g.ReadRegister(regHeartbeatTimeout); err == nil && st == GevStatusSuccess {
		interval = max(time.Duration(v/3)*time.Millisecond, 500*time.Millisecond)
	}
	c := &ccpController{g: g, log: log, stop: make(chan struct{}), done: make(chan struct{}), serial: serial}
	go c.keepalive(interval)
	return c, nil
}

// keepalive 每个周期写一次 CCP=2。读寄存器分不出持有者，只有写能确认控制权还在：
// 写成功说明仍持有（相机偶尔会把 CCP 清零，这时等于重新申请），ACCESS_DENIED 说明被别的程序拿走了。
func (c *ccpController) keepalive(interval time.Duration) {
	defer close(c.done)
	t := time.NewTicker(interval)
	defer t.Stop()
	var lastWarn time.Time
	warn := func(msg string, args ...any) {
		if time.Since(lastWarn) > 3*time.Second {
			lastWarn = time.Now()
			c.log.Warn(msg, append([]any{"serial", c.serial}, args...)...)
		}
	}
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
		}
		if st, v, err := c.g.ReadRegister(regCCP); err == nil && st == GevStatusSuccess && v&ccpMask == ccpOpenAccess {
			c.log.Info("camera released control, reacquiring", "serial", c.serial)
		}
		st, err := c.g.WriteRegister(regCCP, ccpControlAccess)
		switch {
		case err == nil && st == GevStatusSuccess:
			if c.lost.Swap(false) {
				c.log.Info("control reacquired", "serial", c.serial)
			}
		case st == GevStatusAccessDenied:
			c.lost.Store(true)
			warn("control taken by another application")
		default:
			warn("CCP keepalive failed", "err", err, "status", fmt.Sprintf("%#04x", st))
		}
	}
}

// release 停止保活并把 CCP 恢复成开放访问。
func (c *ccpController) release() {
	if c == nil {
		return
	}
	close(c.stop)
	<-c.done
	if status, err := c.g.WriteRegister(regCCP, ccpOpenAccess); err != nil || status != GevStatusSuccess {
		c.log.Debug("release control failed", "err", err, "status", fmt.Sprintf("%#04x", status))
	}
}

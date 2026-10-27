package orbbec

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

// 控制协议（TCP 8090），字段小端。
const (
	vendorPort = 8090

	hpRequestMagic  = 0x4d47
	hpResponseMagic = 0x4252

	opGetProperty          = 1
	opSetProperty          = 2
	opGetStructureData     = 3
	opInitReadStructList   = 29
	opReadStructList       = 30
	opFinishReadStructList = 31

	reqHeaderSize  = 8  // magic, sizeInHalfWords, opcode, requestId
	respHeaderSize = 10 // + errorCode

	listChunkSize   = 768
	hpRespErrorBusy = 0x0003

	// 435Le 把每条响应补零到固定长度，头里的长度只算有效数据。
	vendorRespPad = 1022
	drainMaxBytes = 64 << 10
)

// DeviceError 是相机对命令返回的错误码。
type DeviceError struct {
	PropertyID uint32
	Opcode     uint16
	Code       uint16
	Msg        string
}

func (e *DeviceError) Error() string {
	s := fmt.Sprintf("device error %#04x (property %d, opcode %d)", e.Code, e.PropertyID, e.Opcode)
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	return s
}

// PropertyRange 是属性的当前值和范围，布尔和浮点属性也按 int32 传输。
type PropertyRange struct {
	Cur, Max, Min, Def, Step int32
}

// vendorClient 是控制协议客户端，并发安全，同一时刻只有一条命令在途。
type vendorClient struct {
	mu      sync.Mutex
	addr    string
	conn    net.Conn
	closed  bool
	reqID   uint16
	timeout time.Duration
	buf     []byte
	// padTo：响应补齐到的长度，0 = 还没确认（第一条响应时按 vendorRespPad 试读），-1 = 不补齐。
	padTo int
}

func dialVendor(ctx context.Context, ip string, port int, timeout time.Duration) (*vendorClient, error) {
	c := &vendorClient{
		addr:    net.JoinHostPort(ip, fmt.Sprint(port)),
		reqID:   uint16(rand.Uint32()),
		timeout: timeout,
		buf:     make([]byte, 4096),
	}
	if err := c.connect(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *vendorClient) connect(ctx context.Context) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("connect control port %s: %w", c.addr, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
		_ = tc.SetNoDelay(true)
	}
	c.conn = conn
	c.padTo = 0
	return nil
}

// Close 断开并拒绝之后的命令。
func (c *vendorClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *vendorClient) request(op uint16, payload []byte) ([]byte, uint16) {
	c.reqID++
	n := len(payload)
	req := make([]byte, reqHeaderSize+n+n%2)
	binary.LittleEndian.PutUint16(req[0:], hpRequestMagic)
	binary.LittleEndian.PutUint16(req[2:], uint16((n+1)/2))
	binary.LittleEndian.PutUint16(req[4:], op)
	binary.LittleEndian.PutUint16(req[6:], c.reqID)
	copy(req[reqHeaderSize:], payload)
	return req, c.reqID
}

// roundTrip 发一条命令，返回响应数据（不含响应头）。错位或相机忙时重试一次。
func (c *vendorClient) roundTrip(op uint16, payload []byte, propID uint32) ([]byte, error) {
	if c.closed {
		return nil, ErrClosed
	}
	if c.conn == nil {
		if err := c.connect(context.Background()); err != nil {
			return nil, err
		}
	}
	var lastErr error
	for attempt := range 2 {
		if attempt > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		req, id := c.request(op, payload)
		deadline := time.Now().Add(c.timeout)
		_ = c.conn.SetDeadline(deadline)
		if _, err := c.conn.Write(req); err != nil {
			return nil, c.ioFailed(err)
		}
		resp, err := c.readResponse(deadline)
		if err != nil {
			return nil, c.ioFailed(err)
		}
		magic := binary.LittleEndian.Uint16(resp[0:])
		rop := binary.LittleEndian.Uint16(resp[4:])
		rid := binary.LittleEndian.Uint16(resp[6:])
		code := binary.LittleEndian.Uint16(resp[8:])
		data := resp[respHeaderSize:]
		switch {
		case magic != hpResponseMagic:
			lastErr = fmt.Errorf("bad response magic %#04x", magic)
			c.drain(deadline)
			continue
		case rid != id:
			lastErr = fmt.Errorf("response id %d does not match request %d", rid, id)
			c.drain(deadline)
			continue
		case rop != op:
			return nil, fmt.Errorf("response opcode %d does not match request %d", rop, op)
		case code == hpRespErrorBusy:
			lastErr = &DeviceError{PropertyID: propID, Opcode: op, Code: code, Msg: "device busy"}
			continue
		case code >= 0x8000 && code != 0xffff:
			return data, nil // 警告不算失败
		case code != 0:
			return nil, &DeviceError{PropertyID: propID, Opcode: op, Code: code, Msg: cString(data)}
		}
		return data, nil
	}
	return nil, lastErr
}

// drain 丢掉连接里残留的字节重新对齐：读到连接空闲 100 ms、超过本条命令的期限或 drainMaxBytes 为止；
// 还没对齐就断开，下一条命令重连。
func (c *vendorClient) drain(deadline time.Time) {
	total := 0
	for total < drainMaxBytes {
		d := time.Now().Add(100 * time.Millisecond)
		if deadline.Before(d) {
			d = deadline
		}
		_ = c.conn.SetReadDeadline(d)
		n, err := c.conn.Read(c.buf)
		total += n
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() && time.Now().Before(deadline) {
				return
			}
			break
		}
	}
	_ = c.ioFailed(errors.New("control channel out of sync"))
}

// ioFailed 断开连接，下一条命令自动重连。
func (c *vendorClient) ioFailed(err error) error {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	return fmt.Errorf("control channel: %w", err)
}

// readResponse 读一条响应并跳过补零。
func (c *vendorClient) readResponse(deadline time.Time) ([]byte, error) {
	if _, err := io.ReadFull(c.conn, c.buf[:reqHeaderSize]); err != nil {
		return nil, err
	}
	total := reqHeaderSize + int(binary.LittleEndian.Uint16(c.buf[2:]))*2
	if total < respHeaderSize || total > len(c.buf) {
		return nil, fmt.Errorf("invalid response length %d", total)
	}
	if _, err := io.ReadFull(c.conn, c.buf[reqHeaderSize:total]); err != nil {
		return nil, err
	}
	resp := append([]byte(nil), c.buf[:total]...)
	if err := c.skipPadding(total, deadline); err != nil {
		return nil, err
	}
	return resp, nil
}

// skipPadding 读掉补零。第一条响应时按 vendorRespPad 试读：在本条命令的期限内读不到补零就记为不补齐。
func (c *vendorClient) skipPadding(got int, deadline time.Time) error {
	if c.padTo < 0 || got >= vendorRespPad {
		return nil
	}
	want := vendorRespPad - got
	if c.padTo > 0 {
		_, err := io.ReadFull(c.conn, c.buf[:want])
		return err
	}
	n := 0
	for n < want {
		_ = c.conn.SetReadDeadline(deadline)
		k, err := c.conn.Read(c.buf[n:want])
		n += k
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() && n == 0 {
				c.padTo = -1
				return nil
			}
			return err
		}
	}
	c.padTo = vendorRespPad
	return nil
}

func u32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func (c *vendorClient) GetPropertyRange(id uint32) (PropertyRange, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := c.roundTrip(opGetProperty, u32(id), id)
	if err != nil {
		return PropertyRange{}, err
	}
	if len(data) < 20 {
		return PropertyRange{}, fmt.Errorf("property %d: short response (%d bytes)", id, len(data))
	}
	v := func(i int) int32 { return int32(binary.LittleEndian.Uint32(data[i*4:])) }
	return PropertyRange{Cur: v(0), Max: v(1), Min: v(2), Def: v(3), Step: v(4)}, nil
}

func (c *vendorClient) GetProperty(id uint32) (int32, error) {
	r, err := c.GetPropertyRange(id)
	return r.Cur, err
}

func (c *vendorClient) SetProperty(id uint32, value int32) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	payload := binary.LittleEndian.AppendUint32(u32(id), uint32(value))
	_, err := c.roundTrip(opSetProperty, payload, id)
	return err
}

// GetStructure 读一个结构体属性（单次往返）。
func (c *vendorClient) GetStructure(id uint32) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := c.roundTrip(opGetStructureData, u32(id), id)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("structure %d: empty response", id)
	}
	return data, nil
}

// GetStructList 分包读结构体列表：init（总长、命令版本）→ 按 768 字节读 → finish。
func (c *vendorClient) GetStructList(id uint32, cmdVer uint16) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := c.roundTrip(opInitReadStructList, u32(id), id)
	if err != nil {
		return nil, err
	}
	if len(data) < 6 {
		return nil, fmt.Errorf("struct list %d: short init response", id)
	}
	if v := binary.LittleEndian.Uint16(data); v != cmdVer {
		return nil, fmt.Errorf("struct list %d: command version %d, want %d", id, v, cmdVer)
	}
	size := binary.LittleEndian.Uint32(data[2:])
	if size > 16<<20 {
		return nil, fmt.Errorf("struct list %d: invalid size %d", id, size)
	}
	out := make([]byte, 0, size)
	for off := uint32(0); off < size; off += listChunkSize {
		n := min(listChunkSize, size-off)
		payload := binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(u32(id), off), n)
		chunk, err := c.roundTrip(opReadStructList, payload, id)
		if err == nil && uint32(len(chunk)) < n {
			err = fmt.Errorf("struct list %d: want %d bytes at offset %d, got %d", id, n, off, len(chunk))
		}
		if err != nil {
			_, _ = c.roundTrip(opFinishReadStructList, u32(id), id)
			return nil, err
		}
		out = append(out, chunk[:n]...)
	}
	_, _ = c.roundTrip(opFinishReadStructList, u32(id), id)
	return out, nil
}

package orbbec

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fake435 是测试用的模拟 435Le：GVCP（UDP）、控制协议（TCP）、深度/彩色 RTSP（TCP）都在 127.0.0.1 的随机端口上，
// 行为照真机抓包：控制协议每条响应补零到 1022 字节；深度 SDP 写 m=VIDEO、编码 OB_FMT_Y16；
// 帧头宽高为 0、元数据 108 字节、时间戳单位 ms；彩色是 RFC 2435（负载类型 26，无 rtpmap）。
type fake435 struct {
	t *testing.T

	gvcp       *net.UDPConn
	ctrl       net.Listener
	depthL     net.Listener
	colorL     net.Listener
	padTo      int // 控制协议响应补齐长度，0 = 不补齐
	mu         sync.Mutex
	ccp        uint32
	ccpOwner   string
	props      map[uint32]PropertyRange
	setLog     []string
	structs    map[uint32][]byte
	lists      map[uint32]fakeList
	depthValue uint16 // 深度原始值（乘单位得 mm）
	fps        int
	paused     atomic.Bool
	rtcpSeen   atomic.Int64
	teardowns  atomic.Int64
	jpegScan   []byte // 彩色帧的扫描数据（不含 JPEG 头）
	jpegQ      uint8
	colorW     int
	colorH     int
	conns      sync.WaitGroup
	closed     chan struct{}
	activeConn sync.Map      // net.Conn -> 类型（"ctrl"/"depth"/"color"）
	padDelay   time.Duration // 控制协议：有效数据和补零之间的延迟（模拟 TCP 分包）
	garbage    atomic.Bool   // 控制协议：收到请求后回错误魔数并持续吐字节
	noGVCPRead atomic.Bool   // GVCP：不回应寄存器读
}

type fakeList struct {
	ver  uint16
	data []byte
}

func newFake435(t *testing.T) *fake435 {
	t.Helper()
	f := &fake435{t: t, padTo: 1022, fps: 20, depthValue: 6400, closed: make(chan struct{}),
		props: map[uint32]PropertyRange{
			PropDepthPrecisionLevelInt:     {Cur: 4, Max: 4, Min: 0, Def: 0, Step: 1},
			PropDisparityToDepthBool:       {Cur: 1, Max: 4, Min: 0, Def: 0, Step: 1},
			PropDeviceCommunicationTypeInt: {},
			PropCompatVersionInt:           {Cur: 0, Max: 268435455, Min: 1, Def: 1, Step: 1},
			PropDevicePIDInt:               {Cur: 0x0815, Max: 0x0815, Min: 0x0815, Def: 0x0815, Step: 0x0815},
			PropStopDepthStreamBool:        {Max: 1, Step: 1},
			PropStopColorStreamBool:        {Max: 1, Step: 1},
			PropDepthAlignHardwareBool:     {Max: 1, Step: 1},
		},
		structs: map[uint32][]byte{StructVersion: fakeVersionInfo()},
		lists: map[uint32]fakeList{
			RawDataDepthCalibParam:            {1, fixtureDepthCalib()},
			RawDataAlignCalibParam:            {0, fixtureAlignCalib()},
			RawDataD2CAlignSupportProfileList: {0, fixtureD2CList()},
			RawDataEffectiveStreamProfileList: {0, fixtureEffectiveProfiles()},
		},
	}
	var err error
	if f.gvcp, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}); err != nil {
		t.Fatal(err)
	}
	for _, l := range []*net.Listener{&f.ctrl, &f.depthL, &f.colorL} {
		if *l, err = net.Listen("tcp4", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
	}
	f.makeJPEG(64, 40, 80)
	go f.serveGVCP()
	go f.accept(f.ctrl, "ctrl", f.serveCtrl)
	go f.accept(f.depthL, "depth", func(c net.Conn) { f.serveRTSP(c, true) })
	go f.accept(f.colorL, "color", func(c net.Conn) { f.serveRTSP(c, false) })
	t.Cleanup(f.Close)
	return f
}

func (f *fake435) Close() {
	select {
	case <-f.closed:
		return
	default:
	}
	close(f.closed)
	f.gvcp.Close()
	f.ctrl.Close()
	f.depthL.Close()
	f.colorL.Close()
	f.activeConn.Range(func(k, _ any) bool { k.(net.Conn).Close(); return true })
	f.conns.Wait()
}

// dropConns 断开某一类连接（模拟单路流断线）。
func (f *fake435) dropConns(kind string) {
	f.activeConn.Range(func(k, v any) bool {
		if v == kind {
			k.(net.Conn).Close()
		}
		return true
	})
}

func port(a net.Addr) int {
	_, p, _ := net.SplitHostPort(a.String())
	n, _ := strconv.Atoi(p)
	return n
}

// options 是连这台模拟相机用的 DeviceOptions。
func (f *fake435) options() DeviceOptions {
	return DeviceOptions{IP: "127.0.0.1", GVCPPort: port(f.gvcp.LocalAddr()), ControlPort: port(f.ctrl.Addr()),
		DepthRTSPPort: port(f.depthL.Addr()), ColorRTSPPort: port(f.colorL.Addr())}
}

func (f *fake435) accept(l net.Listener, kind string, serve func(net.Conn)) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		f.conns.Add(1)
		f.activeConn.Store(c, kind)
		go func() {
			defer f.conns.Done()
			defer f.activeConn.Delete(c)
			defer c.Close()
			serve(c)
		}()
	}
}

// ---------------------------------------------------------------- GVCP

func (f *fake435) serveGVCP() {
	buf := make([]byte, 1500)
	for {
		n, from, err := f.gvcp.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 8 || buf[0] != gvcpKeyCode {
			continue
		}
		cmd := binary.BigEndian.Uint16(buf[2:])
		id := binary.BigEndian.Uint16(buf[6:])
		ack := func(status, code uint16, payload []byte) {
			out := make([]byte, 8, 8+len(payload))
			binary.BigEndian.PutUint16(out[0:], status)
			binary.BigEndian.PutUint16(out[2:], code)
			binary.BigEndian.PutUint16(out[4:], uint16(len(payload)))
			binary.BigEndian.PutUint16(out[6:], id)
			_, _ = f.gvcp.WriteToUDP(append(out, payload...), from)
		}
		switch cmd {
		case gvcpDiscoveryCmd:
			ack(GevStatusSuccess, gvcpDiscoveryAck, fakeDiscoveryPayload())
		case gvcpReadRegCmd:
			if f.noGVCPRead.Load() {
				continue
			}
			addr := binary.BigEndian.Uint32(buf[8:])
			var v uint32
			switch addr {
			case regCapability:
				v = 0x14
			case regHeartbeatTimeout:
				v = 6000
			case regCCP:
				f.mu.Lock()
				v = f.ccp
				f.mu.Unlock()
			}
			ack(GevStatusSuccess, gvcpReadRegAck, binary.BigEndian.AppendUint32(nil, v))
		case gvcpWriteRegCmd:
			addr := binary.BigEndian.Uint32(buf[8:])
			v := binary.BigEndian.Uint32(buf[12:])
			status := GevStatusSuccess
			if addr == regCCP {
				f.mu.Lock()
				switch {
				case f.ccp != 0 && f.ccpOwner != from.String():
					status = GevStatusAccessDenied
				default:
					f.ccp = v
					f.ccpOwner = from.String()
				}
				f.mu.Unlock()
			}
			ack(status, gvcpWriteRegAck, []byte{0, 0, 0, 1})
		}
	}
}

// clearCCP 模拟相机自己把控制权清回开放状态（真机偶发）。
func (f *fake435) clearCCP() {
	f.mu.Lock()
	f.ccp, f.ccpOwner = 0, ""
	f.mu.Unlock()
}

func (f *fake435) ccpValue() uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ccp
}

func fakeDiscoveryPayload() []byte {
	p := make([]byte, discoveryAckPayloadSize)
	copy(p[10:16], []byte{0x54, 0x14, 0xFD, 0x25, 0xA4, 0x44})
	copy(p[24+12:], net.IPv4(127, 0, 0, 1).To4())
	copy(p[40+12:], net.IPv4(255, 0, 0, 0).To4())
	copy(p[72:], "Orbbec")
	copy(p[104:], "Gemini 435Le")
	binary.BigEndian.PutUint32(p[132:], 0x0815)
	copy(p[136:], "1.3.19")
	copy(p[216:], "FAKE435LE001")
	return p
}

func fakeVersionInfo() []byte {
	b := make([]byte, 164)
	for i, s := range []string{"1.3.19", "V1.1", "V2.4.2", "ESGM", "AR9341", "FAKE435LE001"} {
		copy(b[i*16:], s)
	}
	binary.LittleEndian.PutUint32(b[96:], 1)
	copy(b[100:], "Orbbec Gemini435")
	copy(b[116:], "1.0.9")
	return b
}

// ---------------------------------------------------------------- 控制协议

func (f *fake435) serveCtrl(c net.Conn) {
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(c, hdr); err != nil {
			return
		}
		if binary.LittleEndian.Uint16(hdr) != hpRequestMagic {
			return
		}
		payload := make([]byte, int(binary.LittleEndian.Uint16(hdr[2:]))*2)
		if _, err := io.ReadFull(c, payload); err != nil {
			return
		}
		op := binary.LittleEndian.Uint16(hdr[4:])
		id := binary.LittleEndian.Uint16(hdr[6:])
		if f.garbage.Load() {
			_, _ = c.Write([]byte{0, 0, 4, 0, 0, 0, 0, 0})
			for {
				if _, err := c.Write([]byte{0}); err != nil {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
		code, data := f.handleCtrl(op, payload)
		resp := make([]byte, 10, 10+len(data))
		binary.LittleEndian.PutUint16(resp[0:], hpResponseMagic)
		binary.LittleEndian.PutUint16(resp[2:], uint16((2+len(data)+1)/2))
		binary.LittleEndian.PutUint16(resp[4:], op)
		binary.LittleEndian.PutUint16(resp[6:], id)
		binary.LittleEndian.PutUint16(resp[8:], code)
		resp = append(resp, data...)
		if len(data)%2 == 1 {
			resp = append(resp, 0)
		}
		if f.padTo > len(resp) {
			if f.padDelay > 0 {
				if _, err := c.Write(resp); err != nil {
					return
				}
				time.Sleep(f.padDelay)
				resp = make([]byte, f.padTo-len(resp))
			} else {
				resp = append(resp, make([]byte, f.padTo-len(resp))...)
			}
		}
		if _, err := c.Write(resp); err != nil {
			return
		}
	}
}

func (f *fake435) handleCtrl(op uint16, p []byte) (uint16, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := binary.LittleEndian.Uint32(p)
	switch op {
	case opGetProperty:
		r, ok := f.props[id]
		if !ok {
			return 0x0002, nil
		}
		var out []byte
		for _, v := range []int32{r.Cur, r.Max, r.Min, r.Def, r.Step} {
			out = binary.LittleEndian.AppendUint32(out, uint32(v))
		}
		return 0, out
	case opSetProperty:
		r, ok := f.props[id]
		if !ok {
			return 0x0002, nil
		}
		r.Cur = int32(binary.LittleEndian.Uint32(p[4:]))
		f.props[id] = r
		f.setLog = append(f.setLog, fmt.Sprintf("%d=%d", id, r.Cur))
		return 0, nil
	case opGetStructureData:
		if b, ok := f.structs[id]; ok {
			return 0, b
		}
		return 0x0002, nil
	case opInitReadStructList:
		l, ok := f.lists[id]
		if !ok {
			return 0x0002, nil
		}
		out := binary.LittleEndian.AppendUint16(nil, l.ver)
		return 0, binary.LittleEndian.AppendUint32(out, uint32(len(l.data)))
	case opReadStructList:
		l := f.lists[id]
		off := int(binary.LittleEndian.Uint32(p[4:]))
		n := int(binary.LittleEndian.Uint32(p[8:]))
		end := min(off+n, len(l.data))
		return 0, l.data[off:end]
	case opFinishReadStructList:
		return 0, nil
	}
	return 0x0002, nil
}

func (f *fake435) prop(id uint32) PropertyRange {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.props[id]
}

// ---------------------------------------------------------------- RTSP

func (f *fake435) serveRTSP(c net.Conn, depth bool) {
	br := bufio.NewReader(c)
	var wmu sync.Mutex
	write := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_, err := c.Write(b)
		return err
	}
	streaming := make(chan struct{})
	started := false
	defer func() {
		if !started {
			close(streaming)
		}
	}()
	for {
		first, err := br.Peek(1)
		if err != nil {
			return
		}
		if first[0] == '$' {
			h := make([]byte, 4)
			if _, err := io.ReadFull(br, h); err != nil {
				return
			}
			body := make([]byte, binary.BigEndian.Uint16(h[2:]))
			if _, err := io.ReadFull(br, body); err != nil {
				return
			}
			if h[1] == 1 && len(body) >= 2 && body[1] == 201 {
				f.rtcpSeen.Add(1)
			}
			continue
		}
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		method, uri, _ := strings.Cut(strings.TrimSpace(line), " ")
		uri, _, _ = strings.Cut(uri, " ")
		hdrs := map[string]string{}
		for {
			h, err := br.ReadString('\n')
			if err != nil {
				return
			}
			h = strings.TrimSpace(h)
			if h == "" {
				break
			}
			k, v, _ := strings.Cut(h, ":")
			hdrs[strings.ToLower(k)] = strings.TrimSpace(v)
		}
		cseq := hdrs["cseq"]
		reply := func(extra, body string) error {
			msg := "RTSP/1.0 200 OK\r\nCSeq: " + cseq + "\r\n" + extra
			if body != "" {
				msg += fmt.Sprintf("Content-Length: %d\r\n", len(body))
			}
			return write([]byte(msg + "\r\n" + body))
		}
		switch method {
		case "DESCRIBE":
			var sdp string
			if depth {
				sdp = "v=0\r\na=control:*\r\nm=VIDEO 0 RTP/AVP 97\r\nc=IN IP4 0.0.0.0\r\na=rtpmap:97 OB_FMT_Y16/90000\r\na=fmtp:97;format=Y16;\r\na=control:track1\r\n"
			} else {
				sdp = "v=0\r\na=control:*\r\nm=video 0 RTP/AVP 26\r\nc=IN IP4 0.0.0.0\r\na=control:track1\r\n"
			}
			_ = reply("Content-Base: "+uri+"/\r\nContent-Type: application/sdp\r\n", sdp)
		case "SETUP":
			if !strings.HasSuffix(uri, "/track1") {
				_ = write([]byte("RTSP/1.0 404 Not Found\r\nCSeq: " + cseq + "\r\n\r\n"))
				continue
			}
			_ = reply("Transport: RTP/AVP/TCP;unicast;interleaved=0-1\r\nSession: FAKE01;timeout=65\r\n", "")
		case "PLAY":
			_ = reply("Session: FAKE01\r\n", "")
			if !started {
				started = true
				go func() {
					defer close(streaming)
					if depth {
						f.streamDepth(write)
					} else {
						f.streamColor(write)
					}
				}()
			}
		case "TEARDOWN":
			f.teardowns.Add(1)
			_ = reply("", "")
			c.Close()
			<-streaming
			return
		}
	}
}

// sendRTP 把一帧拆成 RTP 包，用 '$' 交织格式发出去。
func sendRTP(write func([]byte) error, pt uint8, seq *uint16, ts uint32, payloads [][]byte) error {
	for i, pl := range payloads {
		pkt := make([]byte, 4+12+len(pl))
		pkt[0], pkt[1] = '$', 0
		binary.BigEndian.PutUint16(pkt[2:], uint16(12+len(pl)))
		r := pkt[4:]
		r[0] = 0x80
		r[1] = pt
		if i == len(payloads)-1 {
			r[1] |= 0x80
		}
		binary.BigEndian.PutUint16(r[2:], *seq)
		binary.BigEndian.PutUint32(r[4:], ts)
		binary.BigEndian.PutUint32(r[8:], 0x12345678)
		copy(r[12:], pl)
		*seq++
		if err := write(pkt); err != nil {
			return err
		}
	}
	return nil
}

func (f *fake435) streamDepth(write func([]byte) error) {
	const w, h = 1280, 800
	var seq uint16
	tick := time.NewTicker(time.Second / time.Duration(f.fps))
	defer tick.Stop()
	for n := uint64(1); ; n++ {
		select {
		case <-f.closed:
			return
		case <-tick.C:
		}
		if f.paused.Load() {
			continue
		}
		frame := make([]byte, vendorFrameHeaderSize+108+w*h*2)
		binary.LittleEndian.PutUint64(frame[0:], n)
		binary.LittleEndian.PutUint16(frame[8:], 108)
		binary.LittleEndian.PutUint64(frame[10:], 700000+n*50) // ms
		copy(frame[vendorFrameHeaderSize:], "ORBBEC")
		binary.LittleEndian.PutUint32(frame[vendorFrameHeaderSize+12:], uint32(n))
		pix := frame[vendorFrameHeaderSize+108:]
		for i := range w * h {
			v := f.depthValue
			if i%w == 0 {
				v = 0 // 每行第一个像素无效
			}
			binary.LittleEndian.PutUint16(pix[i*2:], v)
		}
		var payloads [][]byte
		for off := 0; off < len(frame); off += 1400 {
			payloads = append(payloads, frame[off:min(off+1400, len(frame))])
		}
		if sendRTP(write, 97, &seq, uint32(n*9000), payloads) != nil {
			return
		}
	}
}

func (f *fake435) streamColor(write func([]byte) error) {
	var seq uint16
	tick := time.NewTicker(time.Second / time.Duration(f.fps))
	defer tick.Stop()
	for n := uint32(1); ; n++ {
		select {
		case <-f.closed:
			return
		case <-tick.C:
		}
		if f.paused.Load() {
			continue
		}
		if sendRTP(write, 26, &seq, n*9000, rfc2435Packets(f.jpegScan, 1, f.jpegQ, f.colorW, f.colorH, 1000)) != nil {
			return
		}
	}
}

// makeJPEG 用标准库编一张 4:2:0 的 JPEG，取出扫描数据当 RFC 2435 负载。
func (f *fake435) makeJPEG(w, h, q int) {
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	for y := range h {
		for x := range w {
			img.Y[y*img.YStride+x] = uint8(x * 255 / w)
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		f.t.Fatal(err)
	}
	f.jpegScan, f.jpegQ, f.colorW, f.colorH = jpegScanData(f.t, buf.Bytes()), uint8(q), w, h // 宽高只用来写 RFC 2435 头
}

// jpegScanData 取 SOS 段之后到 EOI 之前的熵编码数据。
func jpegScanData(t *testing.T, j []byte) []byte {
	t.Helper()
	i := 2
	for i+4 <= len(j) {
		m := j[i+1]
		l := int(binary.BigEndian.Uint16(j[i+2:]))
		if m == 0xDA {
			end := len(j)
			if bytes.HasSuffix(j, []byte{0xFF, 0xD9}) {
				end -= 2
			}
			return j[i+2+l : end]
		}
		i += 2 + l
	}
	t.Fatal("JPEG 里没找到 SOS")
	return nil
}

// rfc2435Packets 按 RFC 2435 把扫描数据切包，每包带 8 字节 JPEG 头。
func rfc2435Packets(scan []byte, typ, q uint8, w, h, chunk int) [][]byte {
	var out [][]byte
	for off := 0; off < len(scan); off += chunk {
		end := min(off+chunk, len(scan))
		p := []byte{0, byte(off >> 16), byte(off >> 8), byte(off), typ, q, byte(w / 8), byte(h / 8)}
		out = append(out, append(p, scan[off:end]...))
	}
	return out
}

func grayAt(img image.Image, x, y int) uint8 {
	return color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y
}

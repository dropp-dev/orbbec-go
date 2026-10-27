package orbbec

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 最小 RTSP 客户端：DESCRIBE → SETUP（RTP/AVP/TCP 交织）→ PLAY，之后在同一连接上收 RTP/RTCP。

const (
	rtspUserAgent     = "orbbec-go (Open-Orbbec-SDK compatible)"
	rtspMaxLine       = 4096
	rtspMaxHeaders    = 64
	rtspMaxBody       = 1 << 20
	rtspWriteTimeout  = 5 * time.Second
	rtspMinSessionTTL = 5 * time.Second
	rtspMaxSessionTTL = time.Hour
)

type rtspResponse struct {
	status  int
	reason  string
	headers map[string]string // 键为小写
	body    []byte
}

func (r *rtspResponse) header(name string) string { return r.headers[strings.ToLower(name)] }

type rtspSession struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex

	url        string
	controlURL string
	sessionURL string
	session    string
	timeout    time.Duration // 服务端会话超时
	cseq       int
	codec      string
	rtpChan    byte
	rtcpChan   byte
	ssrc       uint32

	teardownCSeq int
	teardownDone chan struct{}
}

// rtspStreamURL 返回 rtsp://ip:port/<stream>/<w>_<h>_<fps>_<format>。
func rtspStreamURL(ip string, port int, stream string, w, h, fps int, format string) string {
	return fmt.Sprintf("rtsp://%s/%s/%d_%d_%d_%s", net.JoinHostPort(ip, strconv.Itoa(port)), stream, w, h, fps, format)
}

func dialRTSP(ctx context.Context, rawURL string) (*rtspSession, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := d.DialContext(dctx, "tcp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("connect RTSP %s: %w", u.Host, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(8 << 20)
	}
	s := &rtspSession{
		conn: conn, br: bufio.NewReaderSize(conn, 256<<10), url: rawURL,
		ssrc: uint32(time.Now().UnixNano()), timeout: 60 * time.Second,
	}
	// ctx 取消时关连接，让握手里的读写立刻返回
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	err = s.handshake(ctx)
	if !stop() {
		err = ctx.Err()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

func (s *rtspSession) handshake(ctx context.Context) error {
	deadline := time.Now().Add(12 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = s.conn.SetDeadline(deadline)
	defer s.conn.SetDeadline(time.Time{})

	resp, err := s.do("DESCRIBE", s.url, map[string]string{"Accept": "application/sdp"})
	if err != nil {
		return err
	}
	base := s.url
	if cb := resp.header("Content-Base"); cb != "" {
		base = cb
	} else if cl := resp.header("Content-Location"); cl != "" {
		base = cl
	}
	codec, control, err := parseSDP(string(resp.body))
	if err != nil {
		return fmt.Errorf("%s: %w", s.url, err)
	}
	s.codec = codec
	s.controlURL = resolveControl(base, control)

	resp, err = s.do("SETUP", s.controlURL, map[string]string{"Transport": "RTP/AVP/TCP;unicast;interleaved=0-1"})
	if err != nil {
		return err
	}
	sess := resp.header("Session")
	if sess == "" {
		return errors.New("RTSP SETUP response has no Session header")
	}
	s.session, s.timeout = parseSessionHeader(sess, s.timeout)
	s.rtpChan, s.rtcpChan = 0, 1
	if tr := resp.header("Transport"); tr != "" {
		for p := range strings.SplitSeq(tr, ";") {
			if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && k == "interleaved" {
				a, b, _ := strings.Cut(v, "-")
				if x, err := strconv.Atoi(a); err == nil {
					s.rtpChan = byte(x)
					s.rtcpChan = byte(x + 1)
				}
				if y, err := strconv.Atoi(b); err == nil {
					s.rtcpChan = byte(y)
				}
			}
		}
	}

	// PLAY/TEARDOWN 用会话地址（Content-Base，带结尾斜杠），和官方 SDK 一致
	s.sessionURL = base
	_, err = s.do("PLAY", s.sessionURL, map[string]string{"Range": "npt=0.000-"})
	return err
}

// parseSessionHeader 解析 "id;timeout=秒"，超时限制在 [rtspMinSessionTTL, rtspMaxSessionTTL]，没写时用 def。
func parseSessionHeader(h string, def time.Duration) (id string, timeout time.Duration) {
	parts := strings.Split(h, ";")
	id, timeout = strings.TrimSpace(parts[0]), def
	for _, p := range parts[1:] {
		if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && strings.EqualFold(k, "timeout") {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				timeout = min(max(time.Duration(min(n, 1<<31))*time.Second, rtspMinSessionTTL), rtspMaxSessionTTL)
			}
		}
	}
	return id, timeout
}

// do 发一条请求并等对应 CSeq 的响应，期间收到的 RTP 包丢弃。
func (s *rtspSession) do(method, uri string, headers map[string]string) (*rtspResponse, error) {
	cseq, err := s.send(method, uri, headers, nil)
	if err != nil {
		return nil, err
	}
	for {
		_, _, resp, err := s.readMessage()
		if err != nil {
			return nil, fmt.Errorf("RTSP %s: %w", method, err)
		}
		if resp == nil {
			continue
		}
		if n, _ := strconv.Atoi(resp.header("CSeq")); n != cseq {
			continue
		}
		if resp.status != 200 {
			return nil, fmt.Errorf("RTSP %s %s: %d %s", method, uri, resp.status, resp.reason)
		}
		return resp, nil
	}
}

// send 发一条请求，返回它的 CSeq；before 在写入前持锁调用，用来登记要匹配的 CSeq。
func (s *rtspSession) send(method, uri string, headers map[string]string, before func(cseq int)) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.cseq++
	if before != nil {
		before(s.cseq)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s RTSP/1.0\r\nCSeq: %d\r\nUser-Agent: %s\r\n", method, uri, s.cseq, rtspUserAgent)
	if s.session != "" {
		fmt.Fprintf(&b, "Session: %s\r\n", s.session)
	}
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("\r\n")
	_, err := s.writeLocked([]byte(b.String()))
	return s.cseq, err
}

// writeLocked 带写超时写入，调用方持 wmu。
func (s *rtspSession) writeLocked(b []byte) (int, error) {
	_ = s.conn.SetWriteDeadline(time.Now().Add(rtspWriteTimeout))
	n, err := s.conn.Write(b)
	_ = s.conn.SetWriteDeadline(time.Time{})
	return n, err
}

// readMessage 读下一条消息：交织包返回通道和数据（下次调用前有效），否则返回 RTSP 响应。
func (s *rtspSession) readMessage() (byte, []byte, *rtspResponse, error) {
	first, err := s.br.Peek(1)
	if err != nil {
		return 0, nil, nil, err
	}
	if first[0] == '$' {
		hdr, err := s.br.Peek(4)
		if err != nil {
			return 0, nil, nil, err
		}
		ch := hdr[1]
		n := int(binary.BigEndian.Uint16(hdr[2:]))
		if _, err := s.br.Discard(4); err != nil {
			return 0, nil, nil, err
		}
		data, err := s.br.Peek(n)
		if err != nil {
			return 0, nil, nil, err
		}
		_, _ = s.br.Discard(n)
		return ch, data, nil, nil
	}
	resp, err := s.readResponse()
	return 0, nil, resp, err
}

// readLine 读一行（不含换行），超过 rtspMaxLine 报错。
func (s *rtspSession) readLine() (string, error) {
	line, err := s.br.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", errors.New("RTSP line too long")
		}
		return "", err
	}
	if len(line) > rtspMaxLine {
		return "", errors.New("RTSP line too long")
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

func (s *rtspSession) readResponse() (*rtspResponse, error) {
	line, err := s.readLine()
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "RTSP/") {
		return nil, fmt.Errorf("unexpected RTSP data %q", truncate(line, 80))
	}
	fields := strings.SplitN(line, " ", 3)
	if len(fields) < 2 {
		return nil, fmt.Errorf("invalid RTSP status line %q", line)
	}
	status, err := strconv.Atoi(fields[1])
	if err != nil {
		return nil, fmt.Errorf("invalid RTSP status line %q", line)
	}
	resp := &rtspResponse{status: status, headers: map[string]string{}}
	if len(fields) == 3 {
		resp.reason = fields[2]
	}
	for {
		h, err := s.readLine()
		if err != nil {
			return nil, err
		}
		if h == "" {
			break
		}
		if len(resp.headers) >= rtspMaxHeaders {
			return nil, errors.New("too many RTSP headers")
		}
		if k, v, ok := strings.Cut(h, ":"); ok {
			resp.headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	if cl := resp.header("Content-Length"); cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil || n < 0 || n > rtspMaxBody {
			return nil, fmt.Errorf("invalid Content-Length %q", cl)
		}
		resp.body = make([]byte, n)
		if _, err := io.ReadFull(s.br, resp.body); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// parseSDP 返回第一个视频段的编码名和 control 属性。
func parseSDP(sdp string) (codec, control string, err error) {
	inVideo := false
	sessionControl := ""
	var pt string
	for line := range strings.SplitSeq(sdp, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "m="):
			if inVideo && codec != "" {
				return codec, control, nil
			}
			f := strings.Fields(line[2:])
			inVideo = len(f) >= 4 && strings.EqualFold(f[0], "video") // 435Le 写的是 m=VIDEO
			if inVideo {
				pt = f[3]
			}
		case strings.HasPrefix(line, "a=control:"):
			if inVideo {
				control = strings.TrimPrefix(line, "a=control:")
			} else if pt == "" {
				sessionControl = strings.TrimPrefix(line, "a=control:")
			}
		case inVideo && strings.HasPrefix(line, "a=rtpmap:"):
			f := strings.Fields(strings.TrimPrefix(line, "a=rtpmap:"))
			if len(f) == 2 && f[0] == pt {
				codec, _, _ = strings.Cut(f[1], "/")
			}
		}
	}
	if pt == "" {
		return "", "", errors.New("no video stream in SDP")
	}
	if codec == "" {
		if pt == "26" { // 静态负载类型 JPEG
			codec = "JPEG"
		} else {
			return "", "", fmt.Errorf("no rtpmap for payload type %s in SDP", pt)
		}
	}
	if control == "" {
		control = sessionControl
	}
	return codec, control, nil
}

func resolveControl(base, control string) string {
	switch {
	case control == "" || control == "*":
		return base
	case strings.HasPrefix(strings.ToLower(control), "rtsp://"):
		return control
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + control
}

// sendReceiverReport 发一个空的 RTCP RR，服务端靠它续会话。
func (s *rtspSession) sendReceiverReport() error {
	pkt := make([]byte, 4+8)
	pkt[0] = '$'
	pkt[1] = s.rtcpChan
	binary.BigEndian.PutUint16(pkt[2:], 8)
	pkt[4] = 0x80 // V=2
	pkt[5] = 201  // RR
	binary.BigEndian.PutUint16(pkt[6:], 1)
	binary.BigEndian.PutUint32(pkt[8:], s.ssrc)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.writeLocked(pkt)
	return err
}

// teardown 发 TEARDOWN，最多等 wait 让读循环收到响应，然后关闭连接。wait 为 0 时发完就关。
func (s *rtspSession) teardown(wait time.Duration) {
	if s.session != "" {
		done := make(chan struct{})
		u := s.sessionURL
		if u == "" {
			u = s.url
		}
		_, err := s.send("TEARDOWN", u, nil, func(cseq int) {
			s.teardownCSeq, s.teardownDone = cseq, done
		})
		if err == nil && wait > 0 {
			select {
			case <-done:
			case <-time.After(wait):
			}
		}
	}
	_ = s.conn.Close()
}

// isTeardownResponse 由读循环调用：是 TEARDOWN 的响应时通知 teardown 并返回 true。
func (s *rtspSession) isTeardownResponse(resp *rtspResponse) bool {
	s.wmu.Lock()
	cseq, done := s.teardownCSeq, s.teardownDone
	s.wmu.Unlock()
	if cseq == 0 || resp.header("CSeq") != strconv.Itoa(cseq) {
		return false
	}
	close(done)
	return true
}

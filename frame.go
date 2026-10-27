package orbbec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

type rtpPacket struct {
	marker  bool
	seq     uint16
	payload []byte
}

func parseRTP(b []byte) (rtpPacket, error) {
	if len(b) < 12 {
		return rtpPacket{}, errors.New("short RTP packet")
	}
	if b[0]>>6 != 2 {
		return rtpPacket{}, fmt.Errorf("unsupported RTP version %d", b[0]>>6)
	}
	p := rtpPacket{marker: b[1]&0x80 != 0, seq: binary.BigEndian.Uint16(b[2:])}
	off := 12 + int(b[0]&0x0f)*4
	if b[0]&0x10 != 0 { // 扩展头
		if len(b) < off+4 {
			return p, errors.New("truncated RTP extension")
		}
		off += 4 + int(binary.BigEndian.Uint16(b[off+2:]))*4
	}
	end := len(b)
	if b[0]&0x20 != 0 && end > off { // 填充
		end -= int(b[end-1])
	}
	if off > end {
		return p, errors.New("invalid RTP header length")
	}
	p.payload = b[off:end]
	return p, nil
}

// frameAssembler 把 RTP 包拼成帧：负载依次追加，marker 位结束一帧；序号不连续的帧丢弃。
type frameAssembler struct {
	buf      []byte
	nextSeq  uint16
	started  bool
	broken   bool
	maxBytes int
	depay    func(p rtpPacket, first bool) ([]byte, error) // 去掉每包的负载格式头
	dropped  int
}

// push 收一个包，帧完整时返回帧数据（下次 push 前有效）。
func (a *frameAssembler) push(p rtpPacket) []byte {
	if a.started && p.seq != a.nextSeq {
		a.broken = true
	}
	a.nextSeq = p.seq + 1
	first := !a.started
	if first {
		a.buf = a.buf[:0]
		a.broken = false
		a.started = true
	}
	if !a.broken {
		data := p.payload
		if a.depay != nil {
			d, err := a.depay(p, first)
			if err != nil {
				a.broken = true
			}
			data = d
		}
		if len(a.buf)+len(data) > a.maxBytes {
			a.broken = true
		} else {
			a.buf = append(a.buf, data...)
		}
	}
	if !p.marker {
		return nil
	}
	a.started = false
	if a.broken {
		a.dropped++
		return nil
	}
	return a.buf
}

// vendorFrameHeader 是厂商编码每帧开头的 26 字节帧头（pack(2)）。435Le 的宽高字段为 0，时间戳单位 ms。
type vendorFrameHeader struct {
	FrameCounter uint64
	ExtLen       uint16
	Timestamp    uint64
	Width        uint32
	Height       uint32
}

const vendorFrameHeaderSize = 26

// splitVendorFrame 拆出帧头、元数据和像素数据。
func splitVendorFrame(b []byte) (vendorFrameHeader, []byte, []byte, error) {
	var h vendorFrameHeader
	if len(b) < vendorFrameHeaderSize {
		return h, nil, nil, fmt.Errorf("short frame: %d bytes", len(b))
	}
	h.FrameCounter = binary.LittleEndian.Uint64(b[0:])
	h.ExtLen = binary.LittleEndian.Uint16(b[8:])
	h.Timestamp = binary.LittleEndian.Uint64(b[10:])
	h.Width = binary.LittleEndian.Uint32(b[18:])
	h.Height = binary.LittleEndian.Uint32(b[22:])
	off := vendorFrameHeaderSize + int(h.ExtLen)
	if off > len(b) {
		return h, nil, nil, fmt.Errorf("frame metadata length %d exceeds frame size %d", h.ExtLen, len(b))
	}
	return h, b[vendorFrameHeaderSize:off], b[off:], nil
}

// decodeY16 把小端 Y16 换算成 mm（乘单位后四舍五入）。
func decodeY16(data []byte, w, h int, unit float32) ([]uint16, error) {
	n := w * h
	if len(data) < n*2 {
		return nil, fmt.Errorf("depth frame has %d bytes, want %d for %dx%d Y16", len(data), n*2, w, h)
	}
	dst := make([]uint16, n)
	if unit == 1 {
		for i := range n {
			dst[i] = binary.LittleEndian.Uint16(data[i*2:])
		}
		return dst, nil
	}
	for i := range n {
		v := float32(binary.LittleEndian.Uint16(data[i*2:]))*unit + 0.5
		dst[i] = uint16(min(v, math.MaxUint16))
	}
	return dst, nil
}

package orbbec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// RFC 2435：彩色流用静态负载类型 26，每包带 8 字节 JPEG 头（可选重启标记头、量化表头），收端补上 JPEG 头。
// 兼容版本 1 下相机直接把完整 JPEG 当扫描数据发来，这时原样使用，缺 Huffman 表时补标准表。

type jpegFrameInfo struct {
	typ, q        uint8
	width, height int
	dri           uint16
	qtables       []byte
}

// jpegDepay 去掉每包的 RFC 2435 头，首包时记下合成 JPEG 头的参数。
type jpegDepay struct {
	info jpegFrameInfo
}

func (d *jpegDepay) depay(p rtpPacket, first bool) ([]byte, error) {
	b := p.payload
	if len(b) < 8 {
		return nil, errors.New("short RTP JPEG packet")
	}
	offset := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	typ, q := b[4], b[5]
	w, h := int(b[6])*8, int(b[7])*8
	if w == 0 {
		w = 2048
	}
	if h == 0 {
		h = 2048
	}
	n := 8
	var dri uint16
	if typ >= 64 && typ <= 127 {
		if len(b) < n+4 {
			return nil, errors.New("truncated RTP JPEG restart header")
		}
		dri = binary.BigEndian.Uint16(b[n:])
		n += 4
	}
	var qt []byte
	if offset == 0 && q >= 128 {
		if len(b) < n+4 {
			return nil, errors.New("truncated RTP JPEG quantization header")
		}
		if b[n] == 0 {
			l := int(binary.BigEndian.Uint16(b[n+2:]))
			n += 4
			if len(b) < n+l {
				return nil, errors.New("truncated RTP JPEG quantization table")
			}
			qt = b[n : n+l]
			n += l
		}
	}
	if first {
		if offset != 0 {
			return nil, fmt.Errorf("first packet has fragment offset %d", offset)
		}
		d.info = jpegFrameInfo{typ: typ, q: q, width: w, height: h, dri: dri, qtables: append([]byte(nil), qt...)}
	}
	return b[n:], nil
}

// finish 把拼好的扫描数据变成完整 JPEG。
func (d *jpegDepay) finish(scan []byte) []byte {
	if len(scan) >= 2 && scan[0] == 0xFF && scan[1] == 0xD8 {
		return ensureHuffmanTables(scan)
	}
	var out bytes.Buffer
	writeJPEGHeader(&out, d.info)
	out.Write(scan)
	if !bytes.HasSuffix(scan, []byte{0xFF, 0xD9}) {
		out.Write([]byte{0xFF, 0xD9})
	}
	return out.Bytes()
}

// 标准量化表（JPEG Annex K，自然顺序）。
var jpegLumaQuant = [64]int{
	16, 11, 10, 16, 24, 40, 51, 61,
	12, 12, 14, 19, 26, 58, 60, 55,
	14, 13, 16, 24, 40, 57, 69, 56,
	14, 17, 22, 29, 51, 87, 80, 62,
	18, 22, 37, 56, 68, 109, 103, 77,
	24, 35, 55, 64, 81, 104, 113, 92,
	49, 64, 78, 87, 103, 121, 120, 101,
	72, 92, 95, 98, 112, 100, 103, 99,
}

var jpegChromaQuant = [64]int{
	17, 18, 24, 47, 99, 99, 99, 99,
	18, 21, 26, 66, 99, 99, 99, 99,
	24, 26, 56, 99, 99, 99, 99, 99,
	47, 66, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
	99, 99, 99, 99, 99, 99, 99, 99,
}

// zigzag[i] 是之字形第 i 个系数的自然顺序下标。
var zigzag = [64]int{
	0, 1, 8, 16, 9, 2, 3, 10, 17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34, 27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36, 29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46, 53, 60, 61, 54, 47, 55, 62, 63,
}

// makeQuantTables 按 RFC 2435 附录 A 由 Q 生成两张量化表（之字形顺序）。
func makeQuantTables(q int) (luma, chroma [64]byte) {
	factor := min(max(q, 1), 99)
	var scale int
	if q < 50 {
		scale = 5000 / factor
	} else {
		scale = 200 - factor*2
	}
	for i := range 64 {
		lq := min(max((jpegLumaQuant[zigzag[i]]*scale+50)/100, 1), 255)
		cq := min(max((jpegChromaQuant[zigzag[i]]*scale+50)/100, 1), 255)
		luma[i], chroma[i] = byte(lq), byte(cq)
	}
	return
}

// 标准 Huffman 表（JPEG Annex K.3）。
var (
	dcLumaBits   = []byte{0, 1, 5, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0, 0, 0}
	dcLumaVals   = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	dcChromaBits = []byte{0, 3, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 0, 0, 0, 0}
	dcChromaVals = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	acLumaBits   = []byte{0, 2, 1, 3, 3, 2, 4, 3, 5, 5, 4, 4, 0, 0, 1, 0x7d}
	acLumaVals   = []byte{
		0x01, 0x02, 0x03, 0x00, 0x04, 0x11, 0x05, 0x12, 0x21, 0x31, 0x41, 0x06, 0x13, 0x51, 0x61, 0x07,
		0x22, 0x71, 0x14, 0x32, 0x81, 0x91, 0xa1, 0x08, 0x23, 0x42, 0xb1, 0xc1, 0x15, 0x52, 0xd1, 0xf0,
		0x24, 0x33, 0x62, 0x72, 0x82, 0x09, 0x0a, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x25, 0x26, 0x27, 0x28,
		0x29, 0x2a, 0x34, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49,
		0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69,
		0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x83, 0x84, 0x85, 0x86, 0x87, 0x88, 0x89,
		0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 0xa7,
		0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3, 0xc4, 0xc5,
		0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda, 0xe1, 0xe2,
		0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xf1, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8,
		0xf9, 0xfa,
	}
	acChromaBits = []byte{0, 2, 1, 2, 4, 4, 3, 4, 7, 5, 4, 4, 0, 1, 2, 0x77}
	acChromaVals = []byte{
		0x00, 0x01, 0x02, 0x03, 0x11, 0x04, 0x05, 0x21, 0x31, 0x06, 0x12, 0x41, 0x51, 0x07, 0x61, 0x71,
		0x13, 0x22, 0x32, 0x81, 0x08, 0x14, 0x42, 0x91, 0xa1, 0xb1, 0xc1, 0x09, 0x23, 0x33, 0x52, 0xf0,
		0x15, 0x62, 0x72, 0xd1, 0x0a, 0x16, 0x24, 0x34, 0xe1, 0x25, 0xf1, 0x17, 0x18, 0x19, 0x1a, 0x26,
		0x27, 0x28, 0x29, 0x2a, 0x35, 0x36, 0x37, 0x38, 0x39, 0x3a, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48,
		0x49, 0x4a, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68,
		0x69, 0x6a, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
		0x88, 0x89, 0x8a, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0xa2, 0xa3, 0xa4, 0xa5,
		0xa6, 0xa7, 0xa8, 0xa9, 0xaa, 0xb2, 0xb3, 0xb4, 0xb5, 0xb6, 0xb7, 0xb8, 0xb9, 0xba, 0xc2, 0xc3,
		0xc4, 0xc5, 0xc6, 0xc7, 0xc8, 0xc9, 0xca, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7, 0xd8, 0xd9, 0xda,
		0xe2, 0xe3, 0xe4, 0xe5, 0xe6, 0xe7, 0xe8, 0xe9, 0xea, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8,
		0xf9, 0xfa,
	}
)

func writeSegment(w *bytes.Buffer, marker byte, body []byte) {
	w.Write([]byte{0xFF, marker})
	_ = binary.Write(w, binary.BigEndian, uint16(len(body)+2))
	w.Write(body)
}

// standardDHT 返回含四张标准表的 DHT 段。
func standardDHT() []byte {
	var body []byte
	for _, t := range []struct {
		class byte
		bits  []byte
		vals  []byte
	}{{0x00, dcLumaBits, dcLumaVals}, {0x10, acLumaBits, acLumaVals}, {0x01, dcChromaBits, dcChromaVals}, {0x11, acChromaBits, acChromaVals}} {
		body = append(body, t.class)
		body = append(body, t.bits...)
		body = append(body, t.vals...)
	}
	var b bytes.Buffer
	writeSegment(&b, 0xC4, body)
	return b.Bytes()
}

// writeJPEGHeader 按 RFC 2435 附录 B 合成 SOI 到 SOS。
func writeJPEGHeader(w *bytes.Buffer, info jpegFrameInfo) {
	w.Write([]byte{0xFF, 0xD8})
	var tables [][]byte
	if len(info.qtables) >= 64 {
		for i := 0; i+64 <= len(info.qtables); i += 64 {
			tables = append(tables, info.qtables[i:i+64])
		}
	} else {
		l, c := makeQuantTables(int(info.q))
		tables = [][]byte{l[:], c[:]}
	}
	for i, t := range tables {
		writeSegment(w, 0xDB, append([]byte{byte(i)}, t...))
	}
	if info.dri != 0 {
		writeSegment(w, 0xDD, binary.BigEndian.AppendUint16(nil, info.dri))
	}
	ySampling := byte(0x21) // 4:2:2
	if info.typ&1 == 1 {
		ySampling = 0x22 // 4:2:0
	}
	chromaQ := byte(1)
	if len(tables) == 1 {
		chromaQ = 0
	}
	sof := []byte{8, byte(info.height >> 8), byte(info.height), byte(info.width >> 8), byte(info.width), 3,
		1, ySampling, 0, 2, 0x11, chromaQ, 3, 0x11, chromaQ}
	writeSegment(w, 0xC0, sof)
	w.Write(standardDHT())
	writeSegment(w, 0xDA, []byte{3, 1, 0x00, 2, 0x11, 3, 0x11, 0, 63, 0})
}

// ensureHuffmanTables 给缺 DHT 的 JPEG 在 SOS 前补上标准表。
func ensureHuffmanTables(j []byte) []byte {
	i := 2
	for i+4 <= len(j) {
		if j[i] != 0xFF {
			return j
		}
		m := j[i+1]
		if m == 0xFF {
			i++
			continue
		}
		if m == 0xC4 {
			return j
		}
		if m == 0xDA {
			out := make([]byte, 0, len(j)+432)
			out = append(out, j[:i]...)
			out = append(out, standardDHT()...)
			return append(out, j[i:]...)
		}
		if m == 0xD8 || (m >= 0xD0 && m <= 0xD7) || m == 0x01 {
			i += 2
			continue
		}
		i += 2 + int(binary.BigEndian.Uint16(j[i+2:]))
	}
	return j
}

// jpegMetadata 取出 JPEG COM 段里的帧元数据，没有返回 nil。
func jpegMetadata(j []byte) []byte {
	i := 2
	for i+4 <= len(j) && j[0] == 0xFF && j[1] == 0xD8 {
		if j[i] != 0xFF {
			return nil
		}
		m := j[i+1]
		if m == 0xDA || m == 0xD9 {
			return nil
		}
		l := int(binary.BigEndian.Uint16(j[i+2:]))
		if m == 0xFE && i+2+l <= len(j) && bytes.HasPrefix(j[i+4:i+2+l], metadataMagic) {
			return append([]byte(nil), j[i+4:i+2+l]...)
		}
		i += 2 + l
	}
	return nil
}

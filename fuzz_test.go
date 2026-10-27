package orbbec

import (
	"encoding/binary"
	"testing"
)

// 解析器收到畸形数据只能报错，不能 panic。

func FuzzParseRTP(f *testing.F) {
	f.Add([]byte{0x80, 0xe1, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 1, 2, 3})
	f.Add([]byte{0xb0, 0x61, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 2, 3, 2})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = parseRTP(b) })
}

func FuzzJPEG(f *testing.F) {
	f.Add([]byte{0xFF, 0xD8, 0xFF, 0xFE, 0, 0})
	f.Add([]byte{0xFF, 0xD8, 0xFF, 0xFE, 0, 1})
	f.Add(append([]byte{0xFF, 0xD8, 0xFF, 0xFE, 0, 10}, []byte("ORBBEC12")...))
	f.Add([]byte{0xFF, 0xD8, 0xFF, 0xDA, 0, 2, 0xFF, 0xD9})
	f.Fuzz(func(t *testing.T, b []byte) {
		_ = jpegMetadata(b)
		_ = ensureHuffmanTables(b)
		d := &jpegDepay{}
		if _, err := d.depay(rtpPacket{payload: b}, true); err == nil {
			_ = d.finish(b)
		}
		_, _ = d.depay(rtpPacket{payload: b}, false)
	})
}

func FuzzVendorFrame(f *testing.F) {
	hdr := make([]byte, vendorFrameHeaderSize)
	binary.LittleEndian.PutUint16(hdr[8:], 300)
	f.Add(hdr)
	f.Add(append(hdr, make([]byte, 400)...))
	f.Fuzz(func(t *testing.T, b []byte) {
		if _, md, data, err := splitVendorFrame(b); err == nil {
			_, _ = ParseFrameMetadata(md, true)
			_, _ = decodeY16(data, 4, 3, 0.2)
		}
	})
}

func FuzzGVCPAndCalib(f *testing.F) {
	f.Add([]byte{0, 0, 0, 3, 0, 0, 0, 1})
	f.Add(fixtureAlignCalib())
	f.Add(fixtureD2CList())
	f.Add(fixtureEffectiveProfiles())
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = parseDiscoveryAck(b)
		_, _ = parseVersionInfo(b)
		_ = parseDepthCalibList(b)
		c := Calibration{align: parseAlignCalibList(b), d2c: parseD2CProfileList(b)}
		_, _ = c.DepthIntrinsics(1280, 800)
		_, _ = c.ColorIntrinsics(640, 400)
		_ = parseEffectiveProfiles(b)
		_, _, _ = parseSDP(string(b))
		_ = firmwareVersionInt(string(b))
	})
}

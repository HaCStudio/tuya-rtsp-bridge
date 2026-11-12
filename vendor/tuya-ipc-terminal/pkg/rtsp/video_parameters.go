package rtsp

import (
	"encoding/base64"
	"encoding/binary"
)

const videoPayloadType = 96

// Cache parameter sets even before any RTSP client has set up a transport.
type videoParameters struct {
	observed bool
	hevc     bool
	vps      string
	sps      string
	pps      string
}

func (p *videoParameters) observe(payload []byte, hevc bool) {
	headerSize := 1
	if hevc {
		headerSize = 2
	}
	if len(payload) <= headerSize {
		return
	}

	if !p.observed || p.hevc != hevc {
		*p = videoParameters{observed: true, hevc: hevc}
	}

	// H.264 STAP-A and HEVC AP without a decoding order number both use
	// a 16-bit length before each encapsulated NAL unit.
	aggregate := (!hevc && payload[0]&0x1f == 24) || (hevc && (payload[0]>>1)&0x3f == 48)
	if !aggregate {
		p.cacheNAL(payload)
		return
	}
	for payload = payload[headerSize:]; len(payload) >= 2; {
		size := int(binary.BigEndian.Uint16(payload))
		payload = payload[2:]
		if size <= headerSize || size > len(payload) {
			return
		}
		p.cacheNAL(payload[:size])
		payload = payload[size:]
	}
}

func (p *videoParameters) cacheNAL(nal []byte) {
	var target *string
	if p.hevc {
		switch (nal[0] >> 1) & 0x3f {
		case 32:
			target = &p.vps
		case 33:
			target = &p.sps
		case 34:
			target = &p.pps
		}
	} else {
		switch nal[0] & 0x1f {
		case 7:
			target = &p.sps
		case 8:
			target = &p.pps
		}
	}
	if target != nil {
		*target = base64.StdEncoding.EncodeToString(nal)
	}
}

func (rf *RTPForwarder) videoDescription(hevc bool) (codec, fmtp string) {
	rf.videoParametersMutex.RLock()
	p := rf.videoParameters
	rf.videoParametersMutex.RUnlock()

	if p.observed {
		hevc = p.hevc
	}
	if hevc {
		fmtp = "profile-id=1"
		if p.vps != "" && p.sps != "" && p.pps != "" {
			fmtp += ";sprop-vps=" + p.vps + ";sprop-sps=" + p.sps + ";sprop-pps=" + p.pps
		}
		return "H265", fmtp
	}
	fmtp = "packetization-mode=1"
	if p.sps != "" && p.pps != "" {
		fmtp += ";sprop-parameter-sets=" + p.sps + "," + p.pps
	}
	return "H264", fmtp
}

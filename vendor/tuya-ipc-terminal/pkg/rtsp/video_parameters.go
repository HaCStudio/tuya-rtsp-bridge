package rtsp

import (
	"encoding/base64"
	"encoding/binary"
	"time"

	"github.com/pion/rtp"
)

const (
	videoPayloadType    = 96
	maxParameterSetSize = 64 * 1024
)

// Cache parameter sets even before any RTSP client has set up a transport.
type videoParameters struct {
	observed bool
	hevc     bool
	vps      string
	sps      string
	pps      string

	fragment          []byte
	fragmentType      byte
	fragmentTimestamp uint32
	nextSequence      uint16
}

func (p *videoParameters) complete() bool {
	return p.sps != "" && p.pps != "" && (!p.hevc || p.vps != "")
}

func (p *videoParameters) observe(packet *rtp.Packet, hevc bool) {
	payload := packet.Payload
	headerSize := 1
	if hevc {
		headerSize = 2
	}
	if len(payload) <= headerSize {
		return
	}

	if !p.observed {
		p.observed = true
		p.hevc = hevc
	} else if p.hevc != hevc {
		return
	}

	nalType := p.nalType(payload[0])
	if (!p.hevc && nalType == 28) || (p.hevc && nalType == 49) {
		p.observeFragment(packet, headerSize)
		return
	}
	p.fragment = nil

	// H.264 STAP-A and HEVC AP without a decoding order number both use
	// a 16-bit length before each encapsulated NAL unit.
	aggregate := (!p.hevc && nalType == 24) || (p.hevc && nalType == 48)
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

func (p *videoParameters) nalType(header byte) byte {
	if p.hevc {
		return (header >> 1) & 0x3f
	}
	return header & 0x1f
}

func (p *videoParameters) observeFragment(packet *rtp.Packet, headerSize int) {
	payload := packet.Payload
	if len(payload) <= headerSize+1 {
		p.fragment = nil
		return
	}
	flags := payload[headerSize]
	typ := flags & 0x1f
	if p.hevc {
		typ = flags & 0x3f
	}
	if p.parameterSet(typ) == nil {
		p.fragment = nil
		return
	}
	if flags&0x80 != 0 {
		if flags&0x40 != 0 {
			p.fragment = nil
			return
		}
		if p.hevc {
			p.fragment = []byte{payload[0]&0x81 | typ<<1, payload[1]}
		} else {
			p.fragment = []byte{payload[0]&0xe0 | typ}
		}
		p.fragmentType = typ
		p.fragmentTimestamp = packet.Timestamp
	} else if p.fragment == nil || packet.SequenceNumber != p.nextSequence ||
		packet.Timestamp != p.fragmentTimestamp || typ != p.fragmentType {
		p.fragment = nil
		return
	}
	part := payload[headerSize+1:]
	if len(part) > maxParameterSetSize-len(p.fragment) {
		p.fragment = nil
		return
	}
	p.fragment = append(p.fragment, part...)
	p.nextSequence = packet.SequenceNumber + 1
	if flags&0x40 != 0 {
		p.cacheNAL(p.fragment)
		p.fragment = nil
	}
}

func (p *videoParameters) parameterSet(typ byte) *string {
	if p.hevc {
		switch typ {
		case 32:
			return &p.vps
		case 33:
			return &p.sps
		case 34:
			return &p.pps
		}
	} else {
		switch typ {
		case 7:
			return &p.sps
		case 8:
			return &p.pps
		}
	}
	return nil
}

func (p *videoParameters) cacheNAL(nal []byte) {
	if target := p.parameterSet(p.nalType(nal[0])); target != nil {
		*target = base64.StdEncoding.EncodeToString(nal)
	}
}

func (rf *RTPForwarder) waitForVideoParameters(timeout time.Duration) bool {
	rf.videoParametersMutex.RLock()
	ready := rf.videoParametersReady
	complete := rf.videoParameters.complete()
	rf.videoParametersMutex.RUnlock()
	if complete {
		return true
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ready:
		return true
	case <-timer.C:
		return false
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
		if p.complete() {
			fmtp += ";sprop-vps=" + p.vps + ";sprop-sps=" + p.sps + ";sprop-pps=" + p.pps
		}
		return "H265", fmtp
	}
	fmtp = "packetization-mode=1"
	if p.complete() {
		fmtp += ";sprop-parameter-sets=" + p.sps + "," + p.pps
	}
	return "H264", fmtp
}

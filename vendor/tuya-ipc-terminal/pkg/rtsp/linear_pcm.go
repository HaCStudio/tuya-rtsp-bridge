package rtsp

import (
	"encoding/json"
	"fmt"

	"github.com/pion/rtp"

	"tuya-ipc-terminal/pkg/tuya"
)

const linearPCMPayloadType = 97

type linearPCMFormat struct {
	sampleRate int
	channels   int
}

func linearPCMFromSkill(rawSkill string) linearPCMFormat {
	const tuyaLinearPCMCodec = 101
	var skill tuya.Skill
	if err := json.Unmarshal([]byte(rawSkill), &skill); err != nil || len(skill.Audios) == 0 {
		return linearPCMFormat{}
	}
	audio := skill.Audios[0]
	if audio.CodecType != tuyaLinearPCMCodec {
		return linearPCMFormat{}
	}
	format := linearPCMFormat{sampleRate: audio.SampleRate, channels: audio.Channels}
	if format.sampleRate <= 0 {
		format.sampleRate = 8000
	}
	if format.channels <= 0 {
		format.channels = 1
	}
	return format
}

func (f linearPCMFormat) sdp() string {
	return fmt.Sprintf("m=audio 0 RTP/AVP %d\r\na=rtpmap:%d L16/%d/%d\r\n",
		linearPCMPayloadType, linearPCMPayloadType, f.sampleRate, f.channels)
}

func (rf *RTPForwarder) forwardDataChannelAudio(packet *rtp.Packet) {
	if rf.linearPCM.sampleRate == 0 {
		rf.ForwardAudioPacket(packet)
		return
	}

	if len(packet.Payload)%2 != 0 {
		return
	}

	// Tuya's PCM is little-endian. RTP L16 uses network byte order.
	outgoing := *packet
	outgoing.PayloadType = linearPCMPayloadType
	outgoing.Payload = make([]byte, len(packet.Payload))
	for i := 0; i < len(packet.Payload); i += 2 {
		outgoing.Payload[i] = packet.Payload[i+1]
		outgoing.Payload[i+1] = packet.Payload[i]
	}
	rf.ForwardAudioPacket(&outgoing)
}

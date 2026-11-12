package rtsp

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"

	"tuya-ipc-terminal/pkg/storage"
)

func TestDataChannelLinearPCMMatchesSDP(t *testing.T) {
	camera := &storage.CameraInfo{Skill: `{
		"videos":[{"streamType":2,"codecType":4,"width":2880,"height":1616}],
		"audios":[{"codecType":101,"sampleRate":16000,"channels":1}]
	}`}
	bridge := NewWebRTCBridge(camera, "hd", nil, nil)
	defer bridge.cancel()
	forwarder := bridge.rtpForwarder
	defer forwarder.Stop()

	server := &RTSPServer{}
	sdp := server.generateSDP(camera, "rtsp://127.0.0.1/Camera/hd", "hd", forwarder)
	if !strings.Contains(sdp, "m=audio 0 RTP/AVP 97\r\na=rtpmap:97 L16/16000/1\r\n") {
		t.Fatalf("unexpected PCM description: %s", sdp)
	}

	sender, receiver := net.Pipe()
	defer sender.Close()
	defer receiver.Close()
	receiver.SetReadDeadline(time.Now().Add(time.Second))
	sender.SetWriteDeadline(time.Now().Add(time.Second))
	if err := forwarder.AddTCPClient("test", sender, 0, 2, 4); err != nil {
		t.Fatal(err)
	}

	original := []byte{0, 0, 0x34, 0x12, 0xcc, 0xed}
	packet := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 101, SequenceNumber: 7, Timestamp: 160},
		Payload: append([]byte(nil), original...),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		forwarder.forwardDataChannelAudio(packet)
	}()

	var header [4]byte
	if _, err := io.ReadFull(receiver, header[:]); err != nil {
		t.Fatal(err)
	}
	if header[0] != '$' || header[1] != 2 {
		t.Fatalf("unexpected interleaved header: %x", header)
	}
	data := make([]byte, binary.BigEndian.Uint16(header[2:]))
	if _, err := io.ReadFull(receiver, data); err != nil {
		t.Fatal(err)
	}
	<-done

	var got rtp.Packet
	if err := got.Unmarshal(data); err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 0, 0x12, 0x34, 0xed, 0xcc}
	if got.PayloadType != 97 || got.SequenceNumber != 7 || got.Timestamp != 160 || !bytes.Equal(got.Payload, want) {
		t.Fatalf("unexpected PCM packet: %+v payload=%x", got.Header, got.Payload)
	}
	if packet.PayloadType != 101 || !bytes.Equal(packet.Payload, original) {
		t.Fatal("input packet was modified")
	}
}

func TestNonPCMTracksKeepG711Description(t *testing.T) {
	server := &RTSPServer{}
	for _, tc := range []struct {
		skill string
		want  string
	}{
		{`{"videos":[{"streamType":2,"codecType":4,"width":160,"height":120}],"audios":[{"codecType":106}]}`, "PCMA/8000"},
		{`{"videos":[{"streamType":2,"codecType":2,"width":160,"height":120}],"audios":[{"codecType":101}]}`, "PCMU/8000"},
	} {
		camera := &storage.CameraInfo{Skill: tc.skill}
		bridge := NewWebRTCBridge(camera, "hd", nil, nil)
		sdp := server.generateSDP(camera, "rtsp://127.0.0.1/Camera/hd", "hd", bridge.rtpForwarder)
		bridge.cancel()
		if !strings.Contains(sdp, tc.want) {
			t.Fatalf("expected %s in SDP: %s", tc.want, sdp)
		}
	}
}

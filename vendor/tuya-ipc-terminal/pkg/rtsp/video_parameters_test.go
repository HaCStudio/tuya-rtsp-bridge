package rtsp

import (
	"encoding/binary"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"

	"tuya-ipc-terminal/pkg/storage"
)

func TestVideoSDPIncludesCachedParameterSets(t *testing.T) {
	tests := []struct {
		name    string
		hevc    bool
		payload []byte
		want    string
	}{
		{"H264", false, []byte{24, 0, 2, 0x67, 1, 0, 2, 0x68, 2},
			"sprop-parameter-sets=ZwE=,aAI="},
		{"H265", true, []byte{96, 1, 0, 3, 64, 1, 1, 0, 3, 66, 1, 2, 0, 3, 68, 1, 3},
			"sprop-vps=QAEB;sprop-sps=QgEC;sprop-pps=RAED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			forwarder := NewRTPForwarder()
			forwarder.ForwardVideoPacket(&rtp.Packet{Payload: tt.payload}, tt.hevc)
			server := &RTSPServer{}
			// Simulate stale camera metadata, including the wrong codec for HEVC.
			camera := &storage.CameraInfo{Skill: "{}"}
			sdp := server.generateSDP(camera, "rtsp://127.0.0.1/Camera/hd", "hd", forwarder)
			if !strings.Contains(sdp, "a=rtpmap:96 "+tt.name+"/90000\r\n") ||
				!strings.Contains(sdp, tt.want+"\r\n") {
				t.Fatalf("unexpected SDP:\n%s", sdp)
			}
			forwarder.Stop()
			_, fmtp := forwarder.videoDescription(tt.hevc)
			if strings.Contains(fmtp, "sprop-") {
				t.Fatalf("parameter sets survived stream shutdown: %s", fmtp)
			}
		})
	}
}

func TestVideoRTPUsesAdvertisedPayloadType(t *testing.T) {
	packet := &rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 98, SequenceNumber: 17, Timestamp: 9000},
		Payload: []byte{0x65, 1},
	}

	for _, transport := range []string{"UDP", "TCP"} {
		t.Run(transport, func(t *testing.T) {
			forwarder := NewRTPForwarder()
			t.Cleanup(forwarder.Stop)
			var data []byte
			var finish func()
			if transport == "UDP" {
				receiver, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { receiver.Close() })
				receiver.SetReadDeadline(time.Now().Add(time.Second))
				err = forwarder.AddUDPClient("client", receiver.LocalAddr().(*net.UDPAddr).Port, 0)
				if err != nil {
					t.Fatal(err)
				}
				finish = func() {
					buf := make([]byte, 1500)
					n, _, err := receiver.ReadFromUDP(buf)
					if err != nil {
						t.Fatal(err)
					}
					data = buf[:n]
				}
			} else {
				sender, receiver := net.Pipe()
				t.Cleanup(func() { sender.Close(); receiver.Close() })
				receiver.SetReadDeadline(time.Now().Add(time.Second))
				sender.SetWriteDeadline(time.Now().Add(time.Second))
				if err := forwarder.AddTCPClient("client", sender, 0, 2, 4); err != nil {
					t.Fatal(err)
				}
				finish = func() {
					buf := make([]byte, 1500)
					n, err := receiver.Read(buf)
					if err != nil {
						t.Fatal(err)
					}
					if n < 4 || buf[0] != '$' || buf[1] != 0 || int(binary.BigEndian.Uint16(buf[2:4])) != n-4 {
						t.Fatalf("unexpected RTSP interleaved frame: %x", buf[:n])
					}
					data = buf[4:n]
				}
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				forwarder.ForwardVideoPacket(packet, false)
			}()
			finish()
			<-done

			var got rtp.Packet
			if err := got.Unmarshal(data); err != nil {
				t.Fatal(err)
			}
			if got.PayloadType != 96 || got.SequenceNumber != 17 || got.Timestamp != 9000 || packet.PayloadType != 98 {
				t.Fatalf("unexpected outgoing packet: %+v (original payload type %d)", got.Header, packet.PayloadType)
			}
		})
	}
}

func TestRefusedUDPVideoPortLeavesAudioActive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("checks Linux connected UDP error delivery")
	}
	listen := func() *net.UDPConn {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	video, audio := listen(), listen()
	forwarder := NewRTPForwarder()
	t.Cleanup(forwarder.Stop)
	if err := forwarder.AddUDPClient("client", video.LocalAddr().(*net.UDPAddr).Port,
		audio.LocalAddr().(*net.UDPAddr).Port); err != nil {
		t.Fatal(err)
	}
	if err := video.Close(); err != nil {
		t.Fatal(err)
	}

	packet := &rtp.Packet{Header: rtp.Header{Version: 2}, Payload: []byte{0x65, 1}}
	client := forwarder.clients["client"]
	deadline := time.Now().Add(time.Second)
	for client.videoConn != nil && time.Now().Before(deadline) {
		forwarder.ForwardVideoPacket(packet, false)
		time.Sleep(time.Millisecond)
	}
	if client.videoConn != nil {
		t.Fatal("refused video socket was not retired")
	}
	audio.SetReadDeadline(time.Now().Add(time.Second))
	forwarder.ForwardAudioPacket(packet)
	if _, _, err := audio.ReadFromUDP(make([]byte, 1500)); err != nil {
		t.Fatalf("audio forwarding stopped with the video socket: %v", err)
	}
}

func TestDescribeWaitsForColdHEVCHeaders(t *testing.T) {
	forwarder := NewRTPForwarder()
	defer forwarder.Stop()
	sender, receiver := net.Pipe()
	defer sender.Close()
	defer receiver.Close()

	server := &RTSPServer{}
	client := &RTSPClient{
		conn: sender,
		stream: &CameraStream{
			camera:       &storage.CameraInfo{Skill: "{}"},
			resolution:   "hd",
			webrtcBridge: &WebRTCBridge{rtpForwarder: forwarder},
		},
	}
	go func() {
		defer sender.Close()
		server.handleDescribe(client, &RTSPRequest{URL: "rtsp://127.0.0.1/Camera/hd", CSeq: 2})
	}()

	if err := receiver.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.Read(make([]byte, 1)); err == nil {
		t.Fatal("DESCRIBE replied before video headers arrived")
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatal(err)
	}

	forwarder.ForwardVideoPacket(&rtp.Packet{Payload: []byte{64, 1, 1}}, true) // VPS
	forwarder.ForwardVideoPacket(&rtp.Packet{Payload: []byte{68, 1, 3}}, true) // PPS
	sendFragment := func(sequence uint16, flags, data byte) {
		forwarder.ForwardVideoPacket(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: sequence, Timestamp: 42},
			Payload: []byte{98, 1, flags | 33, data},
		}, true)
	}
	sendFragment(10, 0x80, 2)
	sendFragment(12, 0x40, 3) // Lost the next fragment.
	if forwarder.waitForVideoParameters(0) {
		t.Fatal("a parameter set with a sequence gap was accepted")
	}

	sendFragment(13, 0x80, 2)
	sendFragment(14, 0x40, 3)
	if err := receiver.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(receiver)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response), "sprop-vps=QAEB;sprop-sps=QgECAw==;sprop-pps=RAED") {
		t.Fatalf("unexpected DESCRIBE response: %s", response)
	}
}

func TestH264FragmentedParameterSet(t *testing.T) {
	forwarder := NewRTPForwarder()
	defer forwarder.Stop()
	forwarder.ForwardVideoPacket(&rtp.Packet{Payload: []byte{0x68, 3}}, false)
	forwarder.ForwardVideoPacket(&rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 65535},
		Payload: []byte{0x7c, 0x87, 1},
	}, false)
	forwarder.ForwardVideoPacket(&rtp.Packet{
		Header:  rtp.Header{SequenceNumber: 0},
		Payload: []byte{0x7c, 0x47, 2},
	}, false)

	codec, fmtp := forwarder.videoDescription(false)
	if codec != "H264" || !strings.Contains(fmtp, "sprop-parameter-sets=ZwEC,aAM=") {
		t.Fatalf("unexpected video format %s %s", codec, fmtp)
	}
}

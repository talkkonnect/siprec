package media

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"siprec-server/pkg/metrics"

	"github.com/sirupsen/logrus"
)

// buildRTPPacket crafts a minimal RTP packet (12-byte header + payload).
func buildRTPPacket(pt byte, seq uint16, ts, ssrc uint32, payload []byte) []byte {
	pkt := make([]byte, 12+len(payload))
	pkt[0] = 0x80 // version 2, no padding/extension
	pkt[1] = pt & 0x7F
	binary.BigEndian.PutUint16(pkt[2:], seq)
	binary.BigEndian.PutUint32(pkt[4:], ts)
	binary.BigEndian.PutUint32(pkt[8:], ssrc)
	copy(pkt[12:], payload)
	return pkt
}

// TestG722RecordingHeaderReconciled reproduces the FreeSWITCH re-INVITE ordering
// bug: StartRTPForwarding creates the WAV header BEFORE the codec is configured
// (so the header defaults to 8000 Hz), then the codec is set to G.722 and 16 kHz
// PCM is decoded and written. The header must be reconciled to 16000 Hz, or the
// recording plays back at half speed.
func TestG722RecordingHeaderReconciled(t *testing.T) {
	metrics.EnableMetrics(false)

	logger := logrus.New()
	logger.SetOutput(io.Discard)

	tempDir := t.TempDir()

	config := &Config{
		RTPBindIP:    "127.0.0.1",
		RecordingDir: tempDir,
		// Jitter buffer and audio processing off to keep the path simple.
	}

	callUUID := "g722-header-test"
	sttProvider := func(ctx context.Context, vendor string, reader io.Reader, callUUID string) error {
		_, _ = io.Copy(io.Discard, reader)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bound := func(f *RTPForwarder) bool {
		f.CleanupMutex.Lock()
		defer f.CleanupMutex.Unlock()
		return f.Conn != nil
	}

	// Allocated ports may collide with a real siprec process on the same host,
	// so retry across forwarders until one actually binds its UDP listener.
	var forwarder *RTPForwarder
	for attempt := 0; attempt < 20; attempt++ {
		f, err := NewRTPForwarder(30*time.Second, nil, logger, false, nil)
		if err != nil {
			t.Fatalf("NewRTPForwarder: %v", err)
		}
		// NOTE: codec is intentionally NOT configured before StartRTPForwarding,
		// so the WAV header is created at the 8000 Hz default — exactly the
		// re-INVITE ordering the earlier "only fix when codec was empty" missed.
		StartRTPForwarding(ctx, f, callUUID, config, sttProvider)

		deadline := time.Now().Add(500 * time.Millisecond)
		for !bound(f) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if bound(f) {
			forwarder = f
			break
		}
		f.Stop()
		f.WaitDone(time.Second)
		f.Cleanup()
	}
	if forwarder == nil {
		t.Fatal("could not bind an RTP listener after 20 attempts")
	}
	defer forwarder.Cleanup()

	// Now configure G.722 (rtpmap advertises the 8 kHz clock), mirroring
	// ConfigureForwarderForMediaDescription running AFTER StartRTPForwarding.
	forwarder.SetCodecInfo(9, "G722", 8000, 1)

	// Send G.722 RTP packets (PT 9) to the forwarder over loopback.
	dst := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: forwarder.LocalPort}
	conn, err := net.DialUDP("udp", nil, dst)
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	defer conn.Close()

	payload := make([]byte, 160) // 20ms of G.722 at 64 kbit/s
	for i := range payload {
		payload[i] = byte(i)
	}
	var ts uint32 = 1000
	for seq := uint16(100); seq < 130; seq++ {
		pkt := buildRTPPacket(9, seq, ts, 0x1234abcd, payload)
		if _, err := conn.Write(pkt); err != nil {
			t.Fatalf("send packet: %v", err)
		}
		ts += 160 // G.722 RTP timestamp advances at the 8 kHz clock
		time.Sleep(5 * time.Millisecond)
	}

	// Give the goroutine time to drain and write.
	time.Sleep(300 * time.Millisecond)

	// Stop forwarding so the WAV is finalized.
	forwarder.Stop()
	forwarder.WaitDone(2 * time.Second)

	recordingPath := filepath.Join(tempDir, "g722-header-test.wav")
	f, err := os.Open(recordingPath)
	if err != nil {
		t.Fatalf("open recording %s: %v", recordingPath, err)
	}
	defer f.Close()

	hdr := make([]byte, 44)
	if _, err := io.ReadFull(f, hdr); err != nil {
		t.Fatalf("read header: %v", err)
	}
	sampleRate := binary.LittleEndian.Uint32(hdr[24:28])
	dataSize := binary.LittleEndian.Uint32(hdr[40:44])

	if sampleRate != 16000 {
		t.Errorf("WAV header sample rate = %d, want 16000 (G.722 half-speed regression)", sampleRate)
	}
	if dataSize == 0 {
		t.Error("WAV has no audio data; packets were not recorded")
	}
	// G.722 decodes 2 PCM samples per payload byte: 160 bytes -> 640 PCM bytes/packet.
	t.Logf("recorded %d bytes of PCM at %d Hz", dataSize, sampleRate)
}

// TestWidebandG711HeaderMeasured reproduces the real FreeSWITCH case that the
// codec-table lookup cannot catch: 16 kHz G.711 μ-law carried on static payload
// type 0. RFC 3551 maps PT 0 to PCMU/8000, so the header is initially written at
// 8000, but the stream actually delivers 320 samples every 20 ms (16 kHz). The
// RTP-clock measurement must detect the true rate and rewrite the header to
// 16000, or the recording plays back at half speed.
func TestWidebandG711HeaderMeasured(t *testing.T) {
	metrics.EnableMetrics(false)

	logger := logrus.New()
	logger.SetOutput(io.Discard)

	tempDir := t.TempDir()

	config := &Config{
		RTPBindIP:    "127.0.0.1",
		RecordingDir: tempDir,
	}

	callUUID := "wideband-g711-test"
	sttProvider := func(ctx context.Context, vendor string, reader io.Reader, callUUID string) error {
		_, _ = io.Copy(io.Discard, reader)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bound := func(f *RTPForwarder) bool {
		f.CleanupMutex.Lock()
		defer f.CleanupMutex.Unlock()
		return f.Conn != nil
	}

	var forwarder *RTPForwarder
	for attempt := 0; attempt < 20; attempt++ {
		f, err := NewRTPForwarder(30*time.Second, nil, logger, false, nil)
		if err != nil {
			t.Fatalf("NewRTPForwarder: %v", err)
		}
		// PT 0 → PCMU/8000 from the codec table, so the header starts at 8000.
		f.SetCodecInfo(0, "PCMU", 8000, 1)
		StartRTPForwarding(ctx, f, callUUID, config, sttProvider)
		deadline := time.Now().Add(500 * time.Millisecond)
		for !bound(f) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if bound(f) {
			forwarder = f
			break
		}
		f.Stop()
		f.WaitDone(time.Second)
		f.Cleanup()
	}
	if forwarder == nil {
		t.Fatal("could not bind an RTP listener after 20 attempts")
	}
	defer forwarder.Cleanup()

	dst := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: forwarder.LocalPort}
	conn, err := net.DialUDP("udp", nil, dst)
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	defer conn.Close()

	// 320-byte μ-law payload = 320 samples per packet. Sending one every ~20 ms
	// while advancing the RTP timestamp by 320 yields a measured clock of 16 kHz.
	// Run past the 2 s measurement threshold.
	payload := make([]byte, 320)
	for i := range payload {
		payload[i] = 0xFF // μ-law silence
	}
	const samplesPerPacket = 320
	var ts uint32 = 5000
	start := time.Now()
	seq := uint16(200)
	for time.Since(start) < 2500*time.Millisecond {
		pkt := buildRTPPacket(0, seq, ts, 0x55667788, payload)
		if _, err := conn.Write(pkt); err != nil {
			t.Fatalf("send packet: %v", err)
		}
		seq++
		ts += samplesPerPacket
		time.Sleep(20 * time.Millisecond)
	}

	time.Sleep(300 * time.Millisecond)
	forwarder.Stop()
	forwarder.WaitDone(2 * time.Second)

	recordingPath := filepath.Join(tempDir, "wideband-g711-test.wav")
	f, err := os.Open(recordingPath)
	if err != nil {
		t.Fatalf("open recording %s: %v", recordingPath, err)
	}
	defer f.Close()

	hdr := make([]byte, 44)
	if _, err := io.ReadFull(f, hdr); err != nil {
		t.Fatalf("read header: %v", err)
	}
	sampleRate := binary.LittleEndian.Uint32(hdr[24:28])
	if sampleRate != 16000 {
		t.Errorf("WAV header sample rate = %d, want 16000 (wideband G.711 half-speed regression)", sampleRate)
	}
}

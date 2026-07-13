package media

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func readWAVSampleRate(t *testing.T, path string) uint32 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	hdr := make([]byte, 44)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		t.Fatalf("read header: %v", err)
	}
	return binary.LittleEndian.Uint32(hdr[24:28])
}

func readWAVDataSize(t *testing.T, path string) uint32 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	hdr := make([]byte, 44)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		t.Fatalf("read header: %v", err)
	}
	return binary.LittleEndian.Uint32(hdr[40:44])
}

// TestWAVWriterSetFormatAfterData reproduces the "half-speed recording" bug's
// core mechanic: the WAV header is created with the wrong (pre-codec-config)
// sample rate, audio is written, and only later is the true codec known. The
// header must be rewritable in place to the correct rate without losing data.
func TestWAVWriterSetFormatAfterData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "late-codec.wav")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Header initially created at the stale default (codec not yet configured).
	w, err := NewWAVWriter(f, 8000, 1)
	if err != nil {
		t.Fatalf("NewWAVWriter: %v", err)
	}

	// A packet arrives and G.722 PCM (16 kHz) gets decoded and written before
	// the header is reconciled — mirrors the RTP loop's ordering.
	pcm := make([]byte, 640) // 20ms of 16 kHz 16-bit mono
	if _, err := w.Write(pcm); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Reconcile to the decoder's real output rate (what the RTP loop now does
	// every packet via OutputSampleRate("G722", 8000) = 16000).
	if err := w.SetFormat(OutputSampleRate("G722", 8000), 1); err != nil {
		t.Fatalf("SetFormat: %v", err)
	}
	if _, err := w.Write(pcm); err != nil {
		t.Fatalf("Write after SetFormat: %v", err)
	}
	if err := w.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	f.Close()

	if got := readWAVSampleRate(t, path); got != 16000 {
		t.Errorf("WAV header sample rate = %d, want 16000 (half-speed bug regression)", got)
	}
	if got := readWAVDataSize(t, path); got != uint32(len(pcm)*2) {
		t.Errorf("WAV data size = %d, want %d (data lost during header rewrite)", got, len(pcm)*2)
	}
}

// TestWAVWriterSetFormatNoopWhenUnchanged guards that reconciling every packet
// (calling SetFormat with the same values repeatedly) does not corrupt the file
// or churn the header.
func TestWAVWriterSetFormatNoopWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "noop.wav")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	w, err := NewWAVWriter(f, 16000, 1)
	if err != nil {
		t.Fatalf("NewWAVWriter: %v", err)
	}
	pcm := make([]byte, 320)
	for i := 0; i < 5; i++ {
		if _, err := w.Write(pcm); err != nil {
			t.Fatalf("Write: %v", err)
		}
		// Same rate every packet — must be a cheap no-op.
		if err := w.SetFormat(16000, 1); err != nil {
			t.Fatalf("SetFormat noop: %v", err)
		}
	}
	if err := w.Finalize(); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	f.Close()

	if got := readWAVSampleRate(t, path); got != 16000 {
		t.Errorf("sample rate = %d, want 16000", got)
	}
	if got := readWAVDataSize(t, path); got != uint32(5*len(pcm)) {
		t.Errorf("data size = %d, want %d", got, 5*len(pcm))
	}
}

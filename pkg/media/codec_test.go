package media

import "testing"

// TestOutputSampleRate guards the G.722 "half-speed recording" bug: G.722's
// rtpmap clock rate is advertised as 8000 Hz per RFC 3551, but the decoder
// produces 16 kHz PCM, so recordings must be written with a 16 kHz header.
func TestOutputSampleRate(t *testing.T) {
	cases := []struct {
		name       string
		codec      string
		rtpClock   int
		wantOutput int
	}{
		{"G722 with advertised 8k clock", "G722", 8000, 16000},
		{"G722 dotted name", "G.722", 8000, 16000},
		{"G722 lowercase", "g722", 8000, 16000},
		{"PCMU stays 8k", "PCMU", 8000, 8000},
		{"PCMA stays 8k", "PCMA", 8000, 8000},
		{"G729 stays 8k", "G729", 8000, 8000},
		{"Opus keeps advertised 48k", "OPUS", 48000, 48000},
		{"unknown falls back to clock", "FOO", 32000, 32000},
		{"zero clock falls back to 8k", "PCMU", 0, 8000},
		{"empty codec zero clock", "", 0, 8000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OutputSampleRate(tc.codec, tc.rtpClock); got != tc.wantOutput {
				t.Errorf("OutputSampleRate(%q, %d) = %d, want %d", tc.codec, tc.rtpClock, got, tc.wantOutput)
			}
		})
	}
}

// TestNearestStandardRate guards the empirical-rate snapping used to detect
// mislabeled clock rates (e.g. 16 kHz G.711 measured as ~15.7 kHz must snap to
// 16000, not the payload-type-implied 8000).
func TestNearestStandardRate(t *testing.T) {
	cases := []struct {
		measured float64
		want     int
	}{
		{7900, 8000},
		{8200, 8000},
		{15660, 16000}, // real measurement from a 16 kHz μ-law stream
		{16800, 16000},
		{11999, 8000},  // just below the 8k/16k midpoint
		{12001, 16000}, // just above it
		{31000, 32000},
		{47000, 48000},
		{0, 0},
		{-5, 0},
	}
	for _, tc := range cases {
		if got := nearestStandardRate(tc.measured); got != tc.want {
			t.Errorf("nearestStandardRate(%v) = %d, want %d", tc.measured, got, tc.want)
		}
	}
}

package main

import (
	"testing"

	"github.com/Beam-Network/beam/internal/workload/contracts"
)

func TestRoomMediaStartupFailsClosed(t *testing.T) {
	webRTC := []string{"room.media", contracts.RoomMediaWebRTCCapability}
	cases := []struct {
		name         string
		capabilities []string
		advertiseURL string
		valid        bool
	}{
		{"legacy media", []string{"room.media"}, "", true},
		{"pinned https", webRTC, "https://worker.example.com:{port}", true},
		{"missing room.media", []string{contracts.RoomMediaWebRTCCapability}, "https://worker.example.com:9460", false},
		{"missing advertise url", webRTC, "", false},
		{"plain http", webRTC, "http://worker.example.com:9460", false},
		{"query", webRTC, "https://worker.example.com:9460?token=value", false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := validateRoomMediaStartup(test.capabilities, test.advertiseURL)
			if (err == nil) != test.valid {
				t.Fatalf("validateRoomMediaStartup(%v, %q) = %v", test.capabilities, test.advertiseURL, err)
			}
		})
	}
}

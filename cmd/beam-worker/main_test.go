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

func TestRoomDirectStartupFailsClosed(t *testing.T) {
	message := []string{"room.message", contracts.RoomMessageDirectCapability}
	stream := []string{"room.stream", contracts.RoomStreamDirectCapability}
	both := append(append([]string{}, message...), stream...)
	listen, advertise := "0.0.0.0:9480", "https://worker.example.com:{port}"
	cases := []struct {
		name         string
		capabilities []string
		listen       string
		advertise    string
		valid        bool
	}{
		{"relay message only", []string{"room.message"}, "", "", true},
		{"direct message", message, listen, advertise, true},
		{"direct stream", stream, listen, advertise, true},
		{"message and stream share the listener", both, listen, advertise, true},
		{"direct message without base", []string{contracts.RoomMessageDirectCapability}, listen, advertise, false},
		{"direct stream without base", []string{contracts.RoomStreamDirectCapability}, listen, advertise, false},
		{"relay stream", []string{"room.stream"}, listen, advertise, false},
		{"direct stream without listen address", stream, "", advertise, false},
		{"direct stream without advertise URL", stream, listen, "", false},
		{"direct stream with plain http", stream, listen, "http://worker.example.com:9480", false},
		{"direct stream with query", stream, listen, "https://worker.example.com:9480?token=value", false},
		{"direct message without listen address", message, "", advertise, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := validateRoomDirectStartup(test.capabilities, test.listen, test.advertise)
			if (err == nil) != test.valid {
				t.Fatalf("validateRoomDirectStartup(%v, %q, %q) = %v", test.capabilities, test.listen, test.advertise, err)
			}
		})
	}
}

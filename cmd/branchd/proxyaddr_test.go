package main

import "testing"

func TestAdvertisedProxy(t *testing.T) {
	cases := []struct {
		advertise, pgAddr string
		host              string
		port              int
		wantErr           bool
	}{
		{"", ":6432", "", 6432, false},          // default: port only, client uses the API host
		{"", "0.0.0.0:5433", "", 5433, false},   // the bind host is never advertised
		{"", "127.0.0.1:6432", "", 6432, false}, // nor is loopback
		{"", "not-an-addr", "", 0, false},       // unreadable: advertise nothing
		{"pgoverlay-proxy.ns:6432", ":7000", "pgoverlay-proxy.ns", 6432, false},
		{"[2001:db8::1]:6432", ":6432", "2001:db8::1", 6432, false},
		{":5433", ":6432", "", 5433, false},
		{"pgoverlay-proxy.ns", ":6432", "", 0, true},   // no port
		{"pgoverlay-proxy.ns:0", ":6432", "", 0, true}, // bad port
		{"pgoverlay-proxy.ns:x", ":6432", "", 0, true}, // bad port
	}
	for _, tc := range cases {
		host, port, err := advertisedProxy(tc.advertise, tc.pgAddr)
		if (err != nil) != tc.wantErr {
			t.Errorf("advertisedProxy(%q, %q) err = %v, wantErr %v", tc.advertise, tc.pgAddr, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && (host != tc.host || port != tc.port) {
			t.Errorf("advertisedProxy(%q, %q) = %q, %d; want %q, %d", tc.advertise, tc.pgAddr, host, port, tc.host, tc.port)
		}
	}
}

package main

import "testing"

func TestHealthAddressFollowsWhereThePageListens(t *testing.T) {
	cases := map[string]string{
		":28761":              "127.0.0.1:28761",
		"0.0.0.0:28761":       "127.0.0.1:28761",
		"[::]:28761":          "127.0.0.1:28761",
		"192.168.10.20:28761": "192.168.10.20:28761",
		"127.0.0.1:28769":     "127.0.0.1:28769",
		"[fd00::5]:28761":     "[fd00::5]:28761",
	}
	for listen, want := range cases {
		if address, err := healthAddress(listen); err != nil || address != want {
			t.Errorf("healthAddress(%q) = %q, %v; want %q", listen, address, err, want)
		}
	}
	if _, err := healthAddress("28761"); err == nil {
		t.Error("a -listen without a colon should be an error, not a panic")
	}
}

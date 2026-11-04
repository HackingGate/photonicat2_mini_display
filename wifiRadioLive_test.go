package main

import (
	"image"
	"testing"
	"time"
)

const sampleIwinfo = `phy0-ap0  ESSID: "photonicat2-46a4d89f"
          Access Point: 9A:11:22:33:44:55
          Mode: Master  Channel: 149 (5.745 GHz)  HT Mode: HE80
          Center Channel 1: 155 2: unknown
          Tx-Power: 23 dBm  Link Quality: unknown/70
          Encryption: WPA2 PSK (CCMP)
          Supports VAPs: yes  PHY name: phy0

phy1-ap0  ESSID: "pcat"
          Access Point: 9A:11:22:33:44:56
          Mode: Master  Channel: 6 (2.437 GHz)  HT Mode: HT20

phy2-sta0 ESSID: unknown
          Access Point: 00:00:00:00:00:00
          Mode: Client  Channel: unknown (unknown)  HT Mode: unknown

phy3-ap0  ESSID: "pcat"
          Mode: Master  Channel: 37 (6.135 GHz)  HT Mode: EHT320
`

func withWifiLive(t *testing.T, live []wifiRadioLive) {
	t.Helper()
	wifiLiveMu.Lock()
	sLive, sAt := wifiLive, wifiLiveAt
	wifiLive, wifiLiveAt = live, time.Now()
	wifiLiveMu.Unlock()
	t.Cleanup(func() {
		wifiLiveMu.Lock()
		wifiLive, wifiLiveAt = sLive, sAt
		wifiLiveMu.Unlock()
	})
}

func TestParseIwinfoAPs(t *testing.T) {
	got := parseIwinfoAPs(sampleIwinfo)
	want := []wifiRadioLive{
		{SSID: "photonicat2-46a4d89f", Channel: 149, FreqMHz: 5745, WidthMHz: 80},
		{SSID: "pcat", Channel: 6, FreqMHz: 2437, WidthMHz: 20},
		{SSID: "pcat", Channel: 37, FreqMHz: 6135, WidthMHz: 320},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d APs %+v, want %d (the client iface must be skipped)", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AP %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if htmodeWidthMHz("NOHT") != 20 || htmodeWidthMHz("VHT160") != 160 {
		t.Error("htmodeWidthMHz")
	}
}

func TestWifiQrRadioLine(t *testing.T) {
	withWifiLive(t, parseIwinfoAPs(sampleIwinfo))
	if got := wifiQrRadioLine(wifiQrNet{SSID: "photonicat2-46a4d89f", Bands: []string{"5g"}}); got != "5.745GHz • 80MHz • ch149" {
		t.Errorf("single radio: %q", got)
	}
	if got := wifiQrRadioLine(wifiQrNet{SSID: "pcat", Bands: []string{"2g", "6g"}}); got != "ch6 20MHz • ch37 320MHz" {
		t.Errorf("merged: %q", got)
	}
	if got := wifiQrRadioLine(wifiQrNet{SSID: "pcat", Bands: []string{"5g"}}); got != "" {
		t.Errorf("no live 5g match should be empty, got %q", got)
	}
}

// The Wi-Fi page is left out of the dots; on it, the footer shows the radio
// line, so the footer differs from the dotted one.
func TestFooterSkipsWifiPages(t *testing.T) {
	withWifiQrState(t)
	fonts = buildFontTable(".")
	assetsPrefix = "."
	withWifiLive(t, parseIwinfoAPs(sampleIwinfo))
	cfg.ShowWifiQr = nil
	cfgNumPages = 4
	gpsPageConfigured = false
	updateWifiQrPages([]WiFiInterface{{Exist: true, Enabled: true, Band: "5g", SSID: "photonicat2-46a4d89f", Password: "pw", Encryption: "psk2"}})
	if cfgNumPages != 5 {
		t.Fatalf("cfgNumPages = %d", cfgNumPages)
	}
	render := func(page int) *image.RGBA {
		cacheFooterStr = ""
		f := image.NewRGBA(image.Rect(0, 0, PCAT2_LCD_WIDTH, PCAT2_FOOTER_HEIGHT))
		if !renderFooter(f, page, cfgNumPages, false) {
			t.Fatalf("renderFooter(%d) did not draw", page)
		}
		return f
	}
	// 4 dots on a template page: the centred row spans (4-1)*26 + 16 px.
	f := render(0)
	minX, maxX := PCAT2_LCD_WIDTH, -1
	for y := 0; y < PCAT2_FOOTER_HEIGHT; y++ {
		for x := 0; x < PCAT2_LCD_WIDTH; x++ {
			if c := f.RGBAAt(x, y); int(c.R)+int(c.G)+int(c.B) > 120 {
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
			}
		}
	}
	if w := maxX - minX + 1; w > 3*26+16+2 {
		t.Errorf("dot row is %dpx wide: the Wi-Fi page is still counted", w)
	}
	cacheFooterStr = ""
	if got := wifiQrFooterLine(4); got != "5.745GHz • 80MHz • ch149" {
		t.Errorf("footer line on the Wi-Fi page = %q", got)
	}
	render(4)
	cacheFooterStr = ""
}

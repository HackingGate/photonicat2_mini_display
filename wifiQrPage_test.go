package main

import (
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// withWifiQrState saves and restores every global the Wi-Fi pages mutate.
func withWifiQrState(t *testing.T) {
	t.Helper()
	withGpsState(t)
	sPages, sNum, sIfs, sHave := wifiQrPages, wifiQrPageNum, wifiQrLastIfs, wifiQrHaveIfs
	sShow, sForce := cfg.ShowWifiQr, forceMiddleRedraw
	t.Cleanup(func() {
		wifiQrPages, wifiQrPageNum, wifiQrLastIfs, wifiQrHaveIfs = sPages, sNum, sIfs, sHave
		cfg.ShowWifiQr, forceMiddleRedraw = sShow, sForce
	})
	wifiQrPages, wifiQrPageNum, wifiQrLastIfs, wifiQrHaveIfs = nil, 0, nil, false
}

func boolp(b bool) *bool { return &b }

func sampleIfs() []WiFiInterface {
	return []WiFiInterface{
		{Exist: true, Enabled: true, Band: "5g", DeviceType: "PCIE", Htmode: "VHT80", SSID: "pcat-5G", Password: "secret55", Encryption: "psk2", Hidden: "0"},
		{Exist: true, Enabled: true, Band: "2g", DeviceType: "Onboard", Htmode: "HE20", SSID: "pcat", Password: "secret24", Encryption: "sae", Hidden: "0"},
	}
}

func TestWifiQrPayload(t *testing.T) {
	cases := []struct {
		n    wifiQrNet
		want string
	}{
		{wifiQrNet{SSID: "home", Password: "pw", Encryption: "psk2"}, "WIFI:T:WPA;S:home;P:pw;;"},
		{wifiQrNet{SSID: "home", Password: "pw", Encryption: "sae"}, "WIFI:T:WPA;S:home;P:pw;;"},
		{wifiQrNet{SSID: "cafe", Encryption: "none"}, "WIFI:T:nopass;S:cafe;;"},
		{wifiQrNet{SSID: "cafe", Encryption: ""}, "WIFI:T:nopass;S:cafe;;"},
		{wifiQrNet{SSID: "old", Password: "12345", Encryption: "wep-open"}, "WIFI:T:WEP;S:old;P:12345;;"},
		{wifiQrNet{SSID: `a;b,c:d"e\f`, Password: `p;w`, Encryption: "psk2", Hidden: true},
			`WIFI:T:WPA;S:a\;b\,c\:d\"e\\f;P:p\;w;H:true;;`},
	}
	for _, c := range cases {
		if got := wifiQrPayload(c.n); got != c.want {
			t.Errorf("wifiQrPayload(%+v) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestWifiQrNetworksFilterSortMerge(t *testing.T) {
	ifs := append(sampleIfs(),
		WiFiInterface{Exist: true, Enabled: false, Band: "2g", SSID: "off"},
		WiFiInterface{Exist: true, Enabled: true, Band: "2g", SSID: "relay", WiFiWan: true},
		WiFiInterface{Exist: true, Enabled: true, Band: "2g", SSID: "nokey", Encryption: "psk2"},
		WiFiInterface{Exist: true, Enabled: true, Band: "2g", SSID: ""},
	)
	nets := wifiQrNetworks(ifs)
	if len(nets) != 2 {
		t.Fatalf("got %d networks, want 2: %+v", len(nets), nets)
	}
	if nets[0].SSID != "pcat" || nets[1].SSID != "pcat-5G" {
		t.Errorf("order = %q,%q; want 2.4GHz first", nets[0].SSID, nets[1].SSID)
	}
	if wifiQrTitle(nets[0]) != "2.4GHz WiFi 6" || wifiQrTitle(nets[1]) != "5GHz WiFi 5" {
		t.Errorf("titles = %q,%q", wifiQrTitle(nets[0]), wifiQrTitle(nets[1]))
	}
	if wifiQrRadioTag(nets[0]) != "Onboard" || wifiQrRadioTag(nets[1]) != "PCIE" {
		t.Errorf("radio tags = %q,%q", wifiQrRadioTag(nets[0]), wifiQrRadioTag(nets[1]))
	}

	// Same SSID+key on both radios (band steering) is one network, one page.
	same := []WiFiInterface{
		{Exist: true, Enabled: true, Band: "5g", DeviceType: "PCIE", Htmode: "EHT160", SSID: "pcat", Password: "k", Encryption: "psk2"},
		{Exist: true, Enabled: true, Band: "2g", DeviceType: "Builtin", Htmode: "HE40", SSID: "pcat", Password: "k", Encryption: "psk2"},
	}
	nets = wifiQrNetworks(same)
	if len(nets) != 1 || wifiQrTitle(nets[0]) != "2.4/5GHz WiFi 6/7" || wifiQrRadioTag(nets[0]) != "Onboard/PCIE" {
		t.Errorf("merged = %+v (title %q), want one '2.4/5GHz WiFi 6/7' page tagged Onboard/PCIE", nets, wifiQrTitle(nets[0]))
	}
}

func TestWifiQrEnabledDefaultsOn(t *testing.T) {
	if !wifiQrEnabled(&Config{}) {
		t.Error("absent show_wifi_qr must mean on")
	}
	if wifiQrEnabled(&Config{ShowWifiQr: boolp(false)}) {
		t.Error("show_wifi_qr=false must mean off")
	}
	for _, p := range []string{"config.json", "config_debian.json"} {
		c, err := loadConfig(p)
		if err != nil {
			t.Fatalf("loadConfig(%s): %v", p, err)
		}
		if c.ShowWifiQr == nil || !*c.ShowWifiQr {
			t.Errorf("%s must ship show_wifi_qr: true", p)
		}
	}
}

// Wi-Fi pages ride after the template pages (GPS included) and before SMS.
func TestWifiQrPageCounts(t *testing.T) {
	withWifiQrState(t)
	cfg.ShowSms = true
	lenSmsPagesImages = 2
	cfg.ShowWifiQr = nil
	cfgNumPages = 4
	gpsPageConfigured, gpsBaseCfgPages, gpsPageShown = true, 5, false
	currPageIdx = 0

	updateWifiQrPages(sampleIfs())
	if cfgNumPages != 6 || totalNumPages != 8 || wifiQrPageCount() != 2 {
		t.Fatalf("cfg/total/wifi = %d/%d/%d, want 6/8/2", cfgNumPages, totalNumPages, wifiQrPageCount())
	}
	if templatePageCount() != 4 || isWifiQrPage(3) || !isWifiQrPage(4) || !isWifiQrPage(5) || isWifiQrPage(6) {
		t.Error("isWifiQrPage boundaries wrong")
	}

	// GPS appearing keeps the Wi-Fi pages and shifts them by one.
	updateGpsPagePresence(true)
	if cfgNumPages != 7 || templatePageCount() != 5 || totalNumPages != 9 {
		t.Errorf("after GPS show: cfg/templ/total = %d/%d/%d, want 7/5/9", cfgNumPages, templatePageCount(), totalNumPages)
	}

	// Turning the feature off drops them; the panel leaves a vanished page.
	currPageIdx = 6
	httpChangePageTriggered = false
	cfg.ShowWifiQr = boolp(false)
	updateWifiQrPages(sampleIfs())
	if cfgNumPages != 5 || totalNumPages != 7 || wifiQrPageCount() != 0 {
		t.Errorf("after off: cfg/total/wifi = %d/%d/%d, want 5/7/0", cfgNumPages, totalNumPages, wifiQrPageCount())
	}
	if !httpChangePageTriggered {
		t.Error("removing the page under the cursor must kick a page change")
	}
}

func TestWifiGeneration(t *testing.T) {
	for _, c := range []struct{ mode, band, want string }{
		{"HT20", "2g", "4"}, {"VHT80", "5g", "5"}, {"HE80", "5g", "6"}, {"HE160", "6g", "6E"},
		{"EHT320", "6g", "7"}, {"eht80", "5g", "7"}, {"NOHT", "2g", ""}, {"", "5g", ""},
	} {
		if got := wifiGeneration(c.mode, c.band); got != c.want {
			t.Errorf("wifiGeneration(%q,%q) = %q, want %q", c.mode, c.band, got, c.want)
		}
	}
	if got := wifiQrTitle(wifiQrNet{Bands: []string{"5g"}}); got != "5GHz" {
		t.Errorf("unknown generation: title %q, want 5GHz", got)
	}
}

// The page follows the /screen colour theme: a themed layout's roles come
// back as the page palette, and the default layout gives PCAT yellow/white.
func TestWifiQrThemePalette(t *testing.T) {
	withWifiQrState(t)
	sEls, sVer := cfg.DisplayTemplate.Elements, configVersion
	t.Cleanup(func() { cfg.DisplayTemplate.Elements, configVersion = sEls, sVer })

	cfg.DisplayTemplate.Elements = map[string][]DisplayElement{
		"page0": {
			{Type: "icon", Color: []int{1, 2, 3}},
			{Type: "text", Font: "huge", Color: []int{0, 229, 255}},
			{Type: "fixed_text", Font: "tiny", Color: []int{255, 64, 160}},
			{Type: "text", Font: "tiny", Color: []int{225, 242, 255}},
		},
	}
	cfgNumPages = 1
	configVersion += 1000
	p := wifiQrThemePalette()
	want := wifiQrPalette{
		primary: color.RGBA{0, 229, 255, 255},
		accent:  color.RGBA{255, 64, 160, 255},
		data:    color.RGBA{225, 242, 255, 255},
	}
	if p != want {
		t.Errorf("neon layout palette = %+v, want %+v", p, want)
	}

	// Cached until the next merge bumps configVersion (a theme apply always
	// re-merges), then picked up.
	cfg.DisplayTemplate.Elements["page0"][1].Color = []int{70, 235, 145}
	if wifiQrThemePalette().primary != want.primary {
		t.Error("palette recomputed without a configVersion bump")
	}
	configVersion++
	if got := wifiQrThemePalette().primary; got != (color.RGBA{70, 235, 145, 255}) {
		t.Errorf("after theme change primary = %v", got)
	}
}

func TestWifiQrInfoLine(t *testing.T) {
	sv, had := globalData.Load("WiFiClientsCount")
	t.Cleanup(func() {
		if had {
			globalData.Store("WiFiClientsCount", sv)
		} else {
			globalData.Delete("WiFiClientsCount")
		}
	})
	globalData.Delete("WiFiClientsCount")
	if got, c := wifiQrInfoLine(wifiQrNet{Encryption: "psk2"}); got != "WPA2" || c {
		t.Errorf("no count yet: %q,%v, want WPA2 without count", got, c)
	}
	globalData.Store("WiFiClientsCount", 3)
	if got, c := wifiQrInfoLine(wifiQrNet{Encryption: "sae", Radios: []string{"Onboard"}}); got != "Onboard • WPA3 • 3" || !c {
		t.Errorf("got %q,%v", got, c)
	}
	globalData.Store("WiFiClientsCount", 1)
	if got, _ := wifiQrInfoLine(wifiQrNet{Encryption: "none"}); got != "Open • 1" {
		t.Errorf("got %q", got)
	}
}

// The page draws a QR block and its fingerprint follows the payload.
func TestDrawWifiQrPage(t *testing.T) {
	withWifiQrState(t)
	fonts = buildFontTable(".")
	cfg.ShowWifiQr = nil
	cfgNumPages = 4
	gpsPageConfigured = false
	updateWifiQrPages(sampleIfs())

	globalData.Store("WiFiClientsCount", 3)
	fp1 := middlePageFingerprint(&cfg, false, 4)
	frame := image.NewRGBA(image.Rect(0, 0, middleFrameWidth, middleFrameHeight))
	renderMiddle(frame, &cfg, false, 4)
	if c := frame.RGBAAt(middleFrameWidth/2, wifiQrBlockY+2); c != PCAT_WHITE {
		t.Errorf("QR quiet zone pixel = %v, want white", c)
	}

	ifs := sampleIfs()
	ifs[1].Password = "changed!"
	updateWifiQrPages(ifs)
	if fp2 := middlePageFingerprint(&cfg, false, 4); fp2 == fp1 {
		t.Error("fingerprint must change when the key changes")
	}

	// "pcat" fits; a 20-char SSID at the title size must tick.
	if wifiQrSsidOverflows(0) {
		t.Error("short SSID must not scroll")
	}
	long := sampleIfs()
	long[1].SSID = "photonicat2-46a4d89f"
	updateWifiQrPages(long)
	if !wifiQrSsidOverflows(0) || !pageHasOverflowText(&cfg, 4) {
		t.Error("long SSID must scroll and keep the page re-rendering")
	}

	if os.Getenv("WIFI_QR_PREVIEW") == "1" {
		for i := 0; i < wifiQrPageCount(); i++ {
			f := image.NewRGBA(image.Rect(0, 0, middleFrameWidth, middleFrameHeight))
			renderMiddle(f, &cfg, false, templatePageCount()+i)
			out, err := os.Create("wifi_qr_preview" + strconv.Itoa(i) + ".png")
			if err != nil {
				t.Fatal(err)
			}
			png.Encode(out, f)
			out.Close()
		}
	}
}

// An upgrade keeps the feature on: a user_config written by older firmware
// has no show_wifi_qr key, and that must merge to the shipped "true". An
// explicit false from the web toggle must survive the merge.
func TestWifiQrMergeDefaultsOnAfterUpgrade(t *testing.T) {
	withWifiQrState(t)
	sDft, sUser, sCfg := dftCfg, userCfg, cfg
	t.Cleanup(func() { dftCfg, userCfg, cfg = sDft, sUser, sCfg })

	d, err := loadConfig("config.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		user string
		want bool
	}{
		{`{"ping_site0": "8.8.8.8", "show_sms": false}`, true},
		{`{"show_wifi_qr": false}`, false},
		{`{"show_wifi_qr": true}`, true},
	} {
		dftCfg = d
		userCfg = Config{}
		if err := json.Unmarshal([]byte(c.user), &userCfg); err != nil {
			t.Fatal(err)
		}
		if err := mergeConfigs(); err != nil {
			t.Fatalf("mergeConfigs: %v", err)
		}
		if got := wifiQrEnabled(&cfg); got != c.want {
			t.Errorf("user %s: wifiQrEnabled = %v, want %v", c.user, got, c.want)
		}
		// Saving the user config back must not invent the key.
		out, _ := json.Marshal(userCfg)
		if c.user[2:7] == "ping_" && strings.Contains(string(out), "show_wifi_qr") {
			t.Errorf("re-saved user config gained show_wifi_qr: %s", out)
		}
	}
}

// The page is laid out by ink, not by font boxes: title and SSID start on
// x=6 (the QR modules' column), the two bottom lines are centred with equal
// visible margins, and every vertical gap —
// title → SSID → card → info line → footer radio line — is 8px.
func TestWifiQrPageInkAlignment(t *testing.T) {
	// A short SSID is drawn in place; a long one runs as a ticker, which must
	// rest on the same column. Pin the ticker clock to its start pause.
	for _, ssid := range []string{"pcat-5G", "photonicat2-46a4d89f"} {
		t.Run(ssid, func(t *testing.T) {
			sEpoch := scrollEpoch
			scrollEpoch = time.Now()
			t.Cleanup(func() { scrollEpoch = sEpoch })
			checkWifiQrInkAlignment(t, ssid)
		})
	}
}

func checkWifiQrInkAlignment(t *testing.T, ssidName string) {
	withWifiQrState(t)
	fonts = buildFontTable(".")
	assetsPrefix = "."
	withWifiLive(t, []wifiRadioLive{{SSID: ssidName, Channel: 36, FreqMHz: 5180, WidthMHz: 80}})
	cfg.ShowWifiQr = nil
	cfgNumPages = 4
	gpsPageConfigured = false
	globalData.Store("WiFiClientsCount", 0)
	updateWifiQrPages([]WiFiInterface{{Exist: true, Enabled: true, Band: "5g", Htmode: "HE80",
		DeviceType: "PCIE", SSID: ssidName, Password: "pw", Encryption: "psk2"}})

	H := middleFrameHeight + PCAT2_FOOTER_HEIGHT
	img := image.NewRGBA(image.Rect(0, 0, middleFrameWidth, H))
	mid := image.NewRGBA(image.Rect(0, 0, middleFrameWidth, middleFrameHeight))
	clearFrame(mid, middleFrameWidth, middleFrameHeight)
	renderMiddle(mid, &cfg, false, 4)
	copyImageToImageAt(img, mid, 0, 0)
	cacheFooterStr = ""
	foot := image.NewRGBA(image.Rect(0, 0, PCAT2_LCD_WIDTH, PCAT2_FOOTER_HEIGHT))
	if !renderFooter(foot, 4, cfgNumPages, false) {
		t.Fatal("footer not drawn")
	}
	cacheFooterStr = ""
	copyImageToImageAt(img, foot, 0, middleFrameHeight)

	ink := func(x, y int) bool { c := img.RGBAAt(x, y); return int(c.R)+int(c.G)+int(c.B) > 150 }
	rowInk := func(y, x0, x1 int) bool {
		for x := x0; x < x1; x++ {
			if ink(x, y) {
				return true
			}
		}
		return false
	}
	type run struct{ top, bot int }
	var runs []run
	for y := 0; y < H; y++ {
		if !rowInk(y, 0, middleFrameWidth) {
			continue
		}
		if n := len(runs); n > 0 && runs[n-1].bot == y-1 {
			runs[n-1].bot = y
		} else {
			runs = append(runs, run{y, y})
		}
	}
	if len(runs) != 5 {
		t.Fatalf("want 5 bands (title, SSID, card, info, footer), got %v", runs)
	}
	title, ssid, card, info, footer := runs[0], runs[1], runs[2], runs[3], runs[4]
	// SSID baseline: skip the leading "p", whose descender hangs below it.
	ssidBase := ssid.top
	for y := ssid.top; y <= ssid.bot; y++ {
		if rowInk(y, 24, middleFrameWidth) {
			ssidBase = y
		}
	}
	gaps := []struct {
		name string
		got  int
	}{
		{"title→SSID", ssid.top - title.bot - 1},
		{"SSID→card", card.top - ssidBase - 1},
		{"card→info", info.top - card.bot - 1},
		{"info→footer", footer.top - info.bot - 1},
	}
	for _, g := range gaps {
		if g.got != 8 {
			t.Errorf("%s gap = %dpx, want 8 (bands %v)", g.name, g.got, runs)
		}
	}
	leftInk := func(r run) int {
		for x := 0; x < middleFrameWidth; x++ {
			for y := r.top; y <= r.bot; y++ {
				if ink(x, y) {
					return x
				}
			}
		}
		return -1
	}
	for name, r := range map[string]run{"title": title, "SSID": ssid} {
		if x := leftInk(r); x != wifiQrSidePad {
			t.Errorf("%s ink starts at x=%d, want %d", name, x, wifiQrSidePad)
		}
	}
	// The two bottom lines are centred: equal visible margins (±1px).
	rightInk := func(r run) int {
		for x := middleFrameWidth - 1; x >= 0; x-- {
			for y := r.top; y <= r.bot; y++ {
				if ink(x, y) {
					return x
				}
			}
		}
		return -1
	}
	for name, r := range map[string]run{"info": info, "footer": footer} {
		l, rr := leftInk(r), middleFrameWidth-1-rightInk(r)
		if d := l - rr; d < -1 || d > 1 {
			t.Errorf("%s not centred: left margin %d, right margin %d", name, l, rr)
		}
	}
}

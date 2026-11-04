package main

// Wi-Fi QR pages: one page per access point, each showing a QR code that a
// phone camera turns into "join this network". The pages are drawn in code
// (not from display_template), so a user's saved layout can never hide them
// and the GPS page's "last template page" detection stays untouched.
//
// Page order in the rotation:
//
//	[template pages (GPS included when shown)] [Wi-Fi QR pages] [SMS pages]
//
// The Wi-Fi pages are counted inside cfgNumPages, the last wifiQrPageCount()
// of them, so the footer dots, page wrap and SMS offsets need no extra cases.
// Use templatePageCount() wherever a page index is looked up in the template.
//
// The key itself is not printed (the QR carries it). The title is band plus
// Wi-Fi generation; the line under the code shows radio, security and the
// Wi-Fi client count.
//
// Data comes from pcat-manager-web's dashboard wifi_interfaces (SSID, key,
// encryption, band), which processData already fetches. Controlled by the
// "show_wifi_qr" config key, on by default (nil = on, so a user_config saved
// by an older firmware, which never had the key, gets the feature too).

import (
	"hash/fnv"
	"image"
	"image/color"
	"image/draw"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/skip2/go-qrcode"
	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// wifiQrNet is one network as it is shown on its page.
type wifiQrNet struct {
	Bands      []string // e.g. ["2g"] or ["2g","5g"] when both radios share SSID+key
	Radios     []string // "Onboard" / "PCIE", one per distinct radio class
	Gens       []string // "6", "6E", "7"...: Wi-Fi generation per distinct radio mode
	SSID       string
	Password   string
	Encryption string // normalized by pcat-manager-web: none/psk/psk2/sae/...
	Hidden     bool
}

type wifiQrRendered struct {
	net     wifiQrNet
	payload string
	hash    uint64
	qr      *image.RGBA // white square incl. quiet zone, wifiQrSize×wifiQrSize
}

const (
	wifiQrSize  = 172 // white QR block is full panel width
	wifiQrQuiet = 6   // white margin around the modules (quiet zone)
	// The QR sits on a white card with rounded corners whose bottom strip
	// carries the "Scan to connect" caption, so the instruction reads as part
	// of the code rather than as one more status line.
	wifiQrCaption     = "Scan to connect"
	wifiQrCaptionGap  = 6 // modules → caption capitals
	wifiQrCaptionPad  = 8 // caption → card bottom
	wifiQrCardRadius  = 8 // corner radius
	wifiQrCaptionCapH = 9 // cap height of "tiny" (Orbitron 12)
	wifiQrCardH       = wifiQrSize - wifiQrQuiet + wifiQrCaptionGap + wifiQrCaptionCapH + wifiQrCaptionPad
	// Offsets give an equal 8px visual gap title → SSID → QR → info line,
	// measured from the rendered ink (the SSID's baseline, not descenders).
	//
	// The group is anchored to the footer's radio line rather than centred,
	// so the info line → footer line gap is the same 8px as the others:
	// footer ink starts 4px below the middle area (micro font at y=2), the
	// info line is 9px tall, the card wifiQrCardH, title+SSID 48px.
	wifiQrFooterInkTop = PCAT2_LCD_HEIGHT - PCAT2_TOP_BAR_HEIGHT - PCAT2_FOOTER_HEIGHT + 4
	wifiQrTitleY       = wifiQrFooterInkTop - 8 - wifiQrCaptionCapH - 8 - wifiQrCardH - 48
	wifiQrSsidY        = wifiQrTitleY + 22
	wifiQrBlockY       = wifiQrTitleY + 48
	wifiQrSidePad      = 6
)

var (
	wifiQrMu      sync.Mutex
	wifiQrPages   []*wifiQrRendered
	wifiQrLastIfs []WiFiInterface
	wifiQrHaveIfs bool
	wifiQrPageNum int // == len(wifiQrPages); read lock-free by the render loop
)

// wifiQrEnabled reports the show_wifi_qr setting; absent means on.
func wifiQrEnabled(c *Config) bool {
	return c == nil || c.ShowWifiQr == nil || *c.ShowWifiQr
}

// wifiQrPageCount is how many of the trailing cfgNumPages are Wi-Fi pages.
func wifiQrPageCount() int { return wifiQrPageNum }

// templatePageCount is the number of visible display_template pages.
func templatePageCount() int {
	n := cfgNumPages - wifiQrPageNum
	if n < 0 {
		return 0
	}
	return n
}

// isWifiQrPage reports whether a cfg page index is one of the Wi-Fi pages.
func isWifiQrPage(pageIdx int) bool {
	return wifiQrPageNum > 0 && pageIdx >= templatePageCount() && pageIdx < cfgNumPages
}

// updateWifiQrPages is called with every fresh wifi_interfaces list.
func updateWifiQrPages(ifs []WiFiInterface) {
	refreshWifiLive()
	wifiQrMu.Lock()
	wifiQrLastIfs = append(wifiQrLastIfs[:0], ifs...)
	wifiQrHaveIfs = true
	wifiQrMu.Unlock()

	// configMutex serializes the page-count arithmetic against mergeConfigs,
	// which rebuilds cfgNumPages from the template under the same lock.
	configMutex.Lock()
	applyWifiQrPages(wifiQrEnabled(&cfg))
	configMutex.Unlock()
}

// refreshWifiQrPagesLocked re-applies the last known interfaces after a config
// merge (the toggle may have changed). The caller holds configMutex and has
// just reset cfgNumPages to the template count, so the Wi-Fi pages are added
// back on top of it rather than adjusted from the previous count.
func refreshWifiQrPagesLocked() {
	wifiQrPageNum = 0
	applyWifiQrPages(wifiQrEnabled(&cfg))
}

// applyWifiQrPages rebuilds the page set; the caller holds configMutex.
func applyWifiQrPages(enabled bool) {
	wifiQrMu.Lock()
	var nets []wifiQrNet
	if enabled && wifiQrHaveIfs {
		nets = wifiQrNetworks(wifiQrLastIfs)
	}
	// Reuse rendered QR blocks whose payload did not change.
	old := make(map[string]*wifiQrRendered, len(wifiQrPages))
	for _, p := range wifiQrPages {
		old[wifiQrCacheKey(p.payload, p.net)] = p
	}
	pages := make([]*wifiQrRendered, 0, len(nets))
	changed := len(nets) != len(wifiQrPages)
	for i, n := range nets {
		payload := wifiQrPayload(n)
		key := wifiQrCacheKey(payload, n)
		if p, ok := old[key]; ok {
			pages = append(pages, p)
			if i >= len(wifiQrPages) || wifiQrPages[i] != p {
				changed = true
			}
			continue
		}
		img, err := renderWifiQrBlock(payload)
		if err != nil {
			log.Printf("wifi qr: cannot encode network %d: %v", i, err)
			continue
		}
		h := fnv.New64a()
		h.Write([]byte(key))
		pages = append(pages, &wifiQrRendered{net: n, payload: payload, hash: h.Sum64(), qr: img})
		changed = true
	}
	wifiQrPages = pages
	newNum := len(pages)
	wifiQrMu.Unlock()

	if !changed && newNum == wifiQrPageNum {
		return
	}
	templ := templatePageCount()
	wifiQrPageNum = newNum
	cfgNumPages = templ + newNum
	if cfg.ShowSms {
		totalNumPages = cfgNumPages + lenSmsPagesImages
	} else {
		totalNumPages = cfgNumPages
	}
	// Parked on a Wi-Fi page (or past it) while the set changed: animate to
	// the next page instead of leaving a stale or vanished page on screen.
	if currPageIdx >= templ {
		forceMiddleRedraw = true
		if currPageIdx >= cfgNumPages {
			httpChangePageTriggered = true
		}
	}
	log.Println("wifi qr pages:", newNum, "cfgNumPages:", cfgNumPages, "totalNumPages:", totalNumPages)
}

// wifiQrCacheKey identifies a page: everything that changes its pixels.
func wifiQrCacheKey(payload string, n wifiQrNet) string {
	return payload + "\x00" + strings.Join(n.Bands, ",") + "\x00" + strings.Join(n.Radios, ",") +
		"\x00" + strings.Join(n.Gens, ",")
}

// wifiRadioLabel names the radio the way page 2 does ("SSID - Onboard",
// "SSID - PCIE"): pcat-manager-web classifies each radio by its device path.
func wifiRadioLabel(deviceType string) string {
	switch strings.ToUpper(strings.TrimSpace(deviceType)) {
	case "PCIE":
		return "PCIE"
	case "ONBOARD", "BUILTIN":
		return "Onboard"
	}
	return ""
}

// wifiGeneration maps the radio's OpenWrt htmode to its Wi-Fi Alliance
// generation: HT = Wi-Fi 4 (11n), VHT = 5 (11ac), HE = 6 (11ax, "6E" on the
// 6 GHz band), EHT = 7 (11be). Legacy/unknown modes yield "".
func wifiGeneration(htmode, band string) string {
	m := strings.ToUpper(strings.TrimSpace(htmode))
	switch {
	case strings.HasPrefix(m, "EHT"):
		return "7"
	case strings.HasPrefix(m, "HE"):
		if band == "6g" {
			return "6E"
		}
		return "6"
	case strings.HasPrefix(m, "VHT"):
		return "5"
	case strings.HasPrefix(m, "HT"):
		return "4"
	}
	return ""
}

func appendUnique(list []string, v string) []string {
	if v == "" {
		return list
	}
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// wifiQrNetworks picks the access points worth a QR page: present, enabled,
// actually broadcasting (not parked relaying an upstream hotspot) and named.
// Radios that share SSID + key + encryption become one page ("2.4/5GHz").
// Sorted 2.4 → 5 → 6 GHz so the order is stable across fetches.
func wifiQrNetworks(ifs []WiFiInterface) []wifiQrNet {
	var nets []wifiQrNet
	for _, it := range ifs {
		if !it.Exist || !it.Enabled || it.WiFiWan || strings.TrimSpace(it.SSID) == "" {
			continue
		}
		enc := strings.ToLower(strings.TrimSpace(it.Encryption))
		if !wifiQrIsOpen(wifiQrNet{Encryption: enc}) && it.Password == "" {
			continue // secured but we do not know the key: a QR would not work
		}
		band := strings.ToLower(strings.TrimSpace(it.Band))
		hidden := it.Hidden == "1" || strings.EqualFold(it.Hidden, "true")
		merged := false
		for i := range nets {
			n := &nets[i]
			if n.SSID == it.SSID && n.Password == it.Password && n.Encryption == enc && n.Hidden == hidden {
				n.Bands = append(n.Bands, band)
				n.Radios = appendUnique(n.Radios, wifiRadioLabel(it.DeviceType))
				n.Gens = appendUnique(n.Gens, wifiGeneration(it.Htmode, band))
				merged = true
				break
			}
		}
		if !merged {
			nets = append(nets, wifiQrNet{Bands: []string{band}, SSID: it.SSID,
				Radios:   appendUnique(nil, wifiRadioLabel(it.DeviceType)),
				Gens:     appendUnique(nil, wifiGeneration(it.Htmode, band)),
				Password: it.Password, Encryption: enc, Hidden: hidden})
		}
	}
	for i := range nets {
		sort.SliceStable(nets[i].Bands, func(a, b int) bool {
			return wifiBandOrder(nets[i].Bands[a]) < wifiBandOrder(nets[i].Bands[b])
		})
		sort.Strings(nets[i].Radios) // "Onboard" before "PCIE"
		sort.Strings(nets[i].Gens)
	}
	sort.SliceStable(nets, func(a, b int) bool {
		return wifiBandOrder(nets[a].Bands[0]) < wifiBandOrder(nets[b].Bands[0])
	})
	return nets
}

func wifiBandOrder(b string) int {
	switch b {
	case "2g":
		return 0
	case "5g":
		return 1
	case "6g":
		return 2
	}
	return 3
}

// wifiBandLabel turns OpenWrt band names into what people read on a router.
func wifiBandLabel(b string) string {
	switch b {
	case "2g":
		return "2.4"
	case "5g":
		return "5"
	case "6g":
		return "6"
	case "60g":
		return "60"
	}
	return ""
}

// wifiQrTitle is the band plus the Wi-Fi generation: "5GHz WiFi 6",
// "2.4GHz WiFi 4", "6GHz WiFi 6E", "2.4/5GHz WiFi 6/7" for a merged page.
// Either part drops out when unknown ("WiFi" when both are).
func wifiQrTitle(n wifiQrNet) string {
	var parts []string
	for _, b := range n.Bands {
		if l := wifiBandLabel(b); l != "" {
			parts = append(parts, l)
		}
	}
	band := ""
	if len(parts) > 0 {
		band = strings.Join(parts, "/") + "GHz"
	}
	gen := ""
	if len(n.Gens) > 0 {
		gen = "WiFi " + strings.Join(n.Gens, "/")
	}
	switch {
	case band != "" && gen != "":
		return band + " " + gen
	case band != "":
		return band
	case gen != "":
		return gen
	}
	return "WiFi"
}

// wifiQrRadioTag is "Onboard", "PCIE" or "Onboard/PCIE" for a merged page.
func wifiQrRadioTag(n wifiQrNet) string {
	return strings.Join(n.Radios, "/")
}

// wifiQrPalette is the active /screen colour theme (配色主题) as the Wi-Fi
// page uses it: title in primary (like the big values on other pages), the
// SSID in data (info text) and the info line in accent (section labels).
type wifiQrPalette struct{ primary, accent, data color.RGBA }

var (
	wifiQrPalMu  sync.Mutex
	wifiQrPalVer = -1
	wifiQrPal    wifiQrPalette
)

// wifiQrThemePalette reads the theme back out of the layout, the same way the
// SMS pages do: the web UI applies a theme by recolouring every element by
// role (screen.html themeRole), so the first element of each role carries
// that role's colour. Cached per configVersion, which bumps on every merge.
// Unthemed, the default layout yields PCAT yellow / yellow / white.
func wifiQrThemePalette() wifiQrPalette {
	wifiQrPalMu.Lock()
	defer wifiQrPalMu.Unlock()
	if wifiQrPalVer == configVersion {
		return wifiQrPal
	}
	pal := wifiQrPalette{primary: PCAT_YELLOW, accent: PCAT_YELLOW, data: PCAT_WHITE}
	got := map[string]bool{}
	bigFonts := map[string]bool{"huge": true, "gigantic": true, "big": true, "reg": true}

	configMutex.RLock()
	for i := 0; i < templatePageCount() && len(got) < 3; i++ {
		for _, el := range cfg.DisplayTemplate.Elements["page"+strconv.Itoa(i)] {
			if len(el.Color) < 3 || el.Type == "icon" {
				continue
			}
			role := "data"
			switch {
			case el.Type == "fixed_text":
				role = "accent"
			case el.Type == "cpu_bars" || el.Type == "hbar" || el.Type == "disk_bars" || bigFonts[el.Font]:
				role = "primary"
			}
			if got[role] {
				continue
			}
			got[role] = true
			c := color.RGBA{uint8(el.Color[0]), uint8(el.Color[1]), uint8(el.Color[2]), 255}
			switch role {
			case "primary":
				pal.primary = c
			case "accent":
				pal.accent = c
			default:
				pal.data = c
			}
		}
	}
	configMutex.RUnlock()

	wifiQrPal, wifiQrPalVer = pal, configVersion
	return pal
}

// wifiQrEscape escapes the characters the WIFI: URI scheme reserves.
func wifiQrEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `;`, `\;`, `,`, `\,`, `:`, `\:`, `"`, `\"`)
	return r.Replace(s)
}

// wifiQrIsOpen: no key to enter. OWE is encrypted but passwordless, and
// phones join it like an open network.
func wifiQrIsOpen(n wifiQrNet) bool {
	return n.Encryption == "" || n.Encryption == "none" || n.Encryption == "owe"
}

// wifiQrPayload builds the de-facto standard Wi-Fi QR string understood by
// iOS and Android cameras: WIFI:T:WPA;S:<ssid>;P:<key>;H:true;;
//
// WPA2, WPA3 and the mixed modes are all "WPA": pcat-manager-web folds
// sae-mixed into "sae", so "SAE" would wrongly tell older phones the network is
// WPA3-only, and both platforms negotiate WPA3 from a "WPA" code anyway.
func wifiQrPayload(n wifiQrNet) string {
	var b strings.Builder
	b.WriteString("WIFI:")
	open := wifiQrIsOpen(n)
	switch {
	case open:
		b.WriteString("T:nopass;")
	case strings.HasPrefix(n.Encryption, "wep"):
		b.WriteString("T:WEP;")
	default:
		b.WriteString("T:WPA;")
	}
	b.WriteString("S:" + wifiQrEscape(n.SSID) + ";")
	if !open {
		b.WriteString("P:" + wifiQrEscape(n.Password) + ";")
	}
	if n.Hidden {
		b.WriteString("H:true;")
	}
	b.WriteString(";")
	return b.String()
}

// renderWifiQrBlock rasterizes the QR as a white wifiQrSize square, the
// modules filling everything inside a wifiQrQuiet margin. Integer module
// sizes would cap a 29-module code at 145px on this 172px panel, so modules
// are allowed to be 5 or 6px (nearest-neighbour, still pure black/white):
// scanners lock onto the finder patterns and tolerate the uneven pitch.
func renderWifiQrBlock(payload string) (*image.RGBA, error) {
	q, err := qrcode.New(payload, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	q.DisableBorder = true
	bm := q.Bitmap()
	n := len(bm)
	img := image.NewRGBA(image.Rect(0, 0, wifiQrSize, wifiQrSize))
	draw.Draw(img, img.Bounds(), image.NewUniform(PCAT_WHITE), image.Point{}, draw.Src)
	if n == 0 {
		return img, nil
	}
	inner := wifiQrSize - 2*wifiQrQuiet
	black := color.RGBA{0, 0, 0, 255}
	for py := 0; py < inner; py++ {
		my := py * n / inner
		for px := 0; px < inner; px++ {
			if bm[my][px*n/inner] {
				img.SetRGBA(wifiQrQuiet+px, wifiQrQuiet+py, black)
			}
		}
	}
	return img, nil
}

func wifiQrPageAt(idx int) *wifiQrRendered {
	wifiQrMu.Lock()
	defer wifiQrMu.Unlock()
	if idx < 0 || idx >= len(wifiQrPages) {
		return nil
	}
	return wifiQrPages[idx]
}

// wifiQrSecurityLabel names the encryption the way phones and routers do.
// pcat-manager-web folds sae-mixed into "sae", so WPA3 may also mean WPA2/3.
func wifiQrSecurityLabel(enc string) string {
	switch {
	case enc == "" || enc == "none":
		return "Open"
	case enc == "owe":
		return "OWE"
	case strings.HasPrefix(enc, "wep"):
		return "WEP"
	case strings.HasPrefix(enc, "sae"):
		return "WPA3"
	case strings.HasPrefix(enc, "psk2"):
		return "WPA2"
	case strings.HasPrefix(enc, "psk-mixed"):
		return "WPA/WPA2"
	case strings.HasPrefix(enc, "psk"):
		return "WPA"
	case strings.HasPrefix(enc, "wpa"):
		return "Enterprise"
	}
	return strings.ToUpper(enc)
}

// wifiQrInfoLine is "Onboard • WPA2 • 3" (radio, security, Wi-Fi clients);
// hasCount says a count ends the line, so a person icon follows it. The word
// "clients" does not fit beside the radio on a 172px panel; the icon says it.
// The count is pcat-manager-web's wifi_clients_count (all access points
// together; there is no per-radio figure), left out until the first dashboard
// fetch has delivered it. Orbitron has no U+00B7 middle dot, so the separator
// is a bullet.
func wifiQrInfoLine(n wifiQrNet) (line string, hasCount bool) {
	line = wifiQrSecurityLabel(n.Encryption)
	if tag := wifiQrRadioTag(n); tag != "" {
		line = tag + " • " + line
	}
	if v, ok := globalData.Load("WiFiClientsCount"); ok {
		if c, ok := v.(int); ok && c >= 0 {
			line += " • " + strconv.Itoa(c)
			hasCount = true
		}
	}
	return line, hasCount
}

// wifiQrFingerprint lets the render loop skip redraws of an unchanged page.
// It carries a hash, never the key itself, plus the live info line.
func wifiQrFingerprint(idx int) string {
	p := wifiQrPageAt(idx)
	h, info := uint64(0), ""
	if p != nil {
		h = p.hash
		info, _ = wifiQrInfoLine(p.net)
	}
	return "wifi:" + strconv.Itoa(idx) + ":" + strconv.Itoa(configVersion) + ":" +
		strconv.FormatUint(h, 16) + ":" + info
}

// pickLineFace returns the largest of the given fonts in which text plus
// extraW pixels fits the panel width, truncating with "…" in the smallest one
// if none does. The returned string is what to draw.
func pickLineFace(text string, extraW int, fontNames ...string) (font.Face, string) {
	avail := middleFrameWidth - 2*wifiQrSidePad - extraW
	var face font.Face
	for _, fn := range fontNames {
		f, _, err := getFontFaceForText(fn, text)
		if err != nil || f == nil {
			continue
		}
		face = f
		if font.MeasureString(f, text).Round() <= avail {
			return f, text
		}
	}
	if face == nil {
		return nil, ""
	}
	r := []rune(text)
	for len(r) > 1 && font.MeasureString(face, string(r)+"…").Round() > avail {
		r = r[:len(r)-1]
	}
	return face, string(r) + "…"
}

// drawTextAtInk draws text so its first glyph's ink begins exactly at column
// inkLeft, using a sub-pixel pen position. Glyphs carry different fractional
// left side-bearings ("5", "p", "O"...), so drawing every line at the same
// whole-pixel pen x leaves their visible left edges up to a pixel apart; this
// aligns the ink, the way a designer would. y has drawText's meaning (top of
// the ascent box). Returns the x where the text ends.
func drawTextAtInk(img *image.RGBA, text string, inkLeft, y int, f font.Face, clr color.Color) int {
	if img == nil || text == "" {
		return inkLeft
	}
	b, _ := font.BoundString(f, text)
	d := &font.Drawer{Dst: img, Src: image.NewUniform(clr), Face: f}
	d.Dot = fixed.Point26_6{X: fixed.I(inkLeft) - b.Min.X, Y: fixed.I(y + f.Metrics().Ascent.Round())}
	drawStringComposed(d, text)
	return d.Dot.X.Round()
}

// drawTextCenteredInk centres text's ink, plus extraW pixels drawn right
// after it (an icon), on column cx with sub-pixel precision, so the visible
// margins left and right are equal. Returns the x where the text's ink ends.
func drawTextCenteredInk(img *image.RGBA, text string, cx, extraW, y int, f font.Face, clr color.Color) int {
	b, _ := font.BoundString(f, text)
	inkW := b.Max.X - b.Min.X
	left := fixed.I(cx) - (inkW+fixed.I(extraW))/2
	d := &font.Drawer{Dst: img, Src: image.NewUniform(clr), Face: f}
	d.Dot = fixed.Point26_6{X: left - b.Min.X, Y: fixed.I(y + f.Metrics().Ascent.Round())}
	drawStringComposed(d, text)
	return (left + inkW).Ceil()
}

// personIconW is drawPersonIcon's width for an icon h px tall.
func personIconW(h int) int { return h * 9 / 10 }

// capHeight is the face's capital-letter height in pixels (ink of "H").
func capHeight(f font.Face) int {
	b, _ := font.BoundString(f, "H")
	return (-b.Min.Y).Round()
}

// drawText's y is the top of the font's ascent box, and that box is taller
// than the letters by a different amount in every face. These place a line by
// its ink instead, so the 8px gaps hold whichever font a line falls back to.
func yForBaseline(f font.Face, baseline int) int { return baseline - f.Metrics().Ascent.Round() }
func yForCapTop(f font.Face, top int) int {
	return top - f.Metrics().Ascent.Round() + capHeight(f)
}

// wifiQrTitleBaseline is where the title sits: its "reg" baseline, which the
// SSID's position is measured from, kept even when the title falls back to a
// smaller font.
func wifiQrTitleBaseline() int {
	if f, _, err := getFontFace("reg"); err == nil && f != nil {
		return wifiQrTitleY + f.Metrics().Ascent.Round()
	}
	return wifiQrTitleY + 18
}

// drawPersonIcon draws a small head-and-shoulders glyph, h px tall, with its
// top-left at (x, y): the "clients" mark after the count. Anti-aliased by 4x4
// supersampling so it matches the font's smooth edges at 9-12px.
func drawPersonIcon(frame *image.RGBA, x, y, h int, clr color.RGBA) int {
	w := personIconW(h)
	fh, fw := float64(h), float64(w)
	headR := fh * 0.24
	headCx, headCy := fw/2, headR
	bodyTop := fh * 0.56
	bodyRx, bodyRy := fw/2, fh-bodyTop
	inside := func(px, py float64) bool {
		dx, dy := px-headCx, py-headCy
		if dx*dx+dy*dy <= headR*headR {
			return true
		}
		if py >= bodyTop && py <= fh {
			// Upper half of an ellipse centred on the bottom edge: shoulders.
			ex, ey := (px-fw/2)/bodyRx, (py-fh)/bodyRy
			return ex*ex+ey*ey <= 1
		}
		return false
	}
	const ss = 4
	for iy := 0; iy < h; iy++ {
		for ix := 0; ix < w; ix++ {
			n := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					if inside(float64(ix)+(float64(sx)+0.5)/ss, float64(iy)+(float64(sy)+0.5)/ss) {
						n++
					}
				}
			}
			if n == 0 {
				continue
			}
			px, py := x+ix, y+iy
			if !(image.Point{px, py}).In(frame.Bounds()) {
				continue
			}
			a := uint32(n) * 255 / (ss * ss)
			bg := frame.RGBAAt(px, py)
			mix := func(f, b uint8) uint8 { return uint8((uint32(f)*a + uint32(b)*(255-a)) / 255) }
			frame.SetRGBA(px, py, color.RGBA{mix(clr.R, bg.R), mix(clr.G, bg.G), mix(clr.B, bg.B), 255})
		}
	}
	return w
}

// wifiQrSsidFace is the SSID's font: the title's own "reg", so the name reads
// as large as the band. There is no reg_cjk, so a Chinese SSID uses the CJK
// face the rest of the panel uses.
func wifiQrSsidFace(ssid string) font.Face {
	if f, _, err := getFontFaceForText("reg", ssid); err == nil && f != nil {
		return f
	}
	if f, _, err := getFontFaceForText("unit", ssid); err == nil && f != nil {
		return f
	}
	return nil
}

func wifiQrTextWidth() int { return middleFrameWidth - 2*wifiQrSidePad }

// wifiQrSsidOverflows reports whether Wi-Fi page idx's SSID is wider than the
// panel, i.e. it runs as a ticker and the page must keep re-rendering.
func wifiQrSsidOverflows(idx int) bool {
	p := wifiQrPageAt(idx)
	if p == nil {
		return false
	}
	f := wifiQrSsidFace(p.net.SSID)
	return f != nil && font.MeasureString(f, p.net.SSID).Round() > wifiQrTextWidth()
}

// wifiQrCaptionColor: dark grey on the white card, quieter than the code.
var wifiQrCaptionColor = color.RGBA{72, 72, 72, 255}

// drawWifiQrCard draws the white card: the QR block, a white strip under it
// with the centred caption, and corners rounded (anti-aliased) into the
// page background. The caption is drawn here on the render goroutine rather
// than baked into the cached QR image, because font faces are not safe to
// share across goroutines.
func drawWifiQrCard(frame *image.RGBA, qr *image.RGBA) {
	x0 := (middleFrameWidth - wifiQrSize) / 2
	y0 := wifiQrBlockY
	card := image.Rect(x0, y0, x0+wifiQrSize, y0+wifiQrCardH).Intersect(frame.Bounds())
	draw.Draw(frame, card, image.NewUniform(PCAT_WHITE), image.Point{}, draw.Src)
	copyImageToImageAt(frame, qr, x0, y0)

	if f, _, err := getFontFace("tiny"); err == nil && f != nil {
		w := font.MeasureString(f, wifiQrCaption).Round()
		top := y0 + wifiQrSize - wifiQrQuiet + wifiQrCaptionGap
		drawText(frame, wifiQrCaption, x0+(wifiQrSize-w)/2, yForCapTop(f, top), f, wifiQrCaptionColor, false)
	}

	// Round the four corners: blend white → background by circle coverage.
	r := wifiQrCardRadius
	fr := float64(r)
	const ss = 4
	bg := screenBgColor
	for iy := 0; iy < r; iy++ {
		for ix := 0; ix < r; ix++ {
			n := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					dx := fr - (float64(ix) + (float64(sx)+0.5)/ss)
					dy := fr - (float64(iy) + (float64(sy)+0.5)/ss)
					if dx*dx+dy*dy <= fr*fr {
						n++
					}
				}
			}
			a := uint32(n) * 255 / (ss * ss)
			mix := func(b uint8) uint8 { return uint8((255*a + uint32(b)*(255-a)) / 255) }
			c := color.RGBA{mix(bg.R), mix(bg.G), mix(bg.B), 255}
			for _, pt := range []image.Point{
				{card.Min.X + ix, card.Min.Y + iy},
				{card.Max.X - 1 - ix, card.Min.Y + iy},
				{card.Min.X + ix, card.Max.Y - 1 - iy},
				{card.Max.X - 1 - ix, card.Max.Y - 1 - iy},
			} {
				if pt.In(frame.Bounds()) {
					frame.SetRGBA(pt.X, pt.Y, c)
				}
			}
		}
	}
}

// drawWifiQrPage renders Wi-Fi page idx (0-based among the Wi-Fi pages).
func drawWifiQrPage(frame *image.RGBA, idx int) {
	p := wifiQrPageAt(idx)
	if p == nil {
		return
	}
	pal := wifiQrThemePalette()
	if f, t := pickLineFace(wifiQrTitle(p.net), 0, "reg", "unit", "tiny"); f != nil {
		drawTextAtInk(frame, t, wifiQrSidePad, yForBaseline(f, wifiQrTitleBaseline()), f, pal.primary)
	}
	// Long SSIDs scroll as a ticker, like the SSID rows on page 2.
	if f := wifiQrSsidFace(p.net.SSID); f != nil {
		if font.MeasureString(f, p.net.SSID).Round() <= wifiQrTextWidth() {
			drawTextAtInk(frame, p.net.SSID, wifiQrSidePad, wifiQrSsidY, f, pal.data)
		} else {
			// Clip starts on the ink column; the sub-pixel shift puts the
			// first glyph's ink exactly on it while the ticker rests.
			b, _ := font.BoundString(f, p.net.SSID)
			drawScrollingTextShift(frame, p.net.SSID, wifiQrSidePad, wifiQrSsidY, wifiQrTextWidth(), f, pal.data, -b.Min.X)
		}
	}

	drawWifiQrCard(frame, p.qr)

	// Info line: its capitals start 8px under the card; centred by ink, the
	// person icon included.
	line, hasCount := wifiQrInfoLine(p.net)
	// 12px ("tiny") on every page, so two networks' pages match; micro only
	// when a merged page's "Onboard/PCIE" makes the line too long.
	iconW := 0
	if hasCount {
		if f, _, err := getFontFace("tiny"); err == nil && f != nil {
			iconW = 3 + capHeight(f) // gap + icon
		}
	}
	if f, t := pickLineFace(line, iconW, "tiny", "micro"); f != nil {
		top := wifiQrBlockY + wifiQrCardH + 8
		extra := 0
		if hasCount && t == line {
			extra = 3 + personIconW(capHeight(f))
		}
		endX := drawTextCenteredInk(frame, t, middleFrameWidth/2, extra, yForCapTop(f, top), f, pal.accent)
		if extra > 0 {
			drawPersonIcon(frame, endX+3, top, capHeight(f), pal.accent)
		}
	}
}

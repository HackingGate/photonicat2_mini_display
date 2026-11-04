package main

// Live radio state for the Wi-Fi QR pages' footer line
// ("5.745GHz • 80MHz • ch149"). The configured channel is often "auto", so the
// operating channel has to come from the driver: `iwinfo` with no arguments
// lists every interface's live ESSID, mode, channel/frequency and HT mode.
// Each QR page is matched to its access point by SSID + band.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type wifiRadioLive struct {
	SSID     string
	Channel  int
	FreqMHz  int
	WidthMHz int
}

const wifiLiveTTL = 30 * time.Second

var (
	wifiLiveMu sync.Mutex
	wifiLive   []wifiRadioLive
	wifiLiveAt time.Time

	reIwHeader  = regexp.MustCompile(`^(\S+)\s+ESSID:\s*(?:"(.*)"|unknown)`)
	reIwMode    = regexp.MustCompile(`Mode:\s*(\S+)`)
	reIwChannel = regexp.MustCompile(`Channel:\s*(\d+)\s*\(([\d.]+)\s*GHz\)`)
	reIwHtmode  = regexp.MustCompile(`HT Mode:\s*(\S+)`)
	reHtWidth   = regexp.MustCompile(`(\d+)$`)
)

// refreshWifiLive re-reads iwinfo at most once per wifiLiveTTL. Called from
// the dashboard collector, so it never runs on the render goroutine.
func refreshWifiLive() {
	wifiLiveMu.Lock()
	fresh := !wifiLiveAt.IsZero() && time.Since(wifiLiveAt) < wifiLiveTTL
	wifiLiveMu.Unlock()
	if fresh {
		return
	}
	var live []wifiRadioLive
	if out, err := secureExecCommand("iwinfo"); err == nil {
		live = parseIwinfoAPs(string(out))
	}
	wifiLiveMu.Lock()
	wifiLive, wifiLiveAt = live, time.Now()
	wifiLiveMu.Unlock()
}

// parseIwinfoAPs extracts the access-point (Mode: Master) interfaces from
// `iwinfo` output. A block starts at an unindented "<ifname>  ESSID: ..." line.
func parseIwinfoAPs(out string) []wifiRadioLive {
	var res []wifiRadioLive
	var cur *wifiRadioLive
	var mode string
	flush := func() {
		if cur != nil && mode == "Master" && cur.Channel > 0 {
			res = append(res, *cur)
		}
		cur, mode = nil, ""
	}
	for _, line := range strings.Split(out, "\n") {
		if m := reIwHeader.FindStringSubmatch(line); m != nil {
			flush()
			cur = &wifiRadioLive{SSID: m[2]}
			continue
		}
		if cur == nil {
			continue
		}
		if m := reIwMode.FindStringSubmatch(line); m != nil {
			mode = m[1]
		}
		if m := reIwChannel.FindStringSubmatch(line); m != nil {
			cur.Channel, _ = strconv.Atoi(m[1])
			if ghz, err := strconv.ParseFloat(m[2], 64); err == nil {
				cur.FreqMHz = int(ghz*1000 + 0.5)
			}
		}
		if m := reIwHtmode.FindStringSubmatch(line); m != nil {
			cur.WidthMHz = htmodeWidthMHz(m[1])
		}
	}
	flush()
	return res
}

// htmodeWidthMHz: HT20/VHT80/HE160/EHT320 → 20/80/160/320; NOHT/unknown → 20.
func htmodeWidthMHz(htmode string) int {
	if m := reHtWidth.FindStringSubmatch(strings.TrimSpace(htmode)); m != nil {
		if w, err := strconv.Atoi(m[1]); err == nil && w > 0 {
			return w
		}
	}
	return 20
}

// bandOfFreq maps an operating frequency to the OpenWrt band name.
func bandOfFreq(mhz int) string {
	switch {
	case mhz >= 2400 && mhz < 2500:
		return "2g"
	case mhz >= 5925 && mhz <= 7125:
		return "6g"
	case mhz >= 5150 && mhz < 5925:
		return "5g"
	}
	return ""
}

// wifiQrRadioLine is the footer text for a network's page: the full
// "5.745GHz • 80MHz • ch149" for one radio, "ch6 20MHz • ch149 80MHz" when the
// page merges radios. "" when nothing live matched (the footer then keeps its
// page dots).
func wifiQrRadioLine(n wifiQrNet) string {
	wifiLiveMu.Lock()
	live := wifiLive
	wifiLiveMu.Unlock()
	var hits []wifiRadioLive
	for _, b := range n.Bands {
		for _, l := range live {
			if l.SSID == n.SSID && bandOfFreq(l.FreqMHz) == b {
				hits = append(hits, l)
				break
			}
		}
	}
	switch len(hits) {
	case 0:
		return ""
	case 1:
		h := hits[0]
		return fmt.Sprintf("%.3fGHz • %dMHz • ch%d", float64(h.FreqMHz)/1000, h.WidthMHz, h.Channel)
	}
	parts := make([]string, len(hits))
	for i, h := range hits {
		parts[i] = fmt.Sprintf("ch%d %dMHz", h.Channel, h.WidthMHz)
	}
	return strings.Join(parts, " • ")
}

// wifiQrFooterLine returns the radio line for cfg page pageIdx when it is a
// Wi-Fi page, else "".
func wifiQrFooterLine(pageIdx int) string {
	if !isWifiQrPage(pageIdx) {
		return ""
	}
	p := wifiQrPageAt(pageIdx - templatePageCount())
	if p == nil {
		return ""
	}
	return wifiQrRadioLine(p.net)
}

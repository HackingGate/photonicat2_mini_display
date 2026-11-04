package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-ping/ping"
)

// uaTransport wraps an http.RoundTripper to inject our User-Agent header.
type uaTransport struct {
	base http.RoundTripper
}

func (t *uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", getUserAgent())
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// Secure HTTP client with timeouts and proper TLS configuration
var secureHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &uaTransport{base: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: false, // Always verify certificates
			MinVersion:         tls.VersionTLS12,
		},
		TLSHandshakeTimeout: 5 * time.Second,
	}},
}

// Local HTTP client for internal APIs (localhost)
var localHTTPClient = &http.Client{
	Timeout:   15 * time.Second,
	Transport: &uaTransport{},
}

func getUserAgent() string {
	return "photonicat2_display/r7700"
}

// Testability seams: these vars default to the exact literals the collectors
// have always used, so production behaviour is unchanged. Tests point them at
// t.TempDir() fixtures (or, for pdCovExecOutput, a canned dispatcher) so the
// /proc-/sys-/exec-backed readers can run on non-Linux development hosts.
var (
	pdCovProcNetRoutePath   = "/proc/net/route"
	pdCovOpenwrtReleasePath = "/etc/openwrt_release"
	pdCovProcUptimePath     = "/proc/uptime"
	pdCovChargerVoltagePath = "/sys/class/power_supply/charger/voltage_now"
	pdCovSysClassNetDir     = "/sys/class/net"
	pdCovProcStatPath       = "/proc/stat"
	pdCovBatteryDir         = "/sys/class/power_supply/battery"
	pdCovThermalZonePath    = "/sys/class/thermal/thermal_zone0/temp"
	pdCovProcMeminfoPath    = "/proc/meminfo"
	pdCovHwmonFanGlob       = "/sys/class/hwmon/hwmon*/fan1_input"
	pdCovMmcTypeGlob        = "/sys/block/mmcblk*/device/type"
	pdCovProcMountsPath     = "/proc/mounts"
	pdCovProcNetDevPath     = "/proc/net/dev"
	pdCovExecOutput         = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output()
	}
)

// sanitizeCommandArg validates and sanitizes command arguments
func sanitizeCommandArg(arg string) string {
	// Remove any shell metacharacters and limit to alphanumeric, dash, underscore, dot, slash
	validPattern := regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)
	if !validPattern.MatchString(arg) {
		return ""
	}
	return arg
}

// secureExecCommand executes a command with sanitized arguments
func secureExecCommand(command string, args ...string) ([]byte, error) {
	// Validate command name
	if sanitizeCommandArg(command) == "" {
		return nil, fmt.Errorf("invalid command: %s", command)
	}

	// Sanitize all arguments
	sanitizedArgs := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "" {
			continue
		}
		// Allow some special arguments for system commands
		if arg == "default" || arg == "--json" || arg == "-r" || arg == "-t" || arg == "-f" ||
			arg == "-c" || arg == "-v" || strings.HasPrefix(arg, "wireless.@wifi-iface") ||
			strings.HasPrefix(arg, "/dev/") || strings.HasPrefix(arg, "-") {
			sanitizedArgs = append(sanitizedArgs, arg)
		} else if sanitized := sanitizeCommandArg(arg); sanitized != "" {
			sanitizedArgs = append(sanitizedArgs, sanitized)
		} else {
			return nil, fmt.Errorf("invalid argument: %s", arg)
		}
	}

	return pdCovExecOutput(command, sanitizedArgs...)
}

// WiFiInterface mirrors each element of "wifi_interfaces" in the JSON.
type WiFiInterface struct {
	Band       string `json:"band"`
	Device     string `json:"device"`
	DeviceType string `json:"device_type"`
	Enabled    bool   `json:"enabled"`
	Encryption string `json:"encryption"`
	Exist      bool   `json:"exist"`
	Hidden     string `json:"hidden"`
	Htmode     string `json:"htmode"`
	Password   string `json:"password,omitempty"`
	SSID       string `json:"ssid"`
	Frequency  string `json:"frequency,omitempty"`
	// WiFiWan is set when this radio is relaying an upstream hotspot as the WAN
	// priority "wifi" slot (its own AP is parked). WiFiWanSSID is the upstream
	// hotspot's SSID, reported only while WiFiWanAssociated is true — a relaying
	// radio that has not (re)joined yet is on standby, not connected to a name.
	WiFiWan           bool   `json:"wifi_wan,omitempty"`
	WiFiWanSSID       string `json:"wifi_wan_ssid,omitempty"`
	WiFiWanAssociated bool   `json:"wifi_wan_associated,omitempty"`
}

// DashboardInfo matches the top‐level keys in your sample JSON.
type DashboardInfo struct {
	BatteryCurrent      float64         `json:"battery_current"`
	BatteryWattage      float64         `json:"battery_wattage"`
	// BatteryRemainingTime is pcat-manager-web's own "H:MM" estimate of the time
	// left to charge (to 90%) or discharge (to 0%). We mirror it verbatim so the
	// LCD never disagrees with the web home page; empty when the web side can't
	// compute it, in which case collectBatteryData() falls back to its own math.
	BatteryRemainingTime string         `json:"battery_remaining_time"`
	BoardTemperature    int             `json:"board_temperature"`
	Carrier             string          `json:"carrier"`
	// ModemMode is pcat-manager-web's "modem_mode": the PMU-reported radio
	// generation ("5G"/"4G"/"3G"), falling back to the modem's own cell_tech.
	// Carrier uses the reverse priority (cell_tech first), which makes it
	// unreliable on modems whose AT+QNWINFO lies: the RM500Q-GL answers
	// "CDMA","46001","CDMA BC0" while actually camped on NR/LTE, so cell_tech
	// — and therefore Carrier — becomes "Other". The web dashboard shows the
	// right generation because it reads modem_mode, so the LCD prefers it too.
	ModemMode           string          `json:"modem_mode"`
	ChargePercent       int             `json:"charge_percent"`
	ChargeVoltage       int             `json:"charge_voltage"`
	Connection          string          `json:"connection"`
	// ActiveEgress is the interface that actually carries the default route:
	// "wan" (eth0), "lan" (eth1), "wifi" (Smart WAN STA uplink) or "mobile".
	// It is more precise than Connection for choosing the top-bar icon.
	ActiveEgress        string          `json:"active_egress"`
	// NetworkMode is the user's "Internet via" selection: "auto", "eth_only",
	// "cell_only", "smart_wan" or "all_off".
	NetworkMode         string          `json:"network_mode"`
	// NetworkModeLabel is the human-readable form of NetworkMode as computed by
	// pcat-manager-web: "Eth/5G"/"Eth/4G", "Eth Only", "Cell Only",
	// "Smart WAN" or "All Off".
	NetworkModeLabel    string          `json:"network_mode_label"`
	// WifiSignalPercent is the Smart WAN upstream RSSI as 0-100% (or null).
	WifiSignalPercent   *int            `json:"wifi_signal_percent"`
	DHCPClientsCount    int             `json:"dhcp_clients_count"`
	UpSpeedBps          float64         `json:"up_speed"`
	DownSpeedBps        float64         `json:"down_speed"`
	FirmwareVersion     string          `json:"firmware_version"`
	Hostname            string          `json:"hostname"`
	ISPName             string          `json:"isp_name"`
	Kernel              string          `json:"kernel"`
	Model               string          `json:"model"`
	ModemModel          string          `json:"modem_model"`
	ModemSignalStrength int             `json:"modem_signal_strength"`
	OnCharging          bool            `json:"on_charging"`
	OpenWRTVersion      string          `json:"openwrt_version"`
	SdState             int             `json:"sd_state"`
	ServerLocation      string          `json:"server_location"`
	SimState            string          `json:"sim_state"`
	SimNumber           string          `json:"sim_number"`
	Uptime              string          `json:"uptime"`
	Voltage             int             `json:"voltage"`
	// Public IP (IPv4) as resolved by pcat-manager-web. Field name on the wire
	// is wan_ip (historical); empty when offline or still probing.
	WanIP               string          `json:"wan_ip"`
	WanIPv6             string          `json:"wan_ipv6"`
	// LocalWanIP is the address on the active uplink interface.
	LocalWanIP          string          `json:"local_wan_ip"`
	WanCarrier          bool            `json:"wan_carrier"`
	// PublicIP is a legacy/alias key some older builds exposed; prefer WanIP.
	PublicIP            string          `json:"public_ip"`
	WiFiClientsCount    int             `json:"wifi_clients_count"`
	WiFiInterfaces      []WiFiInterface `json:"wifi_interfaces"`
}

type ModemBasicInfo struct {
	CellCarrierInfo     string         `json:"cell_carrier_info"`
	FirmwareVersion     string         `json:"firmware_version"`
	IMEINum             string         `json:"imei_num"`
	Messages            []interface{}  `json:"messages"`
	ModemCellID         string         `json:"modem_cell_id"`
	ModemCellInfo       string         `json:"modem_cell_info"`
	ModemCellSignals    string         `json:"modem_cell_signals"`
	ModemCPIN           string         `json:"modem_cpin"`
	ModemIspDetails     string         `json:"modem_isp_details"`
	ModemModel          string         `json:"modem_model"`
	ModemNetworkInfo    string         `json:"modem_network_info"`
	ModemRoamPref       string         `json:"modem_roam_pref"`
	ModemServingInfo    string         `json:"modem_serving_info"`
	ModemServingQuality string         `json:"modem_serving_quality"`
	ModemTemperature    map[string]int `json:"modem_temperature"`
	ModemUSBSpeed       string         `json:"modem_usb_speed"`
	ModemUSBNetMode     string         `json:"modem_usbnet_mode"`
	ModemValid          bool           `json:"modem_valid"`
	PolicyLTEBands      string         `json:"policy_lte_bands"`
	PolicyNR5GBands     string         `json:"policy_nr5g_bands"`
	SelectedLTEBands    string         `json:"selected_lte_bands"`
	SelectedNR5GBands   string         `json:"selected_nr5g_bands"`
	SimNumber           string         `json:"sim_number"`
	SimState            string         `json:"sim_state"`
	SMSCheckInterval    int            `json:"sms_check_interval"`
	SMSForward          bool           `json:"sms_forward"`
	SMSForwardTo        string         `json:"sms_forward_to"`
}

// NetworkStats matches the keys returned by /api/v1/data_stats.json?network_type=mobile
type NetworkStats struct {
	TodayUsed     float64 `json:"today_used"`
	WeekUsed      float64 `json:"week_used"`
	MonthUsed     float64 `json:"month_used"`
	LastMonthUsed float64 `json:"last_month_used"`
}

// NetworkSpeed represents upload/download in bytes per second
type NetworkSpeed struct {
	UploadMbps   float64
	DownloadMbps float64
}

func collectBatteryData() {
	var err error
	if battSOC, err = getBatterySoc(); err != nil {
		fmt.Printf("Could not get battery soc: %v\n", err)
		globalData.Store("BatterySoc", -1)
	} else {
		globalData.Store("BatterySoc", battSOC)
	}

	if battChargingStatus, err = getBatteryCharging(); err != nil {
		fmt.Printf("Could not get battery charging: %v\n", err)
		globalData.Store("BatteryCharging", false)
	} else {
		globalData.Store("BatteryCharging", battChargingStatus)
	}

	//if charging status change, we trigger lastActivity
	if battChargingStatus != lastChargingStatus {
		log.Println("Battery charging status changed to: ", battChargingStatus)
		if idleState == STATE_ACTIVE {
			lastActivity = time.Now().Add(-fadeInDur) //reset lastActivity for screen to stay on, - fadeInDur to send state to active
		} else {
			lastActivity = time.Now() //bring back screen with some fade in
		}
		lastChargingStatus = battChargingStatus

		log.Printf("idleTimeout: %v", idleTimeout)
	}
	if battChargingStatus {
		idleTimeout = time.Duration(cfg.ScreenDimmerTimeOnDCSeconds) * time.Second
	} else {
		idleTimeout = time.Duration(cfg.ScreenDimmerTimeOnBatterySeconds) * time.Second
	}

	// Battery/DC volts and wattage are drawn on page 0 too, so sample them at
	// this collector's page-0 cadence (2 Hz visible, 1/min otherwise) instead
	// of collectLinuxData's cpu-bars-page cadence, which left the page-0 row
	// refreshing once a minute.
	collectPowerData()

	// Remaining time estimate for page0's 4th slot (clock icon + "H:MM").
	// pcat-manager-web is the source of truth: when getInfoFromPcatWeb() has
	// stored a value from dashboard.json, keep it so the LCD never disagrees
	// with the web home page. Only compute our own estimate as a fallback.
	if fromWeb, _ := globalData.Load("RemainingTimeFromWeb"); fromWeb == true {
		applyRemainingTimeUnit() // web already stored the value; just keep unit clear
	} else if hours, ok := computeRemainingTimeHours(battChargingStatus); ok {
		globalData.Store("RemainingTime", formatRemainingTime(hours))
		applyRemainingTimeUnit()
	} else {
		// Nothing to estimate (idle battery, missing counters): leave the slot
		// blank — no "-" placeholder, and the clock icon is skipped too.
		globalData.Store("RemainingTime", "")
		globalData.Store("RemainingTime_Unit", "")
	}
}

// applyRemainingTimeUnit keeps the remaining-time slot to just the clock icon +
// "H:MM": the target-percent suffix (">90%"/">0%") made the row too wide for the
// 172px screen, so no unit is drawn. The clock icon already conveys "time left".
func applyRemainingTimeUnit() {
	globalData.Store("RemainingTime_Unit", "")
}

// cellGeneration reduces the two radio-generation fields the web dashboard
// exposes to the single token the top bar draws: "5", "4", "3" or "" when the
// modem isn't on a known cellular generation.
//
// modem_mode wins over carrier because the two are built from the same pair of
// sources in opposite order (modem_mode = PMU mode or cell_tech; carrier =
// cell_tech or PMU mode) and cell_tech is the untrustworthy half. The RM500Q-GL
// reports "CDMA BC0" to AT+QNWINFO while camped on 5G/LTE, which pins cell_tech
// — and so carrier — to "Other"; modem_mode still reads 5G from the PMU. Any
// modem whose QNWINFO is honest agrees across both fields, so preferring
// modem_mode costs nothing there.
func cellGeneration(modemMode, carrier string) string {
	for _, s := range []string{modemMode, carrier} {
		switch strings.ToUpper(strings.TrimSpace(s)) {
		case "5G", "NR", "NR5G":
			return "5"
		case "4G", "LTE":
			return "4"
		case "3G", "WCDMA", "HSPA", "UMTS":
			return "3"
		}
	}
	return ""
}

// storeOnceNonEmpty fills a key that is fixed for the process lifetime, but
// only from a value that actually says something. Guarding on presence alone
// is not enough: pcat-manager-web answers dashboard.json before it has probed
// the modem, so an early poll reports "modem_model": "" and a presence-only
// guard would pin that empty string forever — the page-3 celldev row then
// stays blank until restart, because drawing skips empty values. OpenWrt hits
// this on every boot (the display starts seconds before pcat-manager-web, and
// has no mmcli fallback to fill the gap).
func storeOnceNonEmpty(key, val string) {
	if val == "" {
		return
	}
	if _, exists := globalData.Load(key); exists {
		return
	}
	globalData.Store(key, val)
}

func getInfoFromPcatWeb() {
	// Runs on the shared 1-minute cadence (including while the screen is
	// dark) so wake always has fresh dashboard values.
	dashbarodURL := "http://localhost:80/api/v1/dashboard.json"
	networkStatsURL := "http://localhost:80/api/v1/data_stats.json?network_type=mobile"
	basicURL := "http://localhost:80/api/v1/modem/basic.json"

	var info DashboardInfo
	webOK := false

	// === 1) Fetch dashboard.json ===
	resp, err := localHTTPClient.Get(dashbarodURL)
	if err != nil {
		fmt.Println("Could not get dashboard info:", err)
	} else if resp.StatusCode != http.StatusOK {
		// Port 80 answered but it is not pcat-manager-web (e.g. another web
		// server on a Debian install) — treat as unavailable.
		resp.Body.Close()
		fmt.Println("Dashboard endpoint returned:", resp.Status)
	} else {
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Println("Failed to read dashboard response body:", err)
		} else {
			if err2 := secureUnmarshal(body, &info); err2 != nil {
				fmt.Println("Could not unmarshal dashboard info:", err2)
			} else {
				webOK = true
				// Store each field into globalData under a sensible key.
				globalData.Store("BoardTemperature", info.BoardTemperature)
				// Mirror pcat-manager-web's remaining-time so the LCD matches the
				// web home page. Drop the leading "<" the web uses for "less than"
				// (e.g. "< 0:10") — the LCD just shows the bare "H:MM". Record
				// whether it was usable so collectBatteryData() only computes its
				// own value as a fallback.
				if rt := normalizeRemainingTime(info.BatteryRemainingTime); rt != "" {
					globalData.Store("RemainingTime", rt)
					globalData.Store("RemainingTimeFromWeb", true)
					applyRemainingTimeUnit()
				} else {
					globalData.Store("RemainingTimeFromWeb", false)
				}
				globalData.Store("Carrier", info.Carrier)
				globalData.Store("ModemMode", info.ModemMode)
				globalData.Store("CellGeneration",
					cellGeneration(info.ModemMode, info.Carrier))
				globalData.Store("GatewayDevice", info.Connection)
				globalData.Store("ActiveEgress", info.ActiveEgress)
				globalData.Store("NetworkMode", info.NetworkMode)
				globalData.Store("NetworkModeLabel", info.NetworkModeLabel)
				if info.WifiSignalPercent != nil {
					globalData.Store("WifiSignalPercent", *info.WifiSignalPercent)
				} else {
					globalData.Store("WifiSignalPercent", -1)
				}
				globalData.Store("DHCPClientsCount", info.DHCPClientsCount)
				// Firmware / model are fixed for a running process — set once.
				storeOnceNonEmpty("FirmwareVersion", info.FirmwareVersion)
				globalData.Store("ISPName", info.ISPName)
				storeOnceNonEmpty("Model", info.Model)
				storeOnceNonEmpty("ModemModel", info.ModemModel)
				globalData.Store("ModemSignalStrength", info.ModemSignalStrength)
				if info.SdState == 0 {
					globalData.Store("SdState", "No")
				} else {
					globalData.Store("SdState", "Yes")
				}
				globalData.Store("ServerLocation", info.ServerLocation)
				globalData.Store("SimNumber", info.SimNumber)

				if info.SimState == "ready" {
					globalData.Store("SimState", "Yes")
				} else {
					globalData.Store("SimState", "No")
				}

				globalData.Store("WiFiClientsCount", info.WiFiClientsCount)
				globalData.Store("WiFiInterfaces", info.WiFiInterfaces)
				// Public / local WAN IPs: prefer the web's live fields (pcat-manager-web
				// now flushes wan_ip the moment there is no usable uplink and
				// re-probes on cable plug). Fall back to legacy public_ip key.
				pubIP := info.WanIP
				if pubIP == "" {
					pubIP = info.PublicIP
				}
				applyWebPublicIP(pubIP, info.WanIPv6, info.LocalWanIP, info.ActiveEgress)
				globalData.Store("UpSpeedBps", info.UpSpeedBps)
				globalData.Store("DownSpeedBps", info.DownSpeedBps)
				// OS version from pcat-manager-web is authoritative on OpenWrt.
				// Always apply it so an earlier os-release fallback
				// ("photonicatWrt 26.04.1") does not stick for the process
				// lifetime. Short form: "R26.04.1 / r7760" (no git hash).
				if raw := info.OpenWRTVersion; raw != "" {
					if theOS := formatOpenWrtVersionLabel(raw); theOS != "" {
						globalData.Store("OSVersion", theOS)
					}
				}

				// Build a slice of SSIDs for convenience
				var ssids []string
				for _, iface := range info.WiFiInterfaces {
					ssids = append(ssids, iface.SSID)
				}
				globalData.Store("WiFiSSIDs", ssids)
				updateWifiQrPages(info.WiFiInterfaces)

				// Page-3 SSID rows. pcat-manager-web classifies each radio by
				// device path (Onboard/Builtin vs PCIE) and reports which one is
				// relaying the upstream WiFi (its AP parked) — the WAN priority
				// table lets either radio carry the STA, so we can't assume a
				// fixed wifi-iface index here. Map by class and let the web be
				// authoritative; collectNetworkData's uci fallback only runs when
				// pcat-manager-web is unreachable.
				globalData.Store("SSID", radioDisplaySSID(info.WiFiInterfaces, false))
				globalData.Store("SSID2", radioDisplaySSID(info.WiFiInterfaces, true))
			}
		}
	}

	// pcat-manager-web unreachable (not installed or stopped — the normal
	// case on plain Debian): fall back to reading everything we can straight
	// from Linux, and skip the remaining web endpoints this round.
	if !webOK {
		if !pcatWebStateKnown || pcatWebUp.Load() {
			log.Println("pcat-manager-web unavailable, using direct Linux data sources")
		}
		pcatWebStateKnown = true
		pcatWebUp.Store(false)
		pcatWebProbed.Store(true)
		collectLinuxFallbackData()
		return
	}
	if pcatWebStateKnown && !pcatWebUp.Load() {
		log.Println("pcat-manager-web is back, resuming web data sources")
	}
	pcatWebStateKnown = true
	pcatWebUp.Store(true)
	pcatWebProbed.Store(true)

	// === 2) Fetch data_stats.json ===
	resp2, err := localHTTPClient.Get(networkStatsURL)
	if err != nil {
		fmt.Println("Could not get network stats:", err)
	} else {
		defer resp2.Body.Close()
		body2, err := io.ReadAll(resp2.Body)
		if err != nil {
			fmt.Println("Failed to read network stats body:", err)
		} else {
			var stats NetworkStats
			if err3 := secureUnmarshal(body2, &stats); err3 != nil {
				fmt.Println("Could not unmarshal network stats:", err3)
			} else {
				// Now store exactly the fields you want:
				strTodayUsed := fmt.Sprintf("%0.2f", stats.TodayUsed/1024/1024/1024)
				strWeekUsed := fmt.Sprintf("%0.2f", stats.WeekUsed/1024/1024/1024)
				strMonthUsed := fmt.Sprintf("%0.2f", stats.MonthUsed/1024/1024/1024)
				strLastMonthUsed := fmt.Sprintf("%0.2f", stats.LastMonthUsed/1024/1024/1024)

				globalData.Store("DailyDataUsage", strTodayUsed)
				globalData.Store("WeeklyDataUsage", strWeekUsed)
				globalData.Store("MonthlyDataUsage", strMonthUsed)
				globalData.Store("LastMonthUsage", strLastMonthUsed)
			}
		}
	}

	// 3) Modem basic
	if resp, err := localHTTPClient.Get(basicURL); err != nil {
		fmt.Println("Could not get modem basic info:", err)
	} else {
		defer resp.Body.Close()
		if body, err := io.ReadAll(resp.Body); err != nil {
			fmt.Println("Failed to read modem basic body:", err)
		} else {
			var mb ModemBasicInfo
			if err := secureUnmarshal(body, &mb); err != nil {
				fmt.Println("Could not unmarshal modem basic info:", err)
			} else {
				globalData.Store("CellCarrierInfo", mb.CellCarrierInfo)
				// Firmware / IMEI don't change at runtime — set once.
				if _, exists := globalData.Load("ModemFirmwareVer"); !exists {
					globalData.Store("ModemFirmwareVer", mb.FirmwareVersion)
				}
				if _, exists := globalData.Load("IMEINum"); !exists {
					globalData.Store("IMEINum", mb.IMEINum)
				}
				globalData.Store("ModemCellID", mb.ModemCellID)
				globalData.Store("ModemCellInfo", mb.ModemCellInfo)
				globalData.Store("ModemSignals", mb.ModemCellSignals)
				globalData.Store("ModemISPDetails", mb.ModemIspDetails)

				networkInfo := mb.ModemNetworkInfo
				if strings.Contains(networkInfo, "BAND ") {
					networkInfo = strings.ReplaceAll(networkInfo, "BAND ", "B.")
				}

				globalData.Store("ModemNetworkInfo", networkInfo)

				globalData.Store("ModemRoamPref", mb.ModemRoamPref)
				globalData.Store("ModemServingInfo", mb.ModemServingInfo)
				globalData.Store("ModemServingQual", mb.ModemServingQuality)
				globalData.Store("ModemUSBSpeed", mb.ModemUSBSpeed)
				globalData.Store("ModemUSBNetMode", mb.ModemUSBNetMode)
				globalData.Store("ModemValid", mb.ModemValid)
				globalData.Store("PolicyLTEBands", mb.PolicyLTEBands)
				globalData.Store("PolicyNR5GBands", mb.PolicyNR5GBands)
				globalData.Store("SelectedLTEBands", mb.SelectedLTEBands)
				globalData.Store("SelectedNR5GBands", mb.SelectedNR5GBands)
				globalData.Store("SMSCheckInterval", mb.SMSCheckInterval)
				globalData.Store("SMSForward", mb.SMSForward)
				globalData.Store("SMSForwardTo", mb.SMSForwardTo)
				globalData.Store("ModemTemperature", mb.ModemTemperature)
			}
		}
	}
}

// formatSpeed formats speed into value and units as Mbps
func formatSpeed(mbps float64) (string, string) {
	if mbps > 100000 || mbps < 0.0 { //clamping
		mbps = 0.0
	}

	if mbps >= 1.0 {
		// For speeds ≥1 Mbps, use 3 significant digits
		return fmt.Sprintf("%.3g", mbps), "Mbps"
	}
	// For speeds <1 Mbps, keep up to 3 digits after decimal point
	return fmt.Sprintf("%.2f", mbps), "Mbps"
}

// getWANInterface returns the netdev carrying the lowest-metric IPv4 default
// route, read from /proc/net/route - immune to which `ip` applet the PATH
// serves and cheap enough to resolve every cycle. Falls back to br-lan on
// OpenWrt so there is something to watch while no uplink is routed.
func getWANInterface() (string, error) {
	if data, err := os.ReadFile(pdCovProcNetRoutePath); err == nil {
		lines := strings.Split(string(data), "\n")
		best, bestMetric := "", int64(-1)
		for _, line := range lines[1:] {
			f := strings.Fields(line)
			if len(f) < 8 || f[1] != "00000000" {
				continue
			}
			metric, _ := strconv.ParseInt(f[6], 10, 64)
			if bestMetric == -1 || metric < bestMetric {
				best, bestMetric = f[0], metric
			}
		}
		if best != "" {
			return best, nil
		}
	}
	if isOpenWRT() {
		return "br-lan", nil
	}
	return "", fmt.Errorf("WAN interface not found")
}

func collectWANNetworkSpeed() {
	// Delta counter read on the default-route device: the sampler diffs against
	// its previous samples, so there is no ~1s sleep here. While page 0 is
	// visible the collector runs at 2 Hz, so the row tracks the live cadence.
	dev, err := getWANInterface()
	if err != nil {
		log.Printf("Could not get WAN interface: %v\n", err)
		storeWANSpeed(0, 0)
		return
	}
	// Re-resolved every cycle so a failover moves the measurement to the new
	// egress within a tick; also shared with the data-usage readers.
	wanInterface = dev

	netData, ok, err := wanSpeedSampler.sample(wanInterface)
	if err != nil {
		log.Printf("Could not get network speed: %v\n", err)
		storeWANSpeed(0, 0)
		return
	}
	if !ok {
		// No window to divide by yet (first tick, or just after a failover).
		// Seed the row with zeros if it has never been filled, otherwise leave
		// the previous number up until the next tick - 0.5s away on page 0.
		if _, exists := globalData.Load("WanUP"); !exists {
			storeWANSpeed(0, 0)
		}
		return
	}
	storeWANSpeed(netData.UploadMbps, netData.DownloadMbps)
}

// storeWANSpeed formats and publishes the page-0 speed row.
func storeWANSpeed(upMbps, downMbps float64) {
	wanUPVal, wanUPUnit := formatSpeed(upMbps)
	wanDOWNVal, wanDOWNUnit := formatSpeed(downMbps)
	globalData.Store("WanUP", wanUPVal)
	globalData.Store("WanDOWN", wanDOWNVal)
	globalData.Store("WanUP_Unit", wanUPUnit)
	globalData.Store("WanDOWN_Unit", wanDOWNUnit)
}

// collectFixedData fills values that never change for the life of the process
// (kernel build date, serial number, device-tree model). OSVersion is filled
// by collectLinuxFallbackData from /etc/os-release, then upgraded to the
// short OpenWrt form ("R26.04.1 / r7760") once pcat-manager-web answers.
func collectFixedData() {
	kernelDate, _ := getKernelDate()
	globalData.Store("Kernel", kernelDate)
	sn, _ := getSN()
	globalData.Store("SN", sn)
	if _, exists := globalData.Load("Model"); !exists {
		if model := getDeviceTreeModel(); model != "" {
			globalData.Store("Model", model)
		}
	}
}

// collectPowerData samples the battery and DC rails. Split out of
// collectLinuxData so collectBatteryData can refresh these at page 0's fast
// cadence - the voltage/wattage row is drawn there, and the full Linux sweep
// (CPU, memory, disks) is too heavy to drag along at 2 Hz.
func collectPowerData() {
	// Battery voltage.
	voltageUV, err := getBatteryVoltageUV()
	if err != nil {
		fmt.Printf("Could not get battery voltage: %v\n", err)
		globalData.Store("BatteryVoltage", "N/A")
	} else {
		voltage_2digit := fmt.Sprintf("%0.2f", voltageUV/1000/1000)
		globalData.Store("BatteryVoltage", voltage_2digit)
	}

	// Battery current.
	currentUA, err := getBatteryCurrentUA()
	if err != nil {
		fmt.Printf("Could not get battery current: %v\n", err)
		globalData.Store("BatteryCurrent", -9999)
	} else {
		current_2digit := fmt.Sprintf("%0.2f", currentUA/1000/1000)
		globalData.Store("BatteryCurrent", current_2digit)
	}

	// Battery wattage. Above 20 W there isn't room (and little value) in the
	// decimal, so drop it; keep one decimal for the finer low-power range.
	wattage := float64(voltageUV) * float64(currentUA) / 1000 / 1000 / 1000 / 1000
	var wattageStr string
	if wattage > 20 {
		wattageStr = fmt.Sprintf("%0.0f", wattage)
	} else {
		wattageStr = fmt.Sprintf("%0.1f", wattage)
	}
	globalData.Store("BatteryWattage", wattageStr)

	// DC voltage.
	dcVoltageUV, err := getDCVoltageUV()
	if err != nil {
		fmt.Printf("Could not get DC voltage: %v\n", err)
		globalData.Store("DCVoltage", -9999)
	} else {
		globalData.Store("DCVoltage", fmt.Sprintf("%0.1f", dcVoltageUV/1000/1000))
	}
}

// collectData gathers several pieces of system and network information and stores them in globalData.
func collectLinuxData(cfg Config) {
	if uptime, err := getUptime(); err != nil {
		fmt.Printf("Could not get uptime: %v\n", err)
		globalData.Store("Uptime", "N/A")
	} else {
		globalData.Store("Uptime", uptime)
	}

	collectPowerData()

	// CPU temperature.
	if cpuTemp, err := getCpuTemp(); err != nil {
		fmt.Printf("Could not get CPU temperature: %v\n", err)
		globalData.Store("CpuTemp", -9999)
	} else {
		cpuTemp_1digit := fmt.Sprintf("%0.1f", cpuTemp/1000)
		globalData.Store("CpuTemp", cpuTemp_1digit)
	}

	// CPU usage. Sample once via getCpuUsages() so we get the per-core slice
	// (for the 8-bar chart) and the aggregate from the same measurement window
	// — calling it twice would rotate the /proc/stat snapshot and halve each
	// window.
	if cpus, err := getCpuUsages(); err != nil {
		fmt.Printf("Could not get CPU usage: %v\n", err)
		globalData.Store("CpuUsage", 0)
		globalData.Store("CpuUsages", nil)
	} else {
		total := 0.0
		for _, c := range cpus {
			total += c
		}
		avg := 0.0
		if len(cpus) > 0 {
			avg = total / float64(len(cpus))
		}
		globalData.Store("CpuUsage", int(avg))
		// Per-core usages (0-100) for the cpu_bars element.
		globalData.Store("CpuUsages", cpus)
	}

	// Memory usage.
	if memUsed, memTotal, err := getMemUsedAndTotalGB(); err != nil {
		fmt.Printf("Could not get memory usage: %v\n", err)
		globalData.Store("MemUsage", nil)
		globalData.Store("MemUsagePercent", nil)
	} else {
		memUsed_1digit := fmt.Sprintf("%0.1f", memUsed)
		memTotal_ceilInt := int(math.Ceil(memTotal))
		memString := fmt.Sprintf("%s/%d", memUsed_1digit, memTotal_ceilInt)
		globalData.Store("MemUsage", memString)
		// Used fraction (0-100) for the memory hbar element.
		memPct := 0.0
		if memTotal > 0 {
			memPct = memUsed / memTotal * 100
		}
		globalData.Store("MemUsagePercent", memPct)
	}

	// Disk usage.
	if diskData, err := getDiskUsage(); err != nil {
		fmt.Printf("Could not get disk usage: %v\n", err)
		globalData.Store("DiskData", nil)
	} else {
		globalData.Store("DiskData", diskData)
	}

	// Per-disk "used/total" GB strings (root / NVMe / SD card) for the display.
	collectDiskUsage()

	//Fan speed
	fanSpeed, err := getFanSpeed()
	if err != nil {
		// No hwmon fan node (e.g. no photonicat-pm kernel driver): use the
		// value from the direct PMU UART status report when available.
		if rpm, ok := pmuUartFanRPM(); ok {
			globalData.Store("FanRPM", rpm)
		} else {
			fmt.Printf("Could not get fan speed: %v\n", err)
			globalData.Store("FanRPM", "N/A")
		}
	} else {
		globalData.Store("FanRPM", fanSpeed)
	}
}

// getFanSpeed scans /sys/class/hwmon/hwmon*/fan1_input and returns the first
// valid integer it reads.
func getFanSpeed() (int, error) {
	// Glob all fan1_input files in hwmon directories
	paths, err := filepath.Glob(pdCovHwmonFanGlob)
	if err != nil {
		return 0, fmt.Errorf("failed to glob hwmon paths: %w", err)
	}

	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			// skip files we can't read
			continue
		}
		s := strings.TrimSpace(string(data))
		if s == "" {
			continue
		}

		speed, err := strconv.Atoi(s)
		if err != nil {
			// skip non-integer contents
			continue
		}
		return speed, nil
	}
	return 0, fmt.Errorf("no valid fan1_input found under /sys/class/hwmon")
}

// collectNetworkData gathers IPs, SSIDs, clients, and data-usage counters.
// Ping lives in pingRow.collect (one goroutine per row) so it can run on a
// faster cadence when the ping page is visible. This still runs every minute
// while the screen is dark so wake does not show stale IPs/usage.
func collectNetworkData(cfg Config) {
	if isOpenWRT() {
		//we have aonther func to get data from pcat-manager-web
	} else {
		if sessionDataUsage, err := getSessionDataUsageGB(wanInterface); err != nil {
			fmt.Printf("Could not get session data usage: %v\n", err)
			globalData.Store("SessionDataUsage", nil)
		} else {
			sessionDataUsage_1digit := fmt.Sprintf("%0.1f", sessionDataUsage)
			globalData.Store("SessionDataUsage", sessionDataUsage_1digit)
		}

		if monthlyDataUsage, err := getDataUsageMonthlyGB(wanInterface); err != nil {
			fmt.Printf("Could not get monthly data usage: %v\n", err)
			globalData.Store("MonthlyDataUsage", nil)
		} else {
			monthlyDataUsage_1digit := fmt.Sprintf("%0.1f", monthlyDataUsage)
			globalData.Store("MonthlyDataUsage", monthlyDataUsage_1digit)
		}
	}

	// Local IP address.
	if localIP, err := getLocalIPv4(); err != nil {
		fmt.Printf("Could not get local IP: %v\n", err)
		globalData.Store("LAN_IP", "N/A")
	} else {
		globalData.Store("LAN_IP", localIP)
	}

	// WAN IP + public IP: collectLinkStatus owns the fast path when the
	// screen is awake; this minute-cadence pass is the backstop (and the
	// only path while dark) so wake never shows a 15-minute-old public IP
	// from a previous uplink.
	applyLocalNetworkIPs()

	// SSID rows (page 3). When pcat-manager-web is up it already populated
	// SSID/SSID2 from its per-radio wifi_interfaces (which knows onboard vs PCIe
	// and which radio relays the upstream WiFi), so don't clobber that with the
	// coarser uci reads below — those can't tell which radio carries the STA and
	// would show a stale name for a disconnected upstream. Only fall back to uci
	// when the web is unavailable (Debian, or pcat-manager-web stopped). On
	// OpenWrt, wait for the first dashboard.json poll before using the fallback
	// so we don't briefly flash a coarse uci SSID at boot before the authoritative
	// per-radio value arrives; on Debian there is no web, so run it immediately.
	if !pcatWebUp.Load() && (pcatWebProbed.Load() || !isOpenWRT()) {
		if ssid, err := getSSID(); err != nil {
			//fmt.Printf("Could not get SSID: %v\n", err)
			globalData.Store("SSID", "N/A")
		} else {
			globalData.Store("SSID", ssid)
		}

		if ssid2, err := getSSID2(); err != nil {
			//fmt.Printf("Could not get SSID: %v\n", err)
			globalData.Store("SSID2", "N/A")
		} else {
			globalData.Store("SSID2", ssid2)
		}
	}

	// DHCP clients (OpenWrt).
	if dhcpClients, err := getDHCPClients(); err != nil {
		fmt.Printf("Could not get DHCP clients: %v\n", err)
		globalData.Store("DHCPClients", nil)
	} else {
		globalData.Store("DHCPClients", dhcpClients)
	}

	// WiFi clients (OpenWrt).
	if wifiClients, err := getWifiClients(); err != nil {
		fmt.Printf("Could not get WiFi clients: %v\n", err)
		globalData.Store("WifiClients", nil)
	} else {
		globalData.Store("WifiClients", wifiClients)
	}
}

// pingProbeDeadline bounds how long a row waits for its own probe. pingICMP
// gives up at pingTimeout, but ping.NewPinger resolves the hostname
// synchronously first and a wedged resolver is not covered by that timeout -
// without a deadline here a dead DNS server could park the row for tens of
// seconds. The margin is just enough to let a probe that is about to time out
// report itself rather than be pre-empted.
// (var, not const, only so tests can shorten it.)
var pingProbeDeadline = pingTimeout + 500*time.Millisecond

// pingProbe is the ICMP probe a row runs; a var so tests can substitute a
// slow or hanging host without touching the network. pingProbeTCP and
// pingProbeHTTP are the other two modes' probes, vars for the same reason.
var (
	pingProbe      = pingICMP
	pingProbeTCP   = pingTCP
	pingProbeHTTP  = pingHTTP
	pingProbeHTTPS = pingHTTPS
)

// probeForPingType picks a row's probe from its configured ping type
// ("icmp"/"tcp"/"http"/"https"; anything else normalizes to ICMP — see
// pingModes.go).
func probeForPingType(pingType string) func(string) (int64, error) {
	switch normalizePingType(pingType) {
	case pingTypeTCP:
		return pingProbeTCP
	case pingTypeHTTP:
		return pingProbeHTTP
	case pingTypeHTTPS:
		return pingProbeHTTPS
	default:
		return pingProbe
	}
}

// pingRow owns one ping row (Ping0/Ping1): its display keys, its success
// counters and its single in-flight probe. Each row is polled by its own
// collector goroutine, so a slow or unreachable host only stretches its own
// row - the other row keeps its 1 Hz cadence. Before this, both rows shared one
// goroutine that waited on both probes, so one 5s timeout stalled the healthy
// row (and the whole next cycle) too.
type pingRow struct {
	valueKey string
	rateKey  string

	mu sync.Mutex
	// site/pingType are the target the counters below describe; when the
	// config hands collect a different pair the counters restart, so the %
	// on screen never mixes the old target's history into the new one.
	site        string
	pingType    string
	total       int
	successful  int
	lastSuccess int64

	// inFlight is held from the moment a probe starts until it returns, even if
	// the collector already gave up waiting for it. It keeps a wedged probe from
	// accumulating goroutines behind every subsequent tick.
	inFlight atomic.Bool
}

var (
	pingRow0 = &pingRow{valueKey: "Ping0", rateKey: "Ping0Rate", lastSuccess: -1}
	pingRow1 = &pingRow{valueKey: "Ping1", rateKey: "Ping1Rate", lastSuccess: -1}
)

// collect runs one probe for this row and publishes the result. It returns as
// soon as the probe answers, or at pingProbeDeadline - whichever comes first -
// so the row's cadence never depends on how long a broken host takes to fail.
func (r *pingRow) collect(site, pingType string) {
	if !r.inFlight.CompareAndSwap(false, true) {
		// The previous probe is still out there (wedged resolver, black-holed
		// route). Don't queue another one behind it; the row already shows the
		// timeout marker this tick's predecessor published.
		return
	}

	probe := probeForPingType(pingType)

	// A row's success rate describes one target. On a site or mode change the
	// old counters describe the wrong thing — start them over. This sits after
	// the CAS on purpose: only the call that actually probes may reset, so a
	// wedged old-target probe can never publish into the fresh counters (its
	// result is only ever published by its own collect call, which has already
	// returned).
	r.mu.Lock()
	if r.site != site || r.pingType != pingType {
		r.site, r.pingType = site, pingType
		r.total, r.successful = 0, 0
		r.lastSuccess = -1
	}
	r.mu.Unlock()

	// Buffered so an abandoned probe can deliver its result and exit instead of
	// blocking forever on a receiver that has moved on.
	done := make(chan int64, 1)
	go func() {
		pingMs, err := probe(site)
		if err != nil {
			pingMs = -1
		}
		r.inFlight.Store(false)
		done <- pingMs
	}()

	timer := time.NewTimer(pingProbeDeadline)
	defer timer.Stop()
	select {
	case pingMs := <-done:
		r.publish(pingMs)
	case <-timer.C:
		// Probe overran its own timeout: show the timeout marker now and let it
		// finish in the background (its late result is dropped).
		r.publish(-2)
	}
}

// publish records one probe outcome and stores the row's display value and
// success rate. pingMs is the round-trip in ms, -2 for a timeout (red X), or
// -1/0 for any other failure - which keeps the last good value on screen so a
// single dropped packet doesn't blank the row.
func (r *pingRow) publish(pingMs int64) {
	r.mu.Lock()
	r.total++
	value := pingMs
	switch {
	case pingMs > 0:
		r.successful++
		r.lastSuccess = pingMs
	case pingMs == -2:
		// Timeout: leave value at -2 so the row draws the red X.
	case r.lastSuccess > 0:
		value = r.lastSuccess
	default:
		value = -1
	}
	successRate := float64(r.successful) / float64(r.total) * 100
	r.mu.Unlock()

	globalData.Store(r.valueKey, value)
	globalData.Store(r.rateKey, fmt.Sprintf("%.0f", successRate))
}

// Public IPs only change when the upstream connection does, so they are
// cached: refreshed at startup, when the local WAN IP / egress class changes,
// or after publicIPRefreshInterval — not on every collect cycle. Fetching
// them every cycle kept the modem radio out of its low-power state around the
// clock and was the single largest battery cost of the app.
//
// Offline (no usable default route) explicitly flushes the displayed value so
// a yanked cable does not leave the previous public IP on the LCD. Coming
// back online forces a re-probe instead of waiting out the 15-minute cache.
const publicIPRefreshInterval = 15 * time.Minute

var (
	publicIPMu         sync.Mutex
	publicIPLastFetch  time.Time
	publicIPWanBasis   string
	publicIPEgress     string
	publicIPFetching   bool
	publicIPHadOnline  bool
)

// flushPublicIPs clears the displayed public addresses and the cache basis so
// the next online transition re-probes immediately.
func flushPublicIPs() {
	publicIPMu.Lock()
	publicIPWanBasis = ""
	publicIPEgress = ""
	publicIPLastFetch = time.Time{}
	publicIPHadOnline = false
	publicIPMu.Unlock()
	globalData.Store("PUBLIC_IP", "N/A")
	globalData.Store("PublicIP", "N/A")
	globalData.Store("PublicIPv6", "0.0.0.0")
}

// applyWebPublicIP mirrors pcat-manager-web's live wan_ip into the LCD keys.
// Empty + no egress means offline (flush); empty with egress means "still
// probing" — leave the local cache alone so a concurrent local fetch can fill
// it. Non-empty adopts the web value and freezes the local cache timer.
func applyWebPublicIP(wanIP, wanIPv6, localWanIP, egress string) {
	if egress == "" || (wanIP == "" && localWanIP == "") {
		// Offline (or web has not resolved anything and reports no path):
		// clear so the LCD does not keep a previous uplink's address.
		if egress == "" {
			flushPublicIPs()
			if localWanIP == "" {
				globalData.Store("WAN_IP", "N/A")
			}
		}
		return
	}
	if localWanIP != "" {
		globalData.Store("WAN_IP", localWanIP)
	}
	if wanIP != "" {
		globalData.Store("PUBLIC_IP", wanIP)
		globalData.Store("PublicIP", wanIP)
		publicIPMu.Lock()
		publicIPLastFetch = time.Now()
		publicIPWanBasis = localWanIP
		publicIPEgress = egress
		publicIPHadOnline = true
		publicIPMu.Unlock()
	}
	if wanIPv6 != "" {
		globalData.Store("PublicIPv6", wanIPv6)
	}
}

// applyLocalNetworkIPs refreshes WAN_IP / PUBLIC_IP from the kernel (used when
// pcat-manager-web is down, and as a fast path from collectLinkStatus).
func applyLocalNetworkIPs() {
	dev := getDefaultRouteDev()
	egress, conn, _ := classifyEgress(dev)
	// Keep the top-bar icon honest even when the web dashboard poll is on a
	// 60s cadence (any page other than page 0, or screen dark).
	globalData.Store("ActiveEgress", egress)
	globalData.Store("GatewayDevice", conn)

	wanIP, err := getWanIPv4()
	if err != nil || wanIP == "" || wanIP == "N/A" || egress == "" {
		globalData.Store("WAN_IP", "N/A")
		flushPublicIPs()
		return
	}
	globalData.Store("WAN_IP", wanIP)
	updatePublicIPs(wanIP, egress)
}

// collectLinkStatus is the cheap, always-on link watcher: carrier-aware
// default route + public-IP flush/reprobe. Runs every few seconds while the
// screen is awake so unplug/plug is visible on the top bar and PUBLIC IP row
// without waiting for the page-0 / 60s collectors.
func collectLinkStatus() {
	applyLocalNetworkIPs()
}

// updatePublicIPs stores PUBLIC_IP and PublicIPv6, hitting the network only
// when the cache is stale, the WAN IP / egress class it was fetched behind has
// changed, or we just came back online. After a failed IPv4 fetch the next
// retry is allowed in 30s instead of a full interval, so a WAN that just came
// up doesn't show N/A for 15 minutes.
func updatePublicIPs(wanIP, egress string) {
	if wanIP == "" || wanIP == "N/A" || egress == "" {
		flushPublicIPs()
		return
	}

	publicIPMu.Lock()
	cameOnline := !publicIPHadOnline
	needFetch := publicIPLastFetch.IsZero() ||
		time.Since(publicIPLastFetch) >= publicIPRefreshInterval ||
		wanIP != publicIPWanBasis ||
		egress != publicIPEgress ||
		cameOnline
	if needFetch && !publicIPFetching {
		publicIPFetching = true
		publicIPLastFetch = time.Now()
		publicIPWanBasis = wanIP
		publicIPEgress = egress
		publicIPHadOnline = true
	} else {
		needFetch = false
	}
	publicIPMu.Unlock()
	if !needFetch {
		return
	}

	fetchedV4 := false
	if publicIP, err := getPublicIPv4(); err != nil {
		fmt.Printf("Could not get public IP: %v\n", err)
		globalData.Store("PUBLIC_IP", "N/A")
		globalData.Store("PublicIP", "N/A")
	} else {
		globalData.Store("PUBLIC_IP", publicIP)
		globalData.Store("PublicIP", publicIP)
		fetchedV4 = true
	}

	// IPv6 failure is normal on v4-only uplinks and doesn't shorten the retry.
	if ipv6, err := getIPv6Public(); err != nil {
		globalData.Store("PublicIPv6", "0.0.0.0")
	} else {
		globalData.Store("PublicIPv6", ipv6)
	}

	publicIPMu.Lock()
	publicIPFetching = false
	if !fetchedV4 {
		// Retry in 30s: set lastFetch so (now - lastFetch) >= interval - 30s.
		publicIPLastFetch = time.Now().Add(30 * time.Second).Add(-publicIPRefreshInterval)
	}
	publicIPMu.Unlock()
}

func getSN() (string, error) {
	// Read first 500 bytes
	out, err := secureExecCommand("head", "-c", "10000", "/dev/mmcblk0boot1")
	if err != nil {
		return "", fmt.Errorf("read partition: %w", err)
	}

	// Truncate at first 0 byte
	if idx := bytes.IndexByte(out, 0); idx != -1 {
		out = out[:idx]
	}

	// Parse JSON
	var payload map[string]interface{}
	if err := secureUnmarshal(out, &payload); err != nil {
		return "", fmt.Errorf("unmarshal JSON: %w", err)
	}

	// Extract "sn" or fallback to "machine_sn"
	var sn string
	if v, ok := payload["sn"]; ok {
		if s, ok2 := v.(string); ok2 && s != "" {
			sn = s
		}
	}
	if sn == "" {
		if v, ok := payload["machine_sn"]; ok {
			if s, ok2 := v.(string); ok2 && s != "" {
				sn = s
			}
		}
	}
	if sn == "" {
		return "", fmt.Errorf(`key "sn" or "machine_sn" not found or not a non-empty string`)
	}

	return sn, nil
}

// getUptimeSeconds returns system uptime in seconds
func getUptimeSeconds() (float64, error) {
	// Read /proc/uptime
	data, err := os.ReadFile(pdCovProcUptimePath)
	if err != nil {
		return 0, fmt.Errorf("error reading /proc/uptime: %v", err)
	}

	// Parse the first value (uptime in seconds)
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, fmt.Errorf("invalid uptime data")
	}

	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("error parsing uptime: %v", err)
	}

	return seconds, nil
}

func getUptime() (string, error) {
	seconds, err := getUptimeSeconds()
	if err != nil {
		return "", err
	}

	// Convert seconds to time.Duration
	uptime := time.Duration(seconds) * time.Second

	// Calculate days, hours, minutes, and seconds
	days := int(uptime.Hours()) / 24
	hours := int(uptime.Hours()) % 24
	minutes := int(uptime.Minutes()) % 60
	secs := int(uptime.Seconds()) % 60

	// Build human-readable string, omitting zero values
	var parts []string
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if secs > 0 || len(parts) == 0 { // Include seconds if zero to avoid empty string
		parts = append(parts, fmt.Sprintf("%ds", secs))
	}

	return strings.Join(parts, " "), nil
}

func getKernelDate() (string, error) {
	// get kernel version (release)
	buildOut, err := secureExecCommand("uname", "-v")
	display_date_str := "unknown-date"
	if err == nil {
		raw := strings.TrimSpace(string(buildOut))
		parts := strings.Split(raw, " ")
		if len(parts) >= 9 { //#15 SMP PREEMPT Wed Apr 30 17:23:30 JST 2025 //debian
			display_date_str = fmt.Sprintf("%s-%s-%s", parts[8], parts[4], parts[5])
		} else if len(parts) >= 8 { //#0 SMP PREEMPT Wed May 14 09:34:38 2025 //openwrt
			display_date_str = fmt.Sprintf("%s-%s-%s", parts[7], parts[4], parts[5])
		}
	}

	return fmt.Sprintf("%s", display_date_str), nil
}

// getDCVoltageUV reads DC voltage from the system.
func getDCVoltageUV() (float64, error) {
	file, err := os.Open(pdCovChargerVoltagePath)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	rawUV, err := strconv.ParseFloat(strings.TrimSpace(string(content)), 64)
	if err != nil {
		return 0.0, err
	}
	if rawUV < 1*1000*1000 {
		return 0.0, nil
	}
	return rawUV, nil
}

// getInterfaceBytes reads rx and tx bytes for a given interface.
func getInterfaceBytes(iface string) (rxBytes, txBytes uint64, err error) {
	basePath := pdCovSysClassNetDir + "/" + iface + "/statistics/"
	rxPath := basePath + "rx_bytes"
	txPath := basePath + "tx_bytes"

	readBytes := func(path string) (uint64, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, err
		}
		val, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		return val, err
	}

	rxBytes, err = readBytes(rxPath)
	if err != nil {
		return
	}
	txBytes, err = readBytes(txPath)
	return
}

func isOpenWRT() bool {
	if _, err := os.Stat(pdCovOpenwrtReleasePath); err == nil {
		return true
	}
	return false
}

// radioDisplaySSID picks the page-3 SSID string for one physical radio from the
// pcat-manager-web wifi_interfaces list. wantPCIe selects the PCIe radio;
// otherwise the onboard radio (device_type "Onboard"/"Builtin"). When the radio
// is relaying the upstream WiFi (WAN priority "wifi" slot, AP parked) it shows
// the upstream hotspot's SSID while associated, or "Standby" while it has not
// (re)joined — never the stale configured name. A radio that isn't present
// yields "N/A", matching the other page-3 rows.
func radioDisplaySSID(ifaces []WiFiInterface, wantPCIe bool) string {
	for _, iface := range ifaces {
		isPCIe := iface.DeviceType == "PCIE"
		if isPCIe != wantPCIe {
			continue
		}
		if iface.WiFiWan {
			// Relaying an upstream hotspot: show what we're actually joined to,
			// or standby when the STA is enabled but not associated.
			if iface.WiFiWanAssociated && iface.WiFiWanSSID != "" {
				return iface.WiFiWanSSID
			}
			return "Standby"
		}
		return iface.SSID
	}
	return "N/A"
}

// getOpenWrtStaSSID reports the upstream STA (WiFi-as-WAN) link state read
// directly from uci/iwinfo. This is the fallback used only when
// pcat-manager-web is unreachable; when it is up, page-3 SSIDs come from its
// per-radio wifi_interfaces instead (see radioDisplaySSID).
//
// Returns:
//   - configured: a wifi-iface in mode=sta exists (the WAN priority wifi slot)
//   - ssid:       the hotspot it is currently *associated* to, or "" when the
//                 STA is enabled but not joined (standby). The stale configured
//                 ssid is deliberately not returned in that case.
func getOpenWrtStaSSID() (ssid string, configured bool) {
	out, err := secureExecCommand("uci", "-q", "show", "wireless")
	if err != nil {
		return "", false
	}
	staSection := ""
	reMode := regexp.MustCompile(`^wireless\.([^.]+)\.mode='?sta'?$`)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if m := reMode.FindStringSubmatch(line); m != nil {
			staSection = m[1]
			break
		}
	}
	if staSection == "" {
		return "", false
	}
	// The STA iface is configured. Its live SSID (only present while actually
	// associated) comes from iwinfo on the sta netdev, not from the uci config
	// value — which persists across disconnects and would be stale.
	return liveStaSSID(), true
}

// liveStaSSID returns the SSID the station iface is currently associated to,
// or "" when not associated. Resolves the sta netdev from `iwinfo`, then reads
// its live ESSID. Empty on any error (treated as not-associated / standby).
func liveStaSSID() string {
	out, err := secureExecCommand("iwinfo")
	if err != nil {
		return ""
	}
	// `iwinfo` lists each iface then its fields; the sta netdev shows
	// `Mode: Client` and `ESSID: "<hotspot>"` (or `ESSID: unknown` when idle).
	reESSID := regexp.MustCompile(`ESSID:\s*"(.*?)"`)
	reMode := regexp.MustCompile(`Mode:\s*(\S+)`)
	var curMode, curESSID string
	for _, line := range strings.Split(string(out), "\n") {
		// A new iface block starts at a non-indented "phyX-staY  ESSID: ..." line.
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
			// Previous block ended; check if it was an associated client.
			if curMode == "Client" && curESSID != "" {
				return curESSID
			}
			curMode, curESSID = "", ""
		}
		if m := reESSID.FindStringSubmatch(line); m != nil {
			curESSID = m[1]
		}
		if m := reMode.FindStringSubmatch(line); m != nil {
			curMode = m[1]
		}
	}
	if curMode == "Client" && curESSID != "" {
		return curESSID
	}
	return ""
}

// getSSID returns connected SSID on Debian or broadcasting SSID on OpenWrt.
func getSSID() (string, error) {
	// OpenWrt detection
	if isOpenWRT() {
		// Smart WAN: when a radio relays an upstream hotspot it runs a station
		// (sta) iface. Show the hotspot we are actually joined to rather than
		// the parked AP's (often stale "OpenWrt") SSID; show "Standby" when the
		// STA is configured but not currently associated.
		if sta, configured := getOpenWrtStaSSID(); configured {
			if sta != "" {
				return sta, nil
			}
			return "Standby", nil
		}
		out, err := secureExecCommand("uci", "get", "wireless.@wifi-iface[0].ssid")
		if err != nil {
			return "", fmt.Errorf("failed to get OpenWrt SSID: %v", err)
		}
		return strings.TrimSpace(string(out)), nil
	}

	// Debian/Ubuntu: Try iwgetid first
	if out, err := secureExecCommand("iwgetid", "-r"); err == nil {
		ssid := strings.TrimSpace(string(out))
		if ssid != "" {
			return ssid, nil
		}
	}

	// Fallback 1: iwconfig
	if out, err := secureExecCommand("iwconfig"); err == nil {
		re := regexp.MustCompile(`ESSID:"(.*?)"`)
		matches := re.FindSubmatch(out)
		if len(matches) >= 2 {
			ssid := string(matches[1])
			if ssid != "" && ssid != "off/any" {
				return ssid, nil
			}
		}
	}

	// Fallback 2: nmcli (NetworkManager)
	if out, err := secureExecCommand("nmcli", "-t", "-f", "active,ssid", "dev", "wifi"); err == nil {
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			fields := strings.Split(line, ":")
			if len(fields) == 2 && fields[0] == "yes" && fields[1] != "" {
				return fields[1], nil
			}
		}
	}

	return "", fmt.Errorf("SSID could not be determined")
}

// getSSID2 returns the second radio's SSID on OpenWrt (or the connected SSID on
// Debian). Fallback for when pcat-manager-web is unreachable.
func getSSID2() (string, error) {
	// OpenWrt detection
	if isOpenWRT() {
		// The second wifi-iface may itself be the upstream STA (the WAN priority
		// wifi slot can sit on either radio). If so, report the live upstream /
		// standby rather than the stale configured sta ssid.
		if mode, err := secureExecCommand("uci", "-q", "get",
			"wireless.@wifi-iface[1].mode"); err == nil &&
			strings.TrimSpace(string(mode)) == "sta" {
			if sta := liveStaSSID(); sta != "" {
				return sta, nil
			}
			return "Standby", nil
		}
		out, err := secureExecCommand("uci", "get", "wireless.@wifi-iface[1].ssid")
		if err != nil {
			return "", fmt.Errorf("failed to get OpenWrt SSID: %v", err)
		}
		return strings.TrimSpace(string(out)), nil
	}

	// Debian/Ubuntu: Try iwgetid first
	if out, err := secureExecCommand("iwgetid", "-r"); err == nil {
		ssid := strings.TrimSpace(string(out))
		if ssid != "" {
			return ssid, nil
		}
	}

	// Fallback 1: iwconfig
	if out, err := secureExecCommand("iwconfig"); err == nil {
		re := regexp.MustCompile(`ESSID:"(.*?)"`)
		matches := re.FindSubmatch(out)
		if len(matches) >= 2 {
			ssid := string(matches[1])
			if ssid != "" && ssid != "off/any" {
				return ssid, nil
			}
		}
	}

	// Fallback 2: nmcli (NetworkManager)
	if out, err := secureExecCommand("nmcli", "-t", "-f", "active,ssid", "dev", "wifi"); err == nil {
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			fields := strings.Split(line, ":")
			if len(fields) == 2 && fields[0] == "yes" && fields[1] != "" {
				return fields[1], nil
			}
		}
	}

	return "", fmt.Errorf("SSID could not be determined")
}

const (
	// netSpeedWindow is the measurement window the sampler aims for: the delta
	// is taken against the newest sample that is at least this old, so a 2 Hz
	// collector still averages over ~1s instead of a jumpy half second.
	netSpeedWindow = 1 * time.Second
	// netSpeedMinWindow is the shortest window worth dividing by. Below it the
	// counters' own granularity dominates, so the sampler reports "not ready"
	// and the caller keeps the previous number.
	netSpeedMinWindow = 200 * time.Millisecond
)

// netCounterSample is one reading of an interface's byte counters.
type netCounterSample struct {
	at time.Time
	rx uint64
	tx uint64
}

// netSpeedSampler derives speed from the deltas between collector ticks: every
// call is two sysfs reads that return immediately, so the published rate
// follows whatever cadence the collector runs at. Keeping a short history
// (rather than just the previous sample) means a 2 Hz collector still measures
// over ~1s instead of a jumpy half second, and it needs no priming sleep on the
// first call after start or a WAN failover.
type netSpeedSampler struct {
	mu      sync.Mutex
	iface   string
	history []netCounterSample
}

// wanSpeedSampler tracks the default-route interface for the page-0 speed row.
var wanSpeedSampler netSpeedSampler

// sample reads iface's counters once and returns the speed over the newest
// window of at least netSpeedWindow (or the whole history when it is shorter).
// ok is false when there is nothing sane to divide by yet: the first call, the
// first call after a WAN failover, or right after the counters reset - callers
// should keep showing the previous value rather than a zero or a spike.
//
// While the screen is dark the collector runs once a minute, so the window then
// spans a minute and the row shows that minute's average until the faster
// cadence resumes.
func (s *netSpeedSampler) sample(iface string) (NetworkSpeed, bool, error) {
	rx, tx, err := getInterfaceBytes(iface)
	if err != nil {
		return NetworkSpeed{}, false, err
	}
	speed, ok := s.record(iface, rx, tx, time.Now())
	return speed, ok, nil
}

// record folds one counter reading into the history and returns the speed over
// the resulting window. Split from sample so the windowing can be exercised
// without sysfs or a real clock.
func (s *netSpeedSampler) record(iface string, rx, tx uint64, now time.Time) (NetworkSpeed, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if iface != s.iface {
		// Failover moved the measurement to a different netdev; its counters are
		// unrelated to the ones we have been tracking.
		s.iface = iface
		s.history = s.history[:0]
	}
	if n := len(s.history); n > 0 && (rx < s.history[n-1].rx || tx < s.history[n-1].tx) {
		// Interface bounced and its counters restarted: drop the history rather
		// than turn the negative delta into a huge speed.
		s.history = s.history[:0]
	}
	s.history = append(s.history, netCounterSample{at: now, rx: rx, tx: tx})

	// Keep the newest sample that is old enough to measure against, plus
	// everything after it - a handful of entries at any cadence.
	base := 0
	for i, sample := range s.history {
		if now.Sub(sample.at) >= netSpeedWindow {
			base = i
		}
	}
	s.history = s.history[base:]
	first := s.history[0]

	elapsed := now.Sub(first.at)
	if elapsed < netSpeedMinWindow {
		return NetworkSpeed{}, false
	}
	// Bytes over elapsed seconds → Mbps (bytes * 8 / 1e6 / s).
	secs := elapsed.Seconds()
	return NetworkSpeed{
		DownloadMbps: float64(rx-first.rx) * 8 / 1e6 / secs,
		UploadMbps:   float64(tx-first.tx) * 8 / 1e6 / secs,
	}, true
}

func getSessionDataUsageGB(iface string) (float64, error) {
	stats := []string{"rx_bytes", "tx_bytes"}
	var totalBytes uint64

	for _, stat := range stats {
		// build path: /sys/class/net/<iface>/statistics/<stat>
		path := filepath.Join(pdCovSysClassNetDir, iface, "statistics", stat)

		// read the file
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, fmt.Errorf("failed to read %s: %w", path, err)
		}

		// parse it as uint64
		s := strings.TrimSpace(string(data))
		val, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("failed to parse %s: %w", path, err)
		}

		totalBytes += val
	}

	// convert bytes → MiB
	return float64(totalBytes) / 1024.0 / 1024.0 / 1024.0, nil
}

type vnstatJSON struct {
	Interfaces []struct {
		Name    string `json:"name"`
		Traffic struct {
			// 对应 JSON 中 "traffic":"month":[…]
			Month []struct {
				Date struct {
					Year  int `json:"year"`
					Month int `json:"month"`
				} `json:"date"`
				Rx uint64 `json:"rx"`
				Tx uint64 `json:"tx"`
			} `json:"month"`
		} `json:"traffic"`
	} `json:"interfaces"`
}

// getDataUsageMonthlyGB returns the total (rx+tx) traffic for the current calendar
// month on the given interface, as reported by vnStat, in GiB.
func getDataUsageMonthlyGB(iface string) (float64, error) {
	// 1. 调用 vnstat 获取 JSON
	out, err := secureExecCommand("vnstat", "-i", iface, "--json")
	if err != nil {
		fmt.Printf("failed to run vnstat with default interface: %s, %v", iface, err)
		iface = "wwan0"
		out, err = secureExecCommand("vnstat", "-i", iface, "--json")
		if err != nil {
			fmt.Printf("failed to run vnstat with default interface: %s, %v", iface, err)
			iface = "br-lan"
			out, err = secureExecCommand("vnstat", "-i", iface, "--json")
			if err != nil {
				fmt.Printf("failed to run vnstat with default interface: %s, %v", iface, err)
				return 0, fmt.Errorf("failed to run vnstat with iface: %s, %w", iface, err)
			}
		}
	}

	// 2. 解析 JSON
	var data vnstatJSON
	if err := secureUnmarshal(out, &data); err != nil {
		return 0, fmt.Errorf("failed to parse vnstat JSON: %w", err)
	}

	// 3. 找到对应接口
	var ifaceData *vnstatJSON
	var entryIdx int
	for i, entry := range data.Interfaces {
		if entry.Name == iface {
			ifaceData = &data
			entryIdx = i
			break
		}
	}
	if ifaceData == nil {
		return 0, fmt.Errorf("interface %q not found in vnstat output", iface)
	}

	// 4. 确定当前年/月
	now := time.Now()
	cy, cm := now.Year(), int(now.Month())
	cmStr := fmt.Sprintf("%02d", cm)

	// 5. 在 traffic.month 数组里找当月条目
	for _, m := range data.Interfaces[entryIdx].Traffic.Month {
		if m.Date.Year == cy && m.Date.Month == cm {
			usedBytes := m.Rx + m.Tx
			return float64(usedBytes) / (1 << 30), nil // GiB
		}
	}

	return 0, fmt.Errorf("no data for %04d-%s in vnstat output", cy, cmStr)
}

// CPUStats represents a CPU usage snapshot.
type CPUStats struct {
	User, Nice, System, Idle, Iowait, Irq, Softirq, Steal uint64
}

func readCPUStats() ([]CPUStats, error) {
	data, err := os.ReadFile(pdCovProcStatPath)
	if err != nil {
		return nil, err
	}

	var stats []CPUStats
	lines := strings.Split(string(data), "\n")

	for _, line := range lines {
		if strings.HasPrefix(line, "cpu") && len(line) > 3 && line[3] >= '0' && line[3] <= '9' {
			fields := strings.Fields(line)
			if len(fields) < 8 {
				continue
			}
			var stat CPUStats
			stat.User, _ = strconv.ParseUint(fields[1], 10, 64)
			stat.Nice, _ = strconv.ParseUint(fields[2], 10, 64)
			stat.System, _ = strconv.ParseUint(fields[3], 10, 64)
			stat.Idle, _ = strconv.ParseUint(fields[4], 10, 64)
			stat.Iowait, _ = strconv.ParseUint(fields[5], 10, 64)
			stat.Irq, _ = strconv.ParseUint(fields[6], 10, 64)
			stat.Softirq, _ = strconv.ParseUint(fields[7], 10, 64)
			if len(fields) > 8 {
				stat.Steal, _ = strconv.ParseUint(fields[8], 10, 64)
			}
			stats = append(stats, stat)
		}
	}

	return stats, nil
}

func getCPUUsage() (float64, error) {
	cpus, err := getCpuUsages()
	if err != nil {
		return 0, err
	}
	total := 0.0
	for _, cpu := range cpus {
		total += cpu
	}
	return total / float64(len(cpus)), nil
}

var (
	prevCPUStatsMu sync.Mutex
	prevCPUStats   []CPUStats
)

// getCpuUsages returns per-core usage computed against the snapshot taken on
// the previous call, so the collect interval is the measurement window and no
// call blocks for 500ms sampling (fewer scheduled wakeups on battery). The
// first call primes the snapshot with a short 200ms window.
func getCpuUsages() ([]float64, error) {
	prevCPUStatsMu.Lock()
	defer prevCPUStatsMu.Unlock()

	if prevCPUStats == nil {
		stats, err := readCPUStats()
		if err != nil {
			return nil, err
		}
		prevCPUStats = stats
		time.Sleep(200 * time.Millisecond)
	}

	stats2, err := readCPUStats()
	if err != nil {
		return nil, err
	}
	stats1 := prevCPUStats
	prevCPUStats = stats2

	var usages []float64
	for i := 0; i < len(stats1) && i < len(stats2); i++ {
		idle1 := stats1[i].Idle + stats1[i].Iowait
		idle2 := stats2[i].Idle + stats2[i].Iowait

		nonIdle1 := stats1[i].User + stats1[i].Nice + stats1[i].System +
			stats1[i].Irq + stats1[i].Softirq + stats1[i].Steal

		nonIdle2 := stats2[i].User + stats2[i].Nice + stats2[i].System +
			stats2[i].Irq + stats2[i].Softirq + stats2[i].Steal

		total1 := idle1 + nonIdle1
		total2 := idle2 + nonIdle2

		totalDelta := float64(total2 - total1)
		idleDelta := float64(idle2 - idle1)

		// Back-to-back calls (e.g. a wake refresh right after a tick) can see
		// a zero window; report 0 instead of NaN.
		if totalDelta <= 0 {
			usages = append(usages, 0)
			continue
		}
		cpuPercentage := (totalDelta - idleDelta) / totalDelta * 100
		usages = append(usages, cpuPercentage)
	}

	return usages, nil
}

// pingTimeout is how long a single probe waits for its reply. Anything slower
// than this is reported as a timeout anyway (see the avgRtt check below), so
// waiting longer only delayed the row's next update.
const pingTimeout = 3 * time.Second

// pingICMP uses github.com/go-ping/ping to perform an ICMP ping.
// Note: raw ICMP ping usually requires root privileges.
// Returns -2 for timeouts >3 seconds, -1 for other errors, or ping time in ms.
// The target goes through pingTarget so a transparent proxy's fake-ip answer
// cannot turn a healthy link into a red X (see pingResolve.go).
func pingICMP(host string) (int64, error) {
	pinger, err := ping.NewPinger(pingTarget(host))
	if err != nil {
		return -1, err
	}
	// Set privileged mode if possible; otherwise, false will use UDP.
	pinger.SetPrivileged(true)
	pinger.Count = 1
	pinger.Timeout = pingTimeout

	// Run the ping (blocking).
	err = pinger.Run()
	if err != nil {
		// Check if this is a timeout error
		if strings.Contains(err.Error(), "timeout") {
			return -2, nil // Special value for timeout
		}
		return -1, err
	}
	
	stats := pinger.Statistics()
	if stats.PacketsRecv == 0 {
		return -2, nil // No packets received = timeout
	}
	
	avgRtt := int64(stats.AvgRtt / time.Millisecond)
	
	// If ping took more than 3 seconds, treat as timeout
	if avgRtt > 3000 {
		return -2, nil
	}
	
	// Return average round-trip time in milliseconds.
	return avgRtt, nil
}

// getBatterySoc returns the battery soc from /sys/class/power_supply/battery/capacity.
func getBatterySoc() (int, error) {
	file, err := os.Open(pdCovBatteryDir + "/capacity")
	if err != nil {
		return -1, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return -1, err
	}
	socInt, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil {
		return -1, err
	}
	return socInt, nil
}

// getBatteryCharging returns the battery charging status from /sys/class/power_supply/battery/status.
func getBatteryCharging() (bool, error) {
	var determineChargingByCurrent bool = false
	if determineChargingByCurrent {
		current, err := getBatteryCurrentUA()
		if err != nil {
			return false, err
		}
		return current > 0, nil
	} else {
		file, err := os.Open(pdCovBatteryDir + "/status")
		if err != nil {
			return false, err
		}
		defer file.Close()
		content, err := io.ReadAll(file)
		if err != nil {
			return false, err
		}

		battContent := strings.TrimSpace(string(content))

		if battContent == "Charging" || battContent == "Full" {
			return true, nil
		}
		return false, nil
	}
}

func getBatteryVoltageUV() (float64, error) {
	file, err := os.Open(pdCovBatteryDir + "/voltage_now")
	if err != nil {
		return 0, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(content)), 64)
}

func getBatteryCurrentUA() (float64, error) {
	file, err := os.Open(pdCovBatteryDir + "/current_now")
	if err != nil {
		return 0, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(content)), 64)
}

// readBatterySysfsFloat reads a single numeric value from a
// /sys/class/power_supply/battery/<name> node. Returns an error when the node
// is missing or unparsable so callers can fall back to alternate counters.
func readBatterySysfsFloat(name string) (float64, error) {
	content, err := os.ReadFile(pdCovBatteryDir + "/" + name)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(content)), 64)
}

// chargeTargetSoc is the SoC (%) we count up to when reporting "time to charge".
// The pack is deliberately not driven to 100% on the display estimate, matching
// the home page which reports time-to-90%.
const chargeTargetSoc = 90.0

// batteryEnergyState returns the battery's full capacity, its current level,
// and the instantaneous draw/charge rate — all in a single consistent unit so
// that level/rate yields hours. It probes, in order of accuracy:
//
//  1. energy_now/energy_full (µWh) + power_now (µW)
//  2. charge_now/charge_full (µAh) + current_now (µA)
//  3. capacity(%) × energy_full (µWh) + power_now (µW)   ← photonicat2 layout,
//     which exposes energy_full and power_now but no *_now counter
//  4. capacity(%) × charge_full (µAh) + current_now (µA)
//
// ok is false when no usable pair is found.
func batteryEnergyState() (level, full, rate float64, ok bool) {
	soc, socErr := readBatterySysfsFloat("capacity") // %

	// 1) energy counter with a live energy level.
	if now, err1 := readBatterySysfsFloat("energy_now"); err1 == nil {
		if full, err2 := readBatterySysfsFloat("energy_full"); err2 == nil && full > 0 {
			if rate, err3 := readBatterySysfsFloat("power_now"); err3 == nil {
				return now, full, rate, true
			}
		}
	}
	// 2) charge counter with a live charge level.
	if now, err1 := readBatterySysfsFloat("charge_now"); err1 == nil {
		if full, err2 := readBatterySysfsFloat("charge_full"); err2 == nil && full > 0 {
			if rate, err3 := getBatteryCurrentUA(); err3 == nil {
				return now, full, rate, true
			}
		}
	}
	// 3) derive the energy level from capacity × energy_full (no energy_now node).
	if socErr == nil {
		if full, err2 := readBatterySysfsFloat("energy_full"); err2 == nil && full > 0 {
			if rate, err3 := readBatterySysfsFloat("power_now"); err3 == nil {
				return full * soc / 100.0, full, rate, true
			}
		}
		// 4) derive the charge level from capacity × charge_full.
		if full, err2 := readBatterySysfsFloat("charge_full"); err2 == nil && full > 0 {
			if rate, err3 := getBatteryCurrentUA(); err3 == nil {
				return full * soc / 100.0, full, rate, true
			}
		}
	}
	return 0, 0, 0, false
}

// computeRemainingTimeHours estimates the hours left until the battery reaches
// its target level:
//
//   - charging:    hours until SoC reaches chargeTargetSoc (90%)
//   - discharging: hours until SoC reaches 0%
//
// ok is false when no usable counter/rate is available or the battery is
// effectively idle (so the caller shows a placeholder).
func computeRemainingTimeHours(charging bool) (hours float64, ok bool) {
	level, full, rate, ok := batteryEnergyState()
	if !ok || full <= 0 {
		return 0, false
	}
	// current_now/power_now can be signed; we only need the magnitude and use
	// the charging flag (derived from /status) for direction.
	rate = math.Abs(rate)
	if rate < 1 { // essentially idle: no meaningful estimate
		return 0, false
	}

	var delta float64
	if charging {
		target := full * (chargeTargetSoc / 100.0)
		delta = target - level
		if delta <= 0 { // already at/above target
			return 0, false
		}
	} else {
		delta = level // down to empty
		if delta <= 0 {
			return 0, false
		}
	}

	// delta and rate share units (µWh/µW or µAh/µA) → result in hours.
	return delta / rate, true
}

// normalizeRemainingTime cleans pcat-manager-web's battery_remaining_time for
// the LCD: it trims whitespace and strips a leading "<" (the web prefixes small
// estimates like "< 0:10"), leaving a bare "H:MM". Returns "" for empty input
// and for zero values ("0:00") — a zero estimate means there is nothing to
// show, so the slot stays blank.
func normalizeRemainingTime(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	s = strings.TrimSpace(s)
	// Web sometimes sends a bare "-" placeholder; treat like empty.
	if s == "" || s == "-" {
		return ""
	}
	if zeroTimeRe.MatchString(s) {
		return ""
	}
	return s
}

// zeroTimeRe matches all-zero "H:MM" strings ("0:00", "00:0", ...).
var zeroTimeRe = regexp.MustCompile(`^0+:0+$`)

// formatRemainingTime renders an hours value as "H:MM" (e.g. 2.67h → "2:40").
// A value that rounds to zero minutes returns "" so the slot shows nothing.
func formatRemainingTime(hours float64) string {
	totalMinutes := int(math.Round(hours * 60))
	if totalMinutes == 0 {
		return ""
	}
	h := totalMinutes / 60
	m := totalMinutes % 60
	return fmt.Sprintf("%d:%02d", h, m)
}

// pdCovLocalIPv4Candidates is the LAN-side interface probe order used by
// getLocalIPv4 (a var only so tests can point it at interfaces that exist on
// the test host; the default is unchanged).
var pdCovLocalIPv4Candidates = []string{"eth1", "end1", "end0", "br-lan"}

// getLocalIPv4 returns eth0 IP on OpenWrt or WAN IP (default route) on Debian.
func getLocalIPv4() (string, error) {
	candidates := pdCovLocalIPv4Candidates

	for _, name := range candidates {
		iface, err := net.InterfaceByName(name)
		if err != nil {
			// interface doesn't exist
			continue
		}
		// skip if interface is down
		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				if ip4 := ipnet.IP.To4(); ip4 != nil {
					return ip4.String(), nil
				}
			}
		}
	}

	// none of the candidates had a usable IPv4
	return "LINK DOWN", nil
}

// getWanIPv4 returns the IPv4 address of the interface that currently carries
// a *usable* default route. Does not fall back to UCI network.wan.ipaddr —
// that value is a configured leftover and kept the LCD WAN/public-IP rows
// populated after the cable was unplugged.
func getWanIPv4() (string, error) {
	dev := getDefaultRouteDev()
	if dev == "" {
		return "N/A", fmt.Errorf("no usable default route")
	}
	iface, err := net.InterfaceByName(dev)
	if err == nil {
		addrs, err := iface.Addrs()
		if err == nil {
			for _, addr := range addrs {
				if ipnet, ok := addr.(*net.IPNet); ok {
					if ip4 := ipnet.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
						return ip4.String(), nil
					}
				}
			}
		}
	}
	// Fallback: ask the kernel for the src it would use to a public host.
	// Only trust it when we already have a usable default route (above).
	out, err := secureExecCommand("ip", "route", "get", "1.1.1.1")
	if err != nil {
		return "N/A", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		for i, field := range fields {
			if field == "src" && i+1 < len(fields) {
				if ip := net.ParseIP(fields[i+1]); ip != nil && ip.To4() != nil {
					return fields[i+1], nil
				}
			}
		}
	}
	return "N/A", fmt.Errorf("WAN IP not found")
}

// ipLookupClient mirrors secureHTTPClient but WITHOUT the global User-Agent
// transport, so each public-IP request can carry a per-config User-Agent.
var ipLookupClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: false,
			MinVersion:         tls.VersionTLS12,
		},
		TLSHandshakeTimeout: 5 * time.Second,
	},
}

// Built-in defaults, used when public_ip_lookup carries no sources. These keep
// the historical photonicat.com behaviour for devices with no override.
var defaultPublicIPv4Sources = []PublicIPSource{{URL: "https://4.photonicat.com/ip.php", Parser: "stdout"}}
var defaultPublicIPv6Sources = []PublicIPSource{{URL: "https://6.photonicat.com/ip.php", Parser: "stdout"}}

// fetchOnePublicIP fetches a single source, applies its parser, and validates
// the result as an IP of the requested family.
func fetchOnePublicIP(src PublicIPSource, userAgent string, wantV6 bool) (string, error) {
	req, err := http.NewRequest("GET", src.URL, nil)
	if err != nil {
		return "", err
	}
	ua := strings.TrimSpace(userAgent)
	if ua == "" {
		ua = getUserAgent()
	}
	req.Header.Set("User-Agent", ua)

	resp, err := ipLookupClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Cap the body: IP responses are tiny, JSON ones still small.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}

	parser := strings.TrimSpace(src.Parser)
	if parser == "" {
		parser = "stdout"
	}
	value, err := parseCommandOutput(string(body), parser)
	if err != nil {
		return "", fmt.Errorf("parser %q: %v", parser, err)
	}

	ipStr := strings.TrimSpace(value)
	parsed := net.ParseIP(ipStr)
	if parsed == nil {
		return "", fmt.Errorf("invalid IP received: %q", ipStr)
	}
	if wantV6 {
		if parsed.To4() != nil {
			return "", fmt.Errorf("expected IPv6, got IPv4: %s", ipStr)
		}
	} else if parsed.To4() == nil {
		return "", fmt.Errorf("expected IPv4, got %s", ipStr)
	}
	return ipStr, nil
}

// fetchPublicIP tries each source in order and returns the first valid IP.
func fetchPublicIP(sources []PublicIPSource, userAgent string, wantV6 bool) (string, error) {
	var lastErr error
	for _, src := range sources {
		if strings.TrimSpace(src.URL) == "" {
			continue
		}
		ipStr, err := fetchOnePublicIP(src, userAgent, wantV6)
		if err != nil {
			lastErr = err
			log.Printf("public IP lookup via %s failed: %v", src.URL, err)
			continue
		}
		return ipStr, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no public IP sources configured")
	}
	return "", lastErr
}

// getPublicIPv4 resolves the external IPv4 address from the configured sources
// (falling back to the photonicat.com default when none are configured).
func getPublicIPv4() (string, error) {
	sources := cfg.PublicIPLookup.IPv4
	if len(sources) == 0 {
		sources = defaultPublicIPv4Sources
	}
	return fetchPublicIP(sources, cfg.PublicIPLookup.UserAgent, false)
}

// getIPv6Public resolves the external IPv6 address from the configured sources.
func getIPv6Public() (string, error) {
	sources := cfg.PublicIPLookup.IPv6
	if len(sources) == 0 {
		sources = defaultPublicIPv6Sources
	}
	return fetchPublicIP(sources, cfg.PublicIPLookup.UserAgent, true)
}

// getCpuTemp returns CPU temperature from /sys/class/thermal/thermal_zone0/temp.
func getCpuTemp() (float64, error) {
	file, err := os.Open(pdCovThermalZonePath)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(content)), 64)
}

// getMemUsedAndTotalGB returns used and total memory in GB.
func getMemUsedAndTotalGB() (usedGB float64, totalGB float64, err error) {
	data, err := os.ReadFile(pdCovProcMeminfoPath)
	if err != nil {
		return 0, 0, err
	}

	var memTotal, memAvailable float64

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key, value := fields[0], fields[1]
		switch key {
		case "MemTotal:":
			memTotal, err = strconv.ParseFloat(value, 64)
			if err != nil {
				return 0, 0, err
			}
		case "MemAvailable:":
			memAvailable, err = strconv.ParseFloat(value, 64)
			if err != nil {
				return 0, 0, err
			}
		}
		if memTotal > 0 && memAvailable > 0 {
			break
		}
	}

	if memTotal == 0 {
		return 0, 0, fmt.Errorf("failed to read MemTotal")
	}

	usedKB := memTotal - memAvailable
	usedGB = usedKB / 1024 / 1024
	totalGB = memTotal / 1024 / 1024

	return usedGB, totalGB, nil
}

// getDiskUsage returns disk usage stats (total and free space in MB) for the current partition.
func getDiskUsage() (map[string]interface{}, error) {
	var stat syscall.Statfs_t
	err := syscall.Statfs("/", &stat)
	if err != nil {
		return nil, fmt.Errorf("failed to stat filesystem: %v", err)
	}

	totalMB := (uint64(stat.Bsize) * stat.Blocks) / (1024 * 1024)
	freeMB := (uint64(stat.Bsize) * stat.Bfree) / (1024 * 1024)

	data := map[string]interface{}{
		"Total": totalMB,
		"Free":  freeMB,
		"Used":  totalMB - freeMB,
	}

	return data, nil
}

// reDiskBase extracts the physical disk name from a partition device path,
// e.g. /dev/mmcblk0p7 -> mmcblk0, /dev/nvme0n1p1 -> nvme0n1, /dev/sda1 -> sda.
var reDiskBase = regexp.MustCompile(`^/dev/(mmcblk[0-9]+|nvme[0-9]+n[0-9]+|sd[a-z]+)`)

// parseBlockMounts returns (device, mountpoint) pairs for real block devices
// from /proc/mounts-formatted content.
func parseBlockMounts(content string) [][2]string {
	var mounts [][2]string
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "/dev/") {
			continue
		}
		mounts = append(mounts, [2]string{fields[0], fields[1]})
	}
	return mounts
}

// sdCardDisks returns the removable SD cards currently in the slot as /dev base
// paths (e.g. {"/dev/mmcblk1": true}), read from the MMC type sysfs attribute.
// The soldered eMMC reports "MMC" there, so it never shows up as an SD card —
// on OpenWrt the root filesystem is an overlay with no /dev device in
// /proc/mounts, and without this check its eMMC /boot partition would look like
// a card.
func sdCardDisks() map[string]bool {
	disks := map[string]bool{}
	types, _ := filepath.Glob(pdCovMmcTypeGlob)
	for _, p := range types {
		data, err := os.ReadFile(p)
		if err != nil || strings.TrimSpace(string(data)) != "SD" {
			continue
		}
		// /sys/block/mmcblk1/device/type -> /dev/mmcblk1
		disks["/dev/"+filepath.Base(filepath.Dir(filepath.Dir(p)))] = true
	}
	return disks
}

// pickExtraDiskMounts classifies non-root physical disks and returns the first
// mountpoint of the NVMe drive and of the SD card ("" when absent). An mmcblk
// mount only counts as the SD card when its disk is in sdDisks (see
// sdCardDisks), so eMMC partitions are never mistaken for a card.
func pickExtraDiskMounts(mounts [][2]string, sdDisks map[string]bool) (nvmeMp, sdMp string) {
	rootBase := ""
	for _, m := range mounts {
		if m[1] == "/" {
			rootBase = reDiskBase.FindString(m[0])
			break
		}
	}
	for _, m := range mounts {
		base := reDiskBase.FindString(m[0])
		if base == "" || base == rootBase {
			continue
		}
		if nvmeMp == "" && strings.HasPrefix(base, "/dev/nvme") {
			nvmeMp = m[1]
		} else if sdMp == "" && sdDisks[base] {
			sdMp = m[1]
		}
	}
	return nvmeMp, sdMp
}

// diskStatfs returns used/total GB and used percent (0-100) for mountpoint.
// ok is false when the mount cannot be read or has no size.
func diskStatfs(mountpoint string) (usedGB, totalGB, pct float64, ok bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(mountpoint, &stat); err != nil {
		return 0, 0, 0, false
	}
	totalGB = float64(stat.Blocks) * float64(stat.Bsize) / (1 << 30)
	usedGB = float64(stat.Blocks-stat.Bfree) * float64(stat.Bsize) / (1 << 30)
	if totalGB <= 0 {
		return 0, 0, 0, false
	}
	pct = usedGB / totalGB * 100
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}
	return usedGB, totalGB, pct, true
}

// formatDiskUsageGB returns "used/total" in GB (e.g. "3.1/29") for the
// filesystem at mountpoint, or "-" if it cannot be read. Used space drops the
// decimal at >=100 GB so large NVMe values still fit the display column.
func formatDiskUsageGB(mountpoint string) string {
	usedGB, totalGB, _, ok := diskStatfs(mountpoint)
	if !ok {
		return "-"
	}
	used := fmt.Sprintf("%0.1f", usedGB)
	if usedGB >= 99.95 {
		used = fmt.Sprintf("%0.0f", usedGB)
	}
	return fmt.Sprintf("%s/%d", used, int(math.Ceil(totalGB)))
}

// collectDiskUsage stores per-disk "used/total" GB strings and 0-100 percent
// values for the display:
//   - DiskUsage / DiskUsagePercent — root (onboard) filesystem
//   - DiskNvme / DiskNvmePercent / DiskNvmePresent — first mounted NVMe
//   - DiskSD / DiskSDPercent / DiskSDPresent — SD card in the slot
// Absent disks store "-" / 0 / false so the UI can hide their bar. An SD card
// counts as present as soon as it is in the slot: an unmounted card has no
// usage to report, so it stores "-" / 0 and the UI draws an empty bar.
func collectDiskUsage() {
	globalData.Store("DiskUsage", formatDiskUsageGB("/"))
	if _, _, pct, ok := diskStatfs("/"); ok {
		globalData.Store("DiskUsagePercent", pct)
	} else {
		globalData.Store("DiskUsagePercent", 0.0)
	}

	sdDisks := sdCardDisks()
	nvme, sd := "-", "-"
	nvmePresent := false
	nvmePct, sdPct := 0.0, 0.0
	sdPresent := len(sdDisks) > 0
	if data, err := os.ReadFile(pdCovProcMountsPath); err == nil {
		nvmeMp, sdMp := pickExtraDiskMounts(parseBlockMounts(string(data)), sdDisks)
		if nvmeMp != "" {
			nvme = formatDiskUsageGB(nvmeMp)
			if _, _, pct, ok := diskStatfs(nvmeMp); ok {
				nvmePresent = true
				nvmePct = pct
			}
		}
		if sdMp != "" {
			sd = formatDiskUsageGB(sdMp)
			if _, _, pct, ok := diskStatfs(sdMp); ok {
				sdPresent = true
				sdPct = pct
			}
		}
	}
	globalData.Store("DiskNvme", nvme)
	globalData.Store("DiskNvmePercent", nvmePct)
	globalData.Store("DiskNvmePresent", nvmePresent)
	globalData.Store("DiskSD", sd)
	globalData.Store("DiskSDPercent", sdPct)
	globalData.Store("DiskSDPresent", sdPresent)
}

// networkStats holds RX and TX bytes for an interface.
type networkStats struct {
	rxBytes uint64
	txBytes uint64
}

// readNetworkStats reads current network stats from /proc/net/dev.
func readNetworkStats() (map[string]networkStats, error) {
	file, err := os.Open(pdCovProcNetDevPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open /proc/net/dev: %v", err)
	}
	defer file.Close()

	stats := make(map[string]networkStats)
	scanner := bufio.NewScanner(file)

	// Skip header lines.
	for i := 0; i < 2 && scanner.Scan(); i++ {
	}

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}

		iface := strings.TrimSuffix(fields[0], ":")
		rxBytes, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		txBytes, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}

		stats[iface] = networkStats{
			rxBytes: rxBytes,
			txBytes: txBytes,
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading /proc/net/dev: %v", err)
	}

	return stats, nil
}

// getDHCPClients returns DHCP clients for OpenWRT.
func getDHCPClients() ([]string, error) {
	// Try OpenWRT method first - read DHCP lease file
	if clients, err := getOpenWrtDHCPClients(); err == nil {
		return clients, nil
	}

	// Fallback for Debian/other systems
	return getDebianDHCPClients()
}

// getWifiClients returns WiFi client MAC addresses for OpenWRT.
func getWifiClients() (string, error) {
	// Try OpenWRT method first
	if clients, err := getOpenWrtWifiClients(); err == nil {
		return clients, nil
	}

	// Fallback for Debian/other systems
	return getDebianWifiClients()
}

// getOpenWrtDHCPClients reads DHCP clients from OpenWRT lease file
func getOpenWrtDHCPClients() ([]string, error) {
	// OpenWRT typically stores DHCP leases in /tmp/dhcp.leases
	out, err := secureExecCommand("cat", "/tmp/dhcp.leases")
	if err != nil {
		return nil, fmt.Errorf("failed to read DHCP leases: %v", err)
	}

	var clients []string
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// DHCP lease format: timestamp mac ip hostname client_id
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			ip := fields[2]
			if ip != "*" && ip != "" {
				clients = append(clients, ip)
			}
		}
	}

	return clients, nil
}

// getOpenWrtWifiClients gets WiFi clients using OpenWRT's iwinfo
func getOpenWrtWifiClients() (string, error) {
	// WiFi interfaces to check (up to 3 max)
	interfaces := []string{
		"wlan0", "wlan1", "wlan2",  // Standard wlan interfaces
		"radio0", "radio1", "radio2", // Radio interfaces
	}
	
	var allMacs []string
	
	// Try each interface
	for _, iface := range interfaces {
		out, err := secureExecCommand("iwinfo", iface, "assoclist")
		if err != nil {
			// Interface doesn't exist or no clients, continue to next
			continue
		}
		
		// Parse iwinfo output to extract MAC addresses
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			// Look for MAC addresses in format XX:XX:XX:XX:XX:XX
			if len(line) >= 17 && strings.Count(line, ":") >= 5 {
				fields := strings.Fields(line)
				if len(fields) > 0 {
					mac := fields[0]
					if strings.Count(mac, ":") == 5 && len(mac) == 17 {
						// Avoid duplicates
						found := false
						for _, existingMac := range allMacs {
							if existingMac == mac {
								found = true
								break
							}
						}
						if !found {
							allMacs = append(allMacs, mac)
						}
					}
				}
			}
		}
	}
	
	if len(allMacs) == 0 {
		return "", fmt.Errorf("no WiFi clients found on any interface")
	}

	return strings.Join(allMacs, ","), nil
}

// getDebianDHCPClients fallback for Debian systems
func getDebianDHCPClients() ([]string, error) {
	// Try to read from common DHCP lease locations
	leaseFiles := []string{
		"/var/lib/dhcp/dhcpd.leases",
		"/var/lib/dhcpcd5/dhcpcd.leases",
	}

	for _, file := range leaseFiles {
		if out, err := secureExecCommand("cat", file); err == nil {
			var clients []string
			lines := strings.Split(string(out), "\n")
			for _, line := range lines {
				if strings.Contains(line, "lease ") && strings.Contains(line, "{") {
					// Extract IP from lease line
					parts := strings.Fields(line)
					if len(parts) >= 2 {
						ip := strings.TrimSpace(parts[1])
						if ip != "" {
							clients = append(clients, ip)
						}
					}
				}
			}
			if len(clients) > 0 {
				return clients, nil
			}
		}
	}

	// Fallback to dummy data
	return []string{"192.168.1.100", "192.168.1.101"}, nil
}

// getDebianWifiClients fallback for Debian systems
func getDebianWifiClients() (string, error) {
	// Try iwconfig first
	if out, err := secureExecCommand("iwconfig"); err == nil {
		// Parse iwconfig output for connected clients (limited info)
		if strings.Contains(string(out), "Access Point:") {
			return "DEBIAN_WIFI_CLIENT", nil
		}
	}

	// Try nmcli if available
	if out, err := secureExecCommand("nmcli", "device", "wifi"); err == nil {
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			if strings.Contains(line, "*") {
				return "NMCLI_CONNECTED", nil
			}
		}
	}

	// Fallback to dummy data
	return "DEBIAN_FALLBACK", nil
}

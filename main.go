package main

import (
	_ "embed"
	"flag"
	"fmt"
	"image"
	"image/color"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	gc9307 "github.com/photonicat/periph.io-gc9307"

	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/conn/v3/physic"
	"periph.io/x/conn/v3/spi"
	"periph.io/x/conn/v3/spi/spireg"
	"periph.io/x/host/v3"
)

// Package default layouts ship inside the binary so every firmware update
// carries the current config.json / config_debian.json. On startup they are
// written to /etc (overwriting any stale system default) and then
// user_config.json is merged on top. pcat-manager-web does not hold a copy —
// it always fetches defaults from this process via HTTP.
//
//go:embed config.json
var embeddedOpenWrtConfigJSON []byte

//go:embed config_debian.json
var embeddedDebianConfigJSON []byte

const (
	RST_PIN              = "GPIO122"
	DC_PIN               = "GPIO121"
	CS_PIN               = "GPIO13"
	BL_PIN               = "GPIO13" //now we are using pwm control backlight
	PCAT2_LCD_WIDTH      = 172
	PCAT2_LCD_HEIGHT     = 320
	PCAT2_X_OFFSET       = 34
	PCAT2_L_MARGIN       = 8
	PCAT2_R_MARGIN       = 7
	PCAT2_T_MARGIN       = 10
	PCAT2_B_MARGIN       = 7
	PCAT2_TOP_BAR_HEIGHT = 32
	PCAT2_FOOTER_HEIGHT  = 22

	STATE_UNKNOWN  = -1
	STATE_IDLE     = 0
	STATE_ACTIVE   = 1
	STATE_FADE_IN  = 2
	STATE_FADE_OUT = 3
	STATE_OFF      = 4

	DEFAULT_FPS               = 3
	// SCROLL_FPS is the frame rate used only while a text value is actively
	// scrolling as a ticker (long SSID/IP/etc.). It is high enough for smooth
	// motion; the loop drops back to DEFAULT_FPS the moment nothing overflows,
	// so idle power draw is unchanged.
	SCROLL_FPS                = 30
	DEFAULT_IDLE_TIMEOUT      = 60 * time.Second
	ON_CHARGING_IDLE_TIMEOUT  = 365 * 86400 * time.Second
	KEYBOARD_DEBOUNCE_TIME    = 40 * time.Millisecond
	ZERO_BACKLIGHT_DELAY      = 5 * time.Second
	OFF_TIMEOUT               = 3 * time.Second
	// Dynamic data (SMS, and anything not shown on the current page) refreshes
	// once a minute whether the screen is on or idle-dark, so wake shows current
	// values without a burst of collectors.
	INTERVAL_DATA_COLLECT = 60 * time.Second
	// Ping is page-sensitive: when the screen is awake and the current page
	// shows Ping0/Ping1, poll at 1 Hz; otherwise the same 1-minute cadence.
	INTERVAL_PING_ACTIVE = 1 * time.Second
	INTERVAL_PING_IDLE   = INTERVAL_DATA_COLLECT
	// Linux stats (CPU bars, mem, temps…) run at 2 Hz (0.5s) while the
	// CPU-bars page is visible and the screen is awake; otherwise 1 minute.
	INTERVAL_LINUX_ACTIVE = 500 * time.Millisecond
	INTERVAL_LINUX_IDLE   = INTERVAL_DATA_COLLECT
	// Page 0 (WAN speed, data usage, battery, DC voltage) refreshes at 2 Hz
	// (0.5s) while page 0 is visible and the screen is awake; otherwise 1
	// minute. This drives the battery, network, pcat-web and WAN-speed
	// collectors together so every value on page 0 stays live.
	INTERVAL_PAGE0_ACTIVE = 500 * time.Millisecond
	INTERVAL_PAGE0_IDLE   = INTERVAL_DATA_COLLECT
	INTERVAL_SMS_COLLECT  = INTERVAL_DATA_COLLECT
	// Link status (carrier-aware egress + public-IP flush/reprobe): fast while
	// the screen is awake so unplug/plug updates the top-bar icon and PUBLIC
	// IP row within a couple of seconds on any page; relaxed when dark.
	INTERVAL_LINK_ACTIVE = 2 * time.Second
	INTERVAL_LINK_IDLE   = 30 * time.Second

	ETC_USER_CONFIG_PATH   = "/etc/pcat2_mini_display-user_config.json"
	ETC_CONFIG_PATH        = "/etc/pcat2_mini_display-config.json"
	ETC_DEBIAN_CONFIG_PATH = "/etc/pcat2_mini_display-config_debian.json"

	// Written on every start; /tmp is a tmpfs cleared at boot, so its absence
	// means no instance has run since power-on (see the welcome gate below).
	WELCOME_MARKER_PATH = "/tmp/pcat2_mini_display.welcomed"
)

var (
	// Build info, injected via -ldflags "-X main.version=... -X main.buildTime=..."
	version   = "dev"
	buildTime = "unknown"

	runMainLoop = true
	offTime     = time.Now()

	// weAreRunning is the process-wide shutdown flag: the SIGTERM handler
	// clears it and every background collector loop polls it to wind down.
	// It is therefore written and read from different goroutines and must be
	// atomic — as a plain bool it was a data race that `make test-race`
	// reported. Use setWeAreRunning/weAreRunning rather than touching it.
	weAreRunningFlag atomic.Bool

	// Shutdown monitoring
	//shutdownMonitor *ShutdownMonitor
	PCAT_YELLOW = color.RGBA{255, 229, 0, 255}
	PCAT_WHITE  = color.RGBA{255, 255, 255, 255}
	PCAT_RED    = color.RGBA{226, 72, 38, 255}
	PCAT_GREY   = color.RGBA{98, 116, 130, 255}
	PCAT_GREEN  = color.RGBA{70, 235, 145, 255}
	PCAT_BLACK  = color.RGBA{0, 0, 0, 255}

	svgCache = make(map[string]*image.RGBA)

	wanInterface = "null"

	// SPI DMA mode control
	dmaMode              = true
	spiTransferOptimized = true

	dirtyTracker *DirtyRegionTracker
	renderCache  *RenderCache

	frameMutex sync.RWMutex
	// Optimized buffer manager
	bufferManager   *BufferManager
	frames          int
	dataMutex       sync.RWMutex
	dynamicData     map[string]string
	imageCache      map[string]*image.RGBA
	imageCacheMu    sync.RWMutex
	cfg             Config
	dftCfg          Config
	userCfg         Config
	currPageIdx     int
	fonts              map[string]FontConfig
	assetsPrefix       = "."
	globalData         sync.Map
	autoRotatePages    = false
	customMetricsMgr   *CustomMetricManager

	// Frame buffer pool is now managed by BufferManager

	lastActivity   = time.Now()
	lastActivityMu sync.Mutex

	numIntermediatePages = 15

	// configuration for idle fade
	fadeDuration = 2 * time.Second // how long the fade takes
	fadeInDur    = 300 * time.Millisecond
	maxBacklight = 100
	idleTimeout  = DEFAULT_IDLE_TIMEOUT

	idleState          = STATE_ACTIVE
	lastChargingStatus = false
	battChargingStatus = false
	battSOC            = 0

	// Collection intervals. SMS stays at 1 minute; ping, linux stats and page-0
	// data change with page/idle state (see currentPingInterval /
	// currentLinuxInterval / currentPage0Interval). These runtime vars just
	// mirror the live cadence for the HTTP status page and diagnostics.
	baseSmsInterval = INTERVAL_SMS_COLLECT

	batteryDataInterval   = INTERVAL_DATA_COLLECT
	dataGatherInterval    = INTERVAL_DATA_COLLECT
	networkGatherInterval = INTERVAL_DATA_COLLECT
	pingGatherInterval    = INTERVAL_PING_IDLE

	// Idle FPS throttle only (data intervals no longer scale with idle).
	// When fully dark (displayAsleep) the render loop skips SPI entirely.
	idleMultiplier = 10
	// Reschedule channels wake page-sensitive collectors so entering the
	// matching page switches cadence without waiting out a leftover 1m sleep.
	// Page 0's fast cadence drives several collectors at once, so its wake is a
	// slice of channels — one per collector — signalled together.
	// Ping has one channel per row (Ping0/Ping1) because each row is collected
	// by its own goroutine; they are signalled together.
	pingReschedule = []chan struct{}{
		make(chan struct{}, 1), // Ping0
		make(chan struct{}, 1), // Ping1
	}
	linuxReschedule = make(chan struct{}, 1)
	page0Reschedule = []chan struct{}{
		make(chan struct{}, 1), // battery
		make(chan struct{}, 1), // network
		make(chan struct{}, 1), // pcat-web
		make(chan struct{}, 1), // WAN speed
	}
	linkReschedule = make(chan struct{}, 1)

	baseFPS    = DEFAULT_FPS
	desiredFPS = DEFAULT_FPS

	// forceTopBarRedraw is set on wake so the first active frame redraws
	// clock/battery instead of waiting for the every-15-frames throttle.
	forceTopBarRedraw = false
	// lastTopBarRefresh drives the once-per-minute clock update even while
	// the screen is dark (displayAsleep skips full renders).
	lastTopBarRefresh = time.Time{}

	lastBrightness = -1

	mu          sync.Mutex
	lastLogical int         // last requested brightness (0–100)
	offTimer    *time.Timer // timer that will write 0 after delay

	smsPagesImages []*image.RGBA

	httpChangePageTriggered = false
	changePageTriggered     = false
	lastButtonPress         = time.Time{}
	buttonDebounceDelay     = 40 * time.Millisecond
	buttonPressInProgress   = false
	// Signal channel for interrupting FPS sleep on page changes
	pageChangeSignal = make(chan struct{}, 1)
	// Button timing tracking
	buttonKeydownTime = time.Time{}
	buttonKeyupTime   = time.Time{}
	stitchStartTime   = time.Time{}
	// Pre-calculation optimization variables
	isPreCalculating          = false
	preCalculatedReady        = false
	preCalculatedStitched     *image.RGBA
	preCalculatedNextIdx      = 0
	preCalculatedIsSMS        = false
	preCalculatedIsNextSMS    = false
	preCalculatedLocalIdx     = 0
	preCalculatedNextLocalIdx = 0
	preCalculationMutex       sync.RWMutex

	// Pre-allocated transition frame buffers
	transitionFrames          []*image.RGBA
	transitionFramesReady     = false
	transitionCalculating     = false
	transitionMutex           sync.RWMutex
	transitionFrameChannel    = make(chan int, numIntermediatePages)
	transitionCompleteChannel = make(chan bool, 1)
	// nextPageIdxFrameBuffer is now managed by BufferManager
	showFPS                = false
	showDetailedTiming     = true // Toggle for detailed timing output
	fps                    = 0.0
	lastUpdate             = time.Now()
	totalFrames            = 0
	stitchedFrames         = 0
	localConfigExists      = false
	stitchedFrame          *image.RGBA
	totalNumPages          = -1
	middleFrames           = 0
	topFrames              = 0
	nextPageIdxFrameBuffer *image.RGBA
	croppedFrameBuffer     *image.RGBA

	// Performance optimization
	easingLookup  []int
	cachedFPSText string
	lastFPSUpdate time.Time

	topBarFrameWidth  = PCAT2_LCD_WIDTH
	topBarFrameHeight = PCAT2_TOP_BAR_HEIGHT

	middleFrameWidth  = PCAT2_LCD_WIDTH
	middleFrameHeight = PCAT2_LCD_HEIGHT - PCAT2_TOP_BAR_HEIGHT - PCAT2_FOOTER_HEIGHT

	footerFrameWidth  = PCAT2_LCD_WIDTH
	footerFrameHeight = PCAT2_FOOTER_HEIGHT

	// Double buffering framebuffers
	topBarFramebuffers [2]*image.RGBA
	middleFramebuffers [2]*image.RGBA
	footerFramebuffers [2]*image.RGBA

	lenSmsPagesImages = 1
	display           gc9307.Device
	displayWrapper    *DisplayWrapper

	cfgNumPages = 0

	// Ping rows and their success counters live in processData.go (pingRow).
)

// ImageBuffer holds a 1D slice of pixels for the display area.
type ImageBuffer struct {
	buffer []color.RGBA
	width  int
	height int
	loaded bool
}

func init() { weAreRunningFlag.Store(true) }

// weAreRunning reports whether the daemon is still meant to be running.
func weAreRunning() bool { return weAreRunningFlag.Load() }

// setWeAreRunning flips the shutdown flag. Clearing it asks every collector
// loop and the main render loop to return at their next check.
func setWeAreRunning(v bool) { weAreRunningFlag.Store(v) }

// GetFrameBuffer retrieves a frame buffer from the pool
func GetFrameBuffer(width, height int) *image.RGBA {
	if bufferManager == nil {
		return image.NewRGBA(image.Rect(0, 0, width, height))
	}
	return bufferManager.GetFrameFromPool(width, height)
}

// ReturnFrameBuffer returns a frame buffer to the pool
func ReturnFrameBuffer(buf *image.RGBA) {
	if bufferManager != nil && buf != nil {
		bufferManager.ReturnFrameToPool(buf)
	}
}

// buildFontTable returns the name→font mapping the display templates address
// by their "font" field, rooted at the given assets prefix.
//
// It is a function rather than a literal inside main() so tests can build the
// same table against a local assets/ dir: a test that copied the literal would
// silently miss any font added later, and an element asking for the missing
// name renders as nothing at all rather than failing loudly.
func buildFontTable(prefix string) map[string]FontConfig {
	orbitron := func(weight string, size float64) FontConfig {
		return FontConfig{
			FontPath: prefix + "/assets/fonts/Orbitron-" + weight + ".ttf",
			FontSize: size,
		}
	}
	return map[string]FontConfig{
		"clock":     orbitron("Medium", 20),
		"clockBold": orbitron("ExtraBold", 17),
		"reg":       orbitron("ExtraBold", 18),
		"big":       orbitron("ExtraBold", 25),
		"unit":      orbitron("Medium", 15),
		"tiny":      orbitron("Regular", 12),
		"micro":     orbitron("Regular", 10),
		"thin":      orbitron("Regular", 18),
		"huge":      orbitron("ExtraBold", 34),
		"gigantic":  orbitron("ExtraBold", 48),
		// One step above gigantic, for the GPS page's speed readout. Sized so
		// three digits ("188") still fit the 172px panel now that the km/h
		// unit is gone from the layout.
		"colossal": orbitron("ExtraBold", 62),
		// Chinese font variants
		"unit_cjk": {FontPath: prefix + "/assets/fonts/NotoSansMonoCJK-VF.ttf.ttc", FontSize: 15},
	}
}

// Position defines X and Y coordinates.
type Position struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// Size defines width and height.
type Size struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// DisplayElement represents one UI element to render.
type DisplayElement struct {
	Type        string       `json:"type"`
	Label       string       `json:"label"`
	Position    Position     `json:"position"`
	Font        string       `json:"font,omitempty"`
	Color       []int        `json:"color,omitempty"`
	Units       string       `json:"units,omitempty"`
	DataKey     string       `json:"data_key,omitempty"`
	// LabelDataKey, when set on an hbar, loads a string (e.g. MemUsage "3.2/16")
	// drawn centered over the bar; Units is appended if present ("GB" → "3.2/16GB").
	LabelDataKey string       `json:"label_data_key,omitempty"`
	UnitsFont    string       `json:"units_font,omitempty"`
	// UnitsDy nudges the units text vertically off wherever it would otherwise
	// land (negative = up, positive = down). A fine-tuning knob; for stacking
	// the unit under the value use UnitsBelow.
	UnitsDy      int          `json:"units_dy,omitempty"`
	// UnitsBelow, when > 0, moves the units onto their own line under the
	// value's left edge, with exactly this many pixels of clear space between
	// the two inks. Trailing units share the value's baseline 1px past its
	// advance width, which reads well at body sizes but not at display sizes:
	// against the GPS page's 62px speed digits the 12px "km/h" ends up nested
	// in the digits' bottom-right corner, and the panel has no spare width to
	// push it further right. 0 (the default) keeps the trailing placement.
	UnitsBelow   int          `json:"units_below,omitempty"`
	IconPath     string       `json:"icon_path,omitempty"`
	Enable       int          `json:"enable,omitempty"`
	Size         *Size        `json:"size,omitempty"`         // for icons, if provided
	Size2        *Size        `json:"_size,omitempty"`        // sometimes provided as _size
	GraphConfig  *GraphConfig `json:"graph_config,omitempty"` // for graph elements
	// AnchorAfterDataKey, when set on an icon, positions the icon immediately to
	// the right of the named text data_key's rendered value (Position.X is then
	// the text's start x, and the icon is offset by the measured text width plus
	// AnchorGap). This lets a trailing icon "stick" to variable-width text like
	// the battery remaining-time ("0:10" vs "12:30") instead of a fixed x.
	AnchorAfterDataKey string `json:"anchor_after_data_key,omitempty"`
	AnchorFont         string `json:"anchor_font,omitempty"` // font of the anchored text (defaults to "reg")
	AnchorGap          int    `json:"anchor_gap,omitempty"`  // extra px between text and icon
	// Align, when "center" on a text element, treats Position.X as the
	// horizontal center of the rendered value instead of its left edge
	// (e.g. the GPS speed centered on the panel regardless of digit count).
	Align string `json:"align,omitempty"`
}

// GraphConfig holds configuration for graph elements
type GraphConfig struct {
	GraphType     string `json:"graph_type"`      // e.g., "power"
	TimeFrameMins int    `json:"time_frame_mins"` // time frame in minutes
}

// DisplayTemplate holds pages of elements.
type DisplayTemplate struct {
	Elements map[string][]DisplayElement `json:"elements"`
	// BgColor is an optional [R,G,B] background for the whole screen. When
	// absent or invalid the background stays black.
	BgColor []int `json:"bg_color,omitempty"`
}

// Config represents the overall config JSON.
type Config struct {
	ScreenDimmerTimeOnBatterySeconds int                 `json:"screen_dimmer_time_on_battery_seconds"`
	ScreenDimmerTimeOnDCSeconds      int                 `json:"screen_dimmer_time_on_dc_seconds"`
	ScreenMaxBrightness              int                 `json:"screen_max_brightness"`
	ScreenMinBrightness              int                 `json:"screen_min_brightness"`
	PingSite0                        string              `json:"ping_site0"`
	PingSite1                        string              `json:"ping_site1"`
	// PingType0/1 select each row's probe: "icmp" (default), "tcp" (one TCP
	// handshake to host[:port], port 443 when omitted), "http" or "https"
	// (one GET, gen_204 style — any HTTP response counts; https times the
	// TLS handshake too). See pingModes.go.
	PingType0 string `json:"ping_type0,omitempty"`
	PingType1 string `json:"ping_type1,omitempty"`
	// ConfigVersion is the schema/feature version of the SHIPPED config.json;
	// it is bumped whenever a release adds config-driven features so the web
	// UI (and support) can tell what this firmware understands. mergeConfigs
	// forces the shipped value, so a stale copy in user_config never wins.
	// History: 100 = pre-versioned scheme, 101 = ping_type0/1 (tcp/http
	// ping), 102 = "https" ping type, 103 = show_wifi_qr (Wi-Fi QR pages).
	ConfigVersion int `json:"config_version,omitempty"`
	DisplayTemplate                  DisplayTemplate     `json:"display_template"`
	ShowSms                          bool                `json:"show_sms"`
	// ShowWifiQr shows one "scan to join" QR page per access point. A pointer
	// so "absent" (every user_config written before this existed) stays
	// distinct from false and inherits the shipped default, which is on.
	ShowWifiQr *bool `json:"show_wifi_qr,omitempty"`
	SmsLimitForScreen                int                 `json:"sms_limit_for_screen"`
	CustomMetrics                    CustomMetricsConfig `json:"custom_metrics,omitempty"`
	PublicIPLookup                   PublicIPLookup      `json:"public_ip_lookup,omitempty"`
	// ColorTheme is the name of the color theme the web UI last applied. The
	// firmware only round-trips it so the UI can highlight the active theme.
	ColorTheme string `json:"color_theme,omitempty"`
}

// PublicIPSource is one configurable endpoint used to resolve the public IP.
// Parser reuses the custom-metrics parser syntax: "stdout" (default, the whole
// trimmed body), "json:path.to.key", "regex:pattern", or "line:N". This lets
// users point at services that return plain text (icanhazip.com, ident.me) or
// JSON (ip-api.com) when the default photonicat.com endpoints are blocked.
type PublicIPSource struct {
	URL    string `json:"url"`
	Parser string `json:"parser,omitempty"`
}

// PublicIPLookup configures how the LCD's PUBLIC IP field is resolved. Sources
// are tried in order and the first one that returns a valid IP of the right
// family wins, so listing several gives automatic fallback. UserAgent, when
// non-empty, overrides the default request User-Agent (some services vary their
// output by client).
type PublicIPLookup struct {
	UserAgent string           `json:"user_agent,omitempty"`
	IPv4      []PublicIPSource `json:"ipv4,omitempty"`
	IPv6      []PublicIPSource `json:"ipv6,omitempty"`
}

// FontConfig holds parameters for a font.
type FontConfig struct {
	FontPath string  // path to TTF file
	FontSize float64 // in points
}

// checkDMAAvailability checks if SPI DMA is usable for display output.
//
// This is a *display* driver: it only ever writes pixels out to the panel, so
// only the TX DMA channel matters. RX is never used, and on this platform the
// SPI node is often given a TX-only DMA channel by the device tree. We
// therefore require only dma:tx and treat dma:rx as optional (logged if
// present, ignored if absent).
func checkDMAAvailability() error {
	return checkDMAAvailabilityAt("/sys/devices/platform/soc/2ad00000.spi")
}

// checkDMAAvailabilityAt is the testable core of checkDMAAvailability. It
// inspects the given SPI sysfs directory for DMA channel symlinks. Only the TX
// channel is required (see checkDMAAvailability for why).
func checkDMAAvailabilityAt(spiDir string) error {
	dmaRxPath := spiDir + "/dma:rx"
	dmaTxPath := spiDir + "/dma:tx"

	if _, err := os.Stat(dmaTxPath); os.IsNotExist(err) {
		return fmt.Errorf("DMA TX channel not found at %s", dmaTxPath)
	}

	if _, err := os.Stat(dmaRxPath); os.IsNotExist(err) {
		log.Printf("DMA TX channel found (RX absent, not needed for display output): TX=%s", dmaTxPath)
	} else {
		log.Printf("DMA channels found: RX=%s, TX=%s", dmaRxPath, dmaTxPath)
	}
	return nil
}

// SPIConfig holds SPI configuration based on DMA mode
type SPIConfig struct {
	MaxTransferSize int
	UseChunking     bool
	ChunkSize       int
	BufferStrategy  string
}

// getSPIConfig returns optimized SPI configuration based on DMA availability
func getSPIConfig() SPIConfig {
	if spiTransferOptimized {
		return SPIConfig{
			MaxTransferSize: 65536, // 64KB for DMA transfers
			UseChunking:     false,
			ChunkSize:       0,
			BufferStrategy:  "dma_optimized",
		}
	} else {
		return SPIConfig{
			MaxTransferSize: 4096, // 4KB for non-DMA transfers
			UseChunking:     true,
			ChunkSize:       1024, // 1KB chunks
			BufferStrategy:  "interrupt_driven",
		}
	}
}

// logSPIMode logs the current SPI transfer mode
func logSPIMode() {
	config := getSPIConfig()
	log.Printf("SPI Transfer Mode: %s, Max Transfer: %d bytes, Chunking: %v",
		config.BufferStrategy, config.MaxTransferSize, config.UseChunking)
	if config.UseChunking {
		log.Printf("Chunk Size: %d bytes", config.ChunkSize)
	}
}

// DisplayWrapper provides optimized display operations based on DMA mode
type DisplayWrapper struct {
	device gc9307.Device
	config SPIConfig
}

// NewDisplayWrapper creates a new display wrapper with DMA optimization
func NewDisplayWrapper(device gc9307.Device) *DisplayWrapper {
	return &DisplayWrapper{
		device: device,
		config: getSPIConfig(),
	}
}

// FillRectangleWithImageOptimized optimizes image transfers based on DMA availability
func (dw *DisplayWrapper) FillRectangleWithImageOptimized(x, y, width, height int16, img *image.RGBA) {
	if dw.config.UseChunking {
		// Non-DMA mode: use smaller chunks to avoid blocking
		dw.fillRectangleChunked(x, y, width, height, img)
	} else {
		// DMA mode: send larger buffers for optimal DMA utilization
		dw.device.FillRectangleWithImage(x, y, width, height, img)
	}
}

// fillRectangleChunked breaks large transfers into smaller chunks for non-DMA mode
func (dw *DisplayWrapper) fillRectangleChunked(x, y, width, height int16, img *image.RGBA) {
	// For non-DMA mode, we can break the image into horizontal strips
	// This reduces the amount of data in each SPI transaction
	chunkHeight := int16(dw.config.ChunkSize / (int(width) * 3)) // 3 bytes per pixel (RGB)
	if chunkHeight < 1 {
		chunkHeight = 1
	}
	if chunkHeight > height {
		chunkHeight = height
	}

	// The display driver expects a zero-based, tightly-packed image whose
	// Bounds() exactly match the (width, chunkHeight) it is told to draw. A
	// SubImage does NOT satisfy that: it keeps the parent's Stride and a
	// non-zero Rect.Min, and the driver's linear pixel walk then indexes past
	// the end of the shared Pix slice (index-out-of-range panic). So copy each
	// strip into a fresh zero-based buffer, row by row, honoring the source
	// stride and origin.
	src := img.Pix
	srcStride := img.Stride
	srcBounds := img.Bounds()
	rowBytes := int(width) * 4 // RGBA

	for currentY := y; currentY < y+height; currentY += chunkHeight {
		remainingHeight := y + height - currentY
		if remainingHeight < chunkHeight {
			chunkHeight = remainingHeight
		}

		// Fresh zero-based strip the driver can walk safely.
		chunkImg := image.NewRGBA(image.Rect(0, 0, int(width), int(chunkHeight)))
		for row := 0; row < int(chunkHeight); row++ {
			// Source row within the original image, offset by this strip's start.
			srcY := srcBounds.Min.Y + int(currentY-y) + row
			srcOff := srcY*srcStride + srcBounds.Min.X*4
			dstOff := row * chunkImg.Stride
			if srcOff < 0 || srcOff+rowBytes > len(src) {
				continue // stay in bounds even if the caller passed a short buffer
			}
			copy(chunkImg.Pix[dstOff:dstOff+rowBytes], src[srcOff:srcOff+rowBytes])
		}

		// Send this chunk
		dw.device.FillRectangleWithImage(x, currentY, width, chunkHeight, chunkImg)
	}
}

// GetTransferStats returns statistics about the current transfer mode
func (dw *DisplayWrapper) GetTransferStats() map[string]interface{} {
	return map[string]interface{}{
		"dma_enabled":       dmaMode,
		"transfer_strategy": dw.config.BufferStrategy,
		"max_transfer_size": dw.config.MaxTransferSize,
		"use_chunking":      dw.config.UseChunking,
		"chunk_size":        dw.config.ChunkSize,
	}
}

// updateIntervals applies idle-state changes that still depend on it (FPS
// throttle + page-sensitive cadences). General data collectors stay at 1
// minute always.
func updateIntervals() {
	if idleState == STATE_IDLE {
		// Very low FPS while idle-but-visible (min brightness > 0). When
		// fully dark the main loop skips render entirely.
		desiredFPS = 1
	} else {
		desiredFPS = baseFPS
	}
	batteryDataInterval = currentPage0Interval()
	networkGatherInterval = currentPage0Interval()
	dataGatherInterval = currentLinuxInterval()
	pingGatherInterval = currentPingInterval()
	signalPingReschedule()
	signalLinuxReschedule()
	// Idle<->active flips page 0's cadence between 2 Hz and 1 minute; wake the
	// page-0 collectors so the change takes effect immediately.
	signalPage0Reschedule()
	// Link watcher also accelerates on wake so the top-bar icon is current.
	signalLinkReschedule()
	signalGpsReschedule()
}

// currentPingInterval returns 1s (1 Hz) when the screen is awake and the
// current page shows ping metrics; otherwise 1 minute (idle or other pages).
func currentPingInterval() time.Duration {
	if idleState != STATE_IDLE && pageShowsPing(currPageIdx) {
		return INTERVAL_PING_ACTIVE
	}
	return INTERVAL_PING_IDLE
}

// currentLinuxInterval returns 500ms (2 Hz) when the screen is awake and the
// current page shows cpu_bars; otherwise 1 minute.
func currentLinuxInterval() time.Duration {
	if idleState != STATE_IDLE && pageShowsCpuBars(currPageIdx) {
		return INTERVAL_LINUX_ACTIVE
	}
	return INTERVAL_LINUX_IDLE
}

// currentPage0Interval returns 500ms (2 Hz) when the screen is awake and page 0
// is the visible page; otherwise 1 minute. Page 0 shows WAN speed, data usage,
// battery and DC voltage, so its collectors (battery/network/pcat-web/WAN
// speed) share this cadence. currPageIdx spans cfg pages then SMS pages, so
// index 0 is unambiguously cfg page 0 (SMS pages have index >= cfgNumPages).
func currentPage0Interval() time.Duration {
	if idleState != STATE_IDLE && currPageIdx == 0 {
		return INTERVAL_PAGE0_ACTIVE
	}
	return INTERVAL_PAGE0_IDLE
}

// currentLinkInterval is 2s while the screen is awake (any page) so cable
// plug/unplug updates ActiveEgress + PUBLIC_IP promptly; 30s while dark.
func currentLinkInterval() time.Duration {
	if idleState != STATE_IDLE {
		return INTERVAL_LINK_ACTIVE
	}
	return INTERVAL_LINK_IDLE
}

// isPage0 reports whether the given combined page index is cfg page 0.
func isPage0(pageIdx int) bool {
	return pageIdx == 0
}

// pageShowsPing reports whether the given page index (cfg page, not SMS)
// has any element bound to Ping0 or Ping1.
func pageShowsPing(pageIdx int) bool {
	return pageHasMatch(pageIdx, func(el DisplayElement) bool {
		return el.DataKey == "Ping0" || el.DataKey == "Ping1"
	})
}

// pageShowsCpuBars reports whether the given page has a cpu_bars element.
func pageShowsCpuBars(pageIdx int) bool {
	return pageHasMatch(pageIdx, func(el DisplayElement) bool {
		return el.Type == "cpu_bars"
	})
}

func pageHasMatch(pageIdx int, match func(DisplayElement) bool) bool {
	if pageIdx < 0 || pageIdx >= templatePageCount() {
		return false
	}
	page, ok := cfg.DisplayTemplate.Elements["page"+strconv.Itoa(pageIdx)]
	if !ok {
		return false
	}
	for _, el := range page {
		if match(el) {
			return true
		}
	}
	return false
}

func signalPingReschedule() {
	for _, ch := range pingReschedule {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func signalLinuxReschedule() {
	select {
	case linuxReschedule <- struct{}{}:
	default:
	}
}

// signalPage0Reschedule wakes every page-0 collector so entering/leaving page 0
// switches its cadence immediately instead of waiting out a leftover 1m sleep.
func signalPage0Reschedule() {
	for _, ch := range page0Reschedule {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func signalLinkReschedule() {
	select {
	case linkReschedule <- struct{}{}:
	default:
	}
}

func main() {
	var wg sync.WaitGroup
	// Resolve the clock's timezone by hand from the first moment: time.Local
	// would lock to UTC if we start before the boot has written the timezone
	// files (see timezone.go).
	startTimezoneKeeper()
	all := flag.Bool("all", false, "if set, listen on all network interfaces (0.0.0.0)")
	port := flag.Int("port", 8081, "TCP port to listen on")
	forceColdBoot := flag.Bool("force-cold-boot", false, "force showing welcome screen even on warm boot")
	useDMA := flag.Bool("dma", true, "enable DMA mode for SPI transfers (default: true)")
	// SPI bus clock in kHz. The 2ad00000.spi controller is fed by a 198 MHz
	// parent and can only divide it by an even integer, so a request is rounded
	// down to {99, 49.5, 33, ...} MHz. The RK35xx SPI block is rated for 50 MHz
	// and the GC9307/ST7789 panel for ~62.5 MHz, so the previous 120 MHz request
	// (which the controller clamped to ~99 MHz) ran ~2x over spec. That only
	// "worked" because the old display driver inserted idle gaps between tiny
	// bursts; the optimized driver streams continuously and pushed marginal
	// units over the edge (blank screen on some machines). 50 MHz rounds to a
	// safe in-spec 49.5 MHz. Override with -spi-mhz for tuning.
	spiMHz := flag.Int("spi-mhz", 50, "SPI bus clock in MHz (rounded down to an achievable divider; keep <=62 for panel spec)")
	showVersion := flag.Bool("version", false, "print version and build time, then exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("photonicat2_mini_display %s (built %s)\n", version, buildTime)
		return
	}
	log.Printf("photonicat2_mini_display %s (built %s)", version, buildTime)

	// Build the listen address:
	var addr string
	if *all {
		addr = fmt.Sprintf(":%d", *port) // all interfaces
	} else {
		addr = fmt.Sprintf("127.0.0.1:%d", *port) // localhost only
	}

	// Asset paths, the font table and the image cache are just string and
	// map setup, but every collector below needs them (collectAndDrawSms
	// builds its font path from assetsPrefix). They move up here with the
	// collectors; only the actual panel bring-up stays below.
	//if assetsFolder not exists, use /usr/local/share/pcat2_mini_display
	if _, err := os.Stat("assets"); os.IsNotExist(err) {
		assetsPrefix = "/usr/local/share/pcat2_mini_display"
	}

	if _, err := os.Stat(assetsPrefix + "/assets"); os.IsNotExist(err) {
		assetsPrefix = "/usr/share/pcat2_mini_display"
	}

	fonts = buildFontTable(assetsPrefix)

	imageCache = make(map[string]*image.RGBA)

	// Warm all font faces off the critical path so the first CJK render
	// doesn't stall a page transition parsing the 37MB CJK collection.
	go preloadFonts()

	// Data collection starts before the panel does. Nothing below this point
	// -- DMA probe, periph host.Init, SPI open, GC9307 configure -- feeds a
	// single globalData value, but on a cold boot it costs several seconds of
	// contended I/O, so the collectors used to start only ~2s before the
	// welcome animation handed the panel over and the first page was drawn
	// from data that had barely been gathered. None of these touch the
	// display, so they now fill in for the whole animation instead of its tail.
	loadAllConfigsToVariables() //load user, default configs

	// Page0's remaining-time slot starts blank: until a real estimate arrives
	// there is nothing to show, so neither the "-" placeholder nor the clock
	// icon should be drawn.
	globalData.Store("RemainingTime", "")
	globalData.Store("RemainingTime_Unit", "")

	// Page-sensitive collector: fixed interval between runs, interruptible
	// via reschedule so entering the matching page does not wait out a
	// leftover long sleep. collect runs immediately on start and on each
	// reschedule.
	startPageSensitiveCollector := func(
		intervalFn func() time.Duration,
		reschedule <-chan struct{},
		setInterval *time.Duration,
		collect func(),
	) {
		go func() {
			collect()
			for weAreRunning() {
				interval := intervalFn()
				if setInterval != nil {
					*setInterval = interval
				}
				timer := time.NewTimer(interval)
				select {
				case <-timer.C:
					collect()
				case <-reschedule:
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					// Page change or wake: refresh immediately.
					collect()
				}
			}
		}()
	}

	// Ping: 1 Hz on the ping page while awake, otherwise 1 minute. Each row runs
	// on its own collector goroutine with its own reschedule channel, so a host
	// that is slow, dead or stuck in DNS only stretches its own row's cadence -
	// the other row keeps ticking. Only the first mirrors the live cadence into
	// pingGatherInterval (both share currentPingInterval).
	startPageSensitiveCollector(currentPingInterval, pingReschedule[0], &pingGatherInterval, func() {
		pingRow0.collect(cfg.PingSite0, cfg.PingType0)
	})
	startPageSensitiveCollector(currentPingInterval, pingReschedule[1], nil, func() {
		pingRow1.collect(cfg.PingSite1, cfg.PingType1)
	})
	// Linux/CPU: 0.5s (2 Hz) on the cpu_bars page while awake, otherwise 1 minute.
	startPageSensitiveCollector(currentLinuxInterval, linuxReschedule, &dataGatherInterval, func() {
		collectLinuxData(cfg)
	})
	// Page 0 (WAN speed, data usage, battery, DC voltage): 2 Hz while page 0 is
	// visible and awake, otherwise 1 minute. Each collector gets its own
	// reschedule channel from page0Reschedule, all signalled together on a page
	// change so every page-0 value updates live in step.
	startPageSensitiveCollector(currentPage0Interval, page0Reschedule[0], &batteryDataInterval, collectBatteryData)
	startPageSensitiveCollector(currentPage0Interval, page0Reschedule[1], &networkGatherInterval, func() {
		collectNetworkData(cfg)
	})
	startPageSensitiveCollector(currentPage0Interval, page0Reschedule[2], nil, getInfoFromPcatWeb)
	startPageSensitiveCollector(currentPage0Interval, page0Reschedule[3], nil, collectWANNetworkSpeed)
	// GPS: 1 Hz while the GPS page is visible, 30 s otherwise. This collector
	// also adds/removes the GPS page as the web toggle flips.
	startPageSensitiveCollector(currentGpsInterval, gpsReschedule, nil, collectGpsData)
	// Carrier-aware egress + public-IP flush/reprobe: every 2s while awake on
	// any page (not only page 0), so unplug drops the eth icon / PUBLIC IP
	// promptly and plug-in re-probes without waiting for the 60s web poll.
	startPageSensitiveCollector(currentLinkInterval, linkReschedule, nil, collectLinkStatus)

	go collectFixedData()
	go getSmsPages()
	startPmuManager() //board temp + G-sensor from the PMU (sysfs or direct UART)

	// Power graph history: loads the persisted trail and starts the 1 Hz
	// sampler. Another pure globalData/disk collector, so it belongs with
	// the rest of the data path rather than behind the panel.
	// Initialize power graph data recording
	initPowerDataRecording()

	// Initialize and start custom metrics manager
	if len(cfg.CustomMetrics.Sources) > 0 {
		var err error
		customMetricsMgr, err = NewCustomMetricManager(cfg.CustomMetrics)
		if err != nil {
			log.Printf("Failed to create custom metrics manager: %v", err)
		} else {
			if err := customMetricsMgr.Start(); err != nil {
				log.Printf("Failed to start custom metrics manager: %v", err)
			} else {
				log.Printf("Custom metrics manager started with %d sources", len(cfg.CustomMetrics.Sources))
			}
		}
	}

	// Set DMA mode from command line flag
	dmaMode = *useDMA
	if dmaMode {
		log.Println("\033[32mSPI DMA mode: ENABLED\033[0m")
	} else {
		log.Println("\033[31mSPI DMA mode: DISABLED\033[0m")
	}

	// Check if DMA channels are available
	if err := checkDMAAvailability(); err != nil {
		log.Printf("\033[31mDMA not available, falling back to non-DMA mode: %v\033[0m", err)
		dmaMode = false
		spiTransferOptimized = false
	} else {
		spiTransferOptimized = dmaMode
		if dmaMode {
			log.Println("\033[32mDMA channels detected and enabled for optimized transfers\033[0m")
		}
	}

	// Log the current SPI configuration
	logSPIMode()

	//rm pcat_display_initialized
	os.Remove("/tmp/pcat_display_initialized")

	// Older builds wrote generated SVG/PNG frames to /tmp; they are rendered
	// in memory now, so sweep any leftovers from a previous version.
	for _, pattern := range []string{"/tmp/strength-*.svg", "/tmp/barBackground.svg", "/tmp/barProgress_*.svg", "/tmp/welcome.png"} {
		if matches, err := filepath.Glob(pattern); err == nil {
			for _, m := range matches {
				os.Remove(m)
			}
		}
	}
	// Initialize board.
	if _, err := host.Init(); err != nil {
		log.Fatal(err)
	}
	rand.Seed(time.Now().UnixNano())
	// Open SPI.
	spiPort, err := spireg.Open("SPI1.0")
	if err != nil {
		log.Fatal(err)
	}
	defer spiPort.Close()

	spiHz := physic.Frequency(*spiMHz) * physic.MegaHertz
	log.Printf("SPI bus clock requested: %s (controller rounds down to an achievable divider)", spiHz)
	conn, err := spiPort.Connect(spiHz, spi.Mode0, 8)
	if err != nil {
		log.Fatal(err)
	}

	// Setup display.
	display = gc9307.New(conn, gpioreg.ByName(RST_PIN), gpioreg.ByName(DC_PIN), gpioreg.ByName(CS_PIN), gpioreg.ByName(BL_PIN))
	display.Configure(gc9307.Config{
		Width:        PCAT2_LCD_WIDTH,
		Height:       PCAT2_LCD_HEIGHT,
		Rotation:     gc9307.ROTATION_180,
		RowOffset:    0,
		ColumnOffset: PCAT2_X_OFFSET,
		FrameRate:    gc9307.FRAMERATE_60,
		VSyncLines:   gc9307.MAX_VSYNC_SCANLINES,
		UseCS:        false,
		// Stream frames in the largest SPI transfers the kernel allows
		// (spidev bufsiz, ~4096 bytes) instead of tiny per-line batches.
		// Cuts a middle-frame send from ~143 Tx syscalls to ~23.
		UseDMA: dmaMode,
	})

	// Initialize display wrapper with DMA optimization
	displayWrapper = NewDisplayWrapper(display)
	log.Printf("Display wrapper initialized with transfer stats: %+v", displayWrapper.GetTransferStats())

	// Initialize shutdown monitoring (temporarily disabled for testing)
	// This monitors system shutdown/restart before SIGTERM is sent
	// EnableDBusLogind: monitors D-Bus logind PrepareForShutdown/PrepareForSleep signals
	// EnableProcComm: monitors /proc/1/comm for init process changes
	// Both methods detect shutdown earlier than SIGTERM for instant shutdown screen display
	/*
		shutdownConfig := ShutdownMonitorConfig{
			EnableDBusLogind: true,  // Monitor D-Bus logind signals (recommended)
			EnableProcComm:   true,  // Monitor /proc/1/comm changes (backup method)
		}

		if shutdownConfig.EnableDBusLogind || shutdownConfig.EnableProcComm {
			shutdownMonitor = NewShutdownMonitor(shutdownConfig)
			if err := shutdownMonitor.Start(); err != nil {
				log.Printf("Failed to start shutdown monitoring: %v", err)
			} else {
				log.Printf("Shutdown monitoring started (D-Bus: %v, /proc/1/comm: %v)",
					shutdownConfig.EnableDBusLogind, shutdownConfig.EnableProcComm)
			}
		}
	*/

	wg.Add(1) //first show welcome and do some other things and wait
	go func() {
		defer wg.Done()

		// Check if we should show welcome screen
		shouldShowWelcome := *forceColdBoot // Always show if forced

		if !shouldShowWelcome {
			// First start since power-on. The uptime heuristic alone misfired
			// on photonicat2/OpenWrt, where the boot chain takes >60s before
			// this daemon starts, so every real boot read as "warm" and the
			// animation never played. The marker in tmpfs is authoritative
			// (absent = nothing ran since power-on); uptime stays as a
			// fallback for systems where /tmp survives a reboot.
			_, markerErr := os.Stat(WELCOME_MARKER_PATH)
			firstStartThisBoot := os.IsNotExist(markerErr)
			if uptimeSeconds, err := getUptimeSeconds(); err != nil {
				log.Printf("Failed to get uptime, showing welcome: %v", err)
				shouldShowWelcome = true // Default to showing welcome on error
			} else if firstStartThisBoot || uptimeSeconds <= 60 {
				log.Printf("Cold boot detected (uptime: %.1fs, first start this boot: %v), showing welcome screen", uptimeSeconds, firstStartThisBoot)
				shouldShowWelcome = true
			} else {
				log.Printf("Warm boot detected (uptime: %.1fs), skipping welcome screen", uptimeSeconds)
			}
		} else {
			log.Println("Welcome screen forced by --force-cold-boot flag")
		}

		if err := os.WriteFile(WELCOME_MARKER_PATH,
			[]byte(fmt.Sprintf("welcome=%v time=%s\n", shouldShowWelcome, time.Now().Format(time.RFC3339))), 0644); err != nil {
			log.Printf("Could not write welcome marker: %v", err)
		}

		if shouldShowWelcome {
			// Cold boot or --force-cold-boot: play the wake-up animation.
			showWelcome(display, PCAT2_LCD_WIDTH, PCAT2_LCD_HEIGHT, welcomeAnimDur*time.Second)
		}
	}()

	go httpServer(addr)                          //listen local for http request
	go monitorKeyboard(&changePageTriggered)     // Start keyboard monitoring in a goroutine
	go monitorConsoleInput(&changePageTriggered) // Start console input monitoring in a goroutine
	go idleDimmer()                              //control backlight

	registerExitHandler() //catch sigterm

	init3FrameBuffers()

	prepareMainLoop()

	wg.Wait()

	// Use optimized main loop if buffer manager is initialized
	if bufferManager != nil {
		mainLoopOptimized()
	} else {
		mainLoop()
	} //main loop

	select {} //blocking for sigterm processing
}

func registerExitHandler() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() { //register ciao screen when sigterm
		sig := <-sigs
		log.Printf("Received signal: %v", sig)
		setWeAreRunning(false)
		offTime = time.Now()

		// Stop shutdown monitoring (temporarily disabled)
		/*
			if shutdownMonitor != nil {
				shutdownMonitor.Stop()
			}
		*/

		time.Sleep(200 * time.Millisecond)

		// Stop custom metrics manager
		if customMetricsMgr != nil {
			log.Printf("Stopping custom metrics manager...")
			if err := customMetricsMgr.Stop(); err != nil {
				log.Printf("Error stopping custom metrics manager: %v", err)
			}
		}

		// Different behavior for SIGTERM vs SIGINT
		if sig == syscall.SIGTERM {
			log.Println("System shutdown detected, showing shutdown screen")
			showCiao(display, PCAT2_LCD_WIDTH, PCAT2_LCD_HEIGHT, OFF_TIMEOUT)
			time.Sleep(OFF_TIMEOUT)
		} else {
			log.Println("Manual interruption detected, showing shutdown screen but dimming instantly")
			showCiaoInstant(display, PCAT2_LCD_WIDTH, PCAT2_LCD_HEIGHT)
		}

		os.Exit(0)
	}()
}

func init3FrameBuffers() {
	// Initialize optimized buffer manager
	bufferManager = NewBufferManager()
	// Initialize render cache
	renderCache = NewRenderCache()
	// Initialize dirty region tracker
	dirtyTracker = NewDirtyRegionTracker()
	// Initialize legacy buffers for backward compatibility
	initLegacyBuffers()
	initLegacyTransitionBuffers()
	// Initialize pre-allocated transition frame buffers
	initTransitionFrameBuffers()
}

func prepareMainLoop() {
	stitchedFrame = image.NewRGBA(image.Rect(0, 0, middleFrameWidth*2, middleFrameHeight))
	croppedFrameBuffer = image.NewRGBA(image.Rect(0, 0, middleFrameWidth, middleFrameHeight))
	nextPageIdxFrameBuffer = image.NewRGBA(image.Rect(0, 0, middleFrameWidth, middleFrameHeight))

	// Initialize performance optimization
	easingLookup = preCalculateEasing(numIntermediatePages, middleFrameWidth)
	lastFPSUpdate = time.Now()
}

func mainLoop() {
	log.Println("Main loop started")
	localIdx := 0
	nextLocalIdx := 0
	isSMS := false
	nextPageIdx := 0
	isNextPageSMS := false
	faceTiny, _, err := getFontFace("tiny")

	// Track frame-by-frame performance during transition
	frameTimestamps := make([]time.Time, numIntermediatePages+1)
	copyTimings := make([]int, numIntermediatePages)
	sendTimings := make([]int, numIntermediatePages)

	// Reused across frames so the FPS sleep does not allocate a timer each time.
	frameSleepTimer := time.NewTimer(time.Hour)
	if !frameSleepTimer.Stop() {
		<-frameSleepTimer.C
	}

	if err != nil {
		log.Fatalf("Failed to load font: %v", err)
	}

	for weAreRunning() {
		if middleFrames%300 == 0 { // Log less frequently
			log.Println("showsms:", cfg.ShowSms, "totalPages:", totalNumPages, "cfgPages:", cfgNumPages)
		}
		if runMainLoop {
			start := time.Now()
			if changePageTriggered || httpChangePageTriggered { //CHANGE PAGE
				if buttonPressInProgress { // Too soon, skip this press
					changePageTriggered = false
					httpChangePageTriggered = false
					continue
				}

				log.Printf("🔄 Page change called")
				now := time.Now()

				// Mark button press in progress and immediately set activity
				buttonPressInProgress = true
				lastButtonPress = now
				lastActivityMu.Lock()
				lastActivity = now
				lastActivityMu.Unlock()
				// New page → always re-render middle (fingerprint would still
				// match if two pages share the same keys with equal values).
				forceMiddleRedraw = true
				lastMiddleFingerprint = ""

				// Optimize page calculations - calculate once and reuse
				currPageIdx = currPageIdx % totalNumPages
				nextPageIdx = (currPageIdx + 1) % totalNumPages

				// Pre-calculate SMS status to avoid redundant checks
				isSMS = cfg.ShowSms && currPageIdx >= cfgNumPages
				isNextPageSMS = cfg.ShowSms && nextPageIdx >= cfgNumPages

				if isSMS {
					localIdx = currPageIdx - cfgNumPages
				} else {
					localIdx = currPageIdx
				}

				if isNextPageSMS {
					if lenSmsPagesImages <= 0 {
						lenSmsPagesImages = 1
					}
					nextLocalIdx = (nextPageIdx - cfgNumPages) % lenSmsPagesImages
				} else {
					if cfgNumPages > 0 {
						nextLocalIdx = nextPageIdx % cfgNumPages
					} else {
						nextLocalIdx = 0
					}
				}

				log.Println("curr/next Idx:", currPageIdx, nextPageIdx, "json/sms/total:", cfgNumPages, lenSmsPagesImages, totalNumPages, "localIdx:", localIdx, "nextLocalIdx:", nextLocalIdx, "isSMS:", isSMS, "isNextPageSMS:", isNextPageSMS)

				clearFrame(nextPageIdxFrameBuffer, middleFrameWidth, middleFrameHeight)

				// Time the next frame rendering
				nextFrameRenderStart := time.Now()
				renderMiddle(nextPageIdxFrameBuffer, &cfg, isNextPageSMS, nextLocalIdx)
				nextFrameRenderEnd := time.Now()
				nextFrameRenderDuration := nextFrameRenderEnd.Sub(nextFrameRenderStart)

				if showDetailedTiming {
					log.Printf("📱 Render: %.1fms (page %d)",
						float64(nextFrameRenderDuration.Nanoseconds())/1000000.0, nextPageIdx)
				}

				stitchStart := time.Now()
				stitchStartTime = stitchStart // Record stitch start for button timing

				// Use optimized stitching for better performance
				err := stitchFramesOptimized(stitchedFrame, middleFramebuffers[(middleFrames+1)%2], nextPageIdxFrameBuffer)
				if err != nil {
					// Fallback to original method if optimized fails
					log.Printf("⚠️ Optimized stitch failed, using fallback: %v", err)
					copyImageToImageAt(stitchedFrame, middleFramebuffers[(middleFrames+1)%2], 0, 0) //current frame
					copyImageToImageAt(stitchedFrame, nextPageIdxFrameBuffer, middleFrameWidth, 0)  //next frame
				}

				stitchEnd := time.Now()

				// Calculate timing from button events to stitch completion
				stitchDuration := stitchEnd.Sub(stitchStart)

				if showDetailedTiming {
					log.Printf("🔧 Stitch: %.1fms", durationToMs(stitchDuration))
				}

				calculateTransitionFramesAsync(stitchedFrame, easingLookup)

				// Initialize frame timing tracking
				frameTimestamps[0] = time.Now() // Start of transition

				// Process all frames - use pre-calculated if ready, otherwise calculate on-demand
				for i := 1; i < numIntermediatePages; i++ {
					if i == numIntermediatePages/3 { // Update page indices at the halfway point /3 cos we are not linear.
						prevPageIdx := currPageIdx
						localIdx = nextLocalIdx
						currPageIdx = nextPageIdx
						isSMS = isNextPageSMS
						// Entering/leaving a page-sensitive screen changes
						// its collector cadence; wake the matching goroutine.
						if pageShowsPing(prevPageIdx) != pageShowsPing(currPageIdx) {
							signalPingReschedule()
						}
						if pageShowsCpuBars(prevPageIdx) != pageShowsCpuBars(currPageIdx) {
							signalLinuxReschedule()
						}
						if isPage0(prevPageIdx) != isPage0(currPageIdx) {
							signalPage0Reschedule()
						}
						// GPS page (last cfg page when shown): 1 Hz on it,
						// 30 s off it.
						signalGpsReschedule()
						nextPageLength := 0
						if isNextPageSMS {
							nextPageLength = lenSmsPagesImages
						} else {
							nextPageLength = cfgNumPages
						}
						var displayPageIdx int
						if isNextPageSMS {
							displayPageIdx = nextLocalIdx
						} else {
							displayPageIdx = nextPageIdx
						}
						drawFooter(display, footerFramebuffers[middleFrames%2], displayPageIdx, nextPageLength, isNextPageSMS)
					}

					// Try to use pre-calculated frame, fallback to real-time calculation
					var frameToSend *image.RGBA
					frameReady := false

					// Check if this frame is pre-calculated (non-blocking)
					transitionMutex.RLock()
					if transitionFramesReady {
						// All frames are ready
						frameToSend = transitionFrames[i]
						frameReady = true
						copyTimings[i] = 0 // Mark as pre-calculated
						// Pre-calculated frame used silently
					} else {
						// Check channel for ready frames (non-blocking)
						select {
						case readyIndex := <-transitionFrameChannel:
							if readyIndex >= i {
								frameToSend = transitionFrames[i]
								frameReady = true
								copyTimings[i] = 0 // Mark as pre-calculated
								// Frame retrieved from async channel silently
								// Put the signal back for later frames
								select {
								case transitionFrameChannel <- readyIndex:
								default:
								}
							} else {
								// Put back and use fallback
								select {
								case transitionFrameChannel <- readyIndex:
								default:
								}
							}
						default:
							// Channel empty, use fallback
						}
					}
					transitionMutex.RUnlock()

					if !frameReady {
						// Fallback: calculate frame on-demand if not pre-calculated
						log.Printf("🔨 Frame not ready")
						copyStart := time.Now()
						xPos := easingLookup[i]
						copyImageRegion(croppedFrameBuffer, stitchedFrame, xPos, 0, middleFrameWidth, middleFrameHeight)
						copyEnd := time.Now()
						copyTimings[i] = int(copyEnd.Sub(copyStart).Microseconds())
						frameToSend = croppedFrameBuffer
					}

					// Send frame immediately
					sendStart := time.Now()
					sendMiddle(display, frameToSend)
					sendEnd := time.Now()
					sendTimings[i] = int(sendEnd.Sub(sendStart).Microseconds())

					middleFrames++
					stitchedFrames++
					frameTimestamps[i] = time.Now() // Record end time of this frame
				}
				// Record final timestamp for the last frame duration calculation
				frameTimestamps[numIntermediatePages] = time.Now()

				// Print total transition time
				transitionEnd := time.Now()
				totalTransitionDuration := transitionEnd.Sub(frameTimestamps[0])
				if showDetailedTiming {
					log.Printf("🎬 Transition: %.1fms (%d frames)",
						float64(totalTransitionDuration.Nanoseconds())/1000000.0, numIntermediatePages-1)
				}

				//=============== begin of performance printing ===============
				// Print detailed timing when page change animation is complete
				pageChangeEnd := time.Now()
				pageChangeDuration := pageChangeEnd.Sub(start)
				pageChangeFPS := float64(numIntermediatePages) / pageChangeDuration.Seconds()

				// Calculate frame-by-frame timing statistics
				var frameDurations []int
				var minFrameTime, maxFrameTime, totalFrameTime int
				minFrameTime = int(^uint(0) >> 1) // Max int value

				for i := 1; i < len(frameTimestamps); i++ {
					// Validate timestamps before calculating duration
					if frameTimestamps[i].IsZero() || frameTimestamps[i-1].IsZero() {
						continue
					}

					duration := frameTimestamps[i].Sub(frameTimestamps[i-1])
					frameDuration := int(duration.Microseconds())

					// Validate duration is reasonable (between 1μs and 1 second)
					if frameDuration < 1 || frameDuration > 1000000 {
						continue
					}

					frameDurations = append(frameDurations, frameDuration)
					totalFrameTime += frameDuration
					if frameDuration < minFrameTime {
						minFrameTime = frameDuration
					}
					if frameDuration > maxFrameTime {
						maxFrameTime = frameDuration
					}
				}

				// Handle case where no valid frames were processed
				var avgFrameTime int
				if len(frameDurations) > 0 {
					avgFrameTime = totalFrameTime / len(frameDurations)
				}

				// Fix min/max values if no valid frames were processed
				if len(frameDurations) == 0 {
					minFrameTime = 0
					maxFrameTime = 0
				}

				// Calculate button press to transition finish timing
				var buttonToFinishMs float64
				if !buttonKeydownTime.IsZero() {
					buttonToFinishMs = durationToMs(pageChangeEnd.Sub(buttonKeydownTime))
				}

				// Simple consolidated timing print
				log.Printf("✅ Page change: %.1fms | Button→Finish: %.1fms | Frames: %.1fms avg | FPS: %d",
					durationToMs(pageChangeDuration),
					buttonToFinishMs,
					float64(avgFrameTime)/1000.0,
					int(pageChangeFPS))

				// Show detailed breakdown only when requested
				if showDetailedTiming {
					preCalculatedCount := 0
					for _, duration := range copyTimings {
						if duration == 0 {
							preCalculatedCount++
						}
					}
					if len(copyTimings) > 0 {
						log.Printf("⚡ Async: %d/%d (%.0f%%) | Range: %.1f-%.1fms",
							preCalculatedCount, len(copyTimings),
							float64(preCalculatedCount)/float64(len(copyTimings))*100,
							float64(minFrameTime)/1000.0, float64(maxFrameTime)/1000.0)
					}
				}
				//=============== end of performance printing ===============

				// Mark button press complete
				buttonPressInProgress = false
				httpChangePageTriggered = false
				changePageTriggered = false
			} else { //normal page rendering
				// Backlight is physically off: skip full middle/footer renders
				// to save power, but still refresh the top bar once a minute
				// so the clock (and battery SOC) stay current for wake / dim
				// idle-visible cases. drawTopBar no-ops when magicStr is
				// unchanged, so this is cheap between minute ticks.
				if displayAsleep() {
					if forceTopBarRedraw || time.Since(lastTopBarRefresh) >= time.Minute {
						forceTopBarRedraw = false
						drawTopBar(display, topBarFramebuffers[topFrames%2])
						lastTopBarRefresh = time.Now()
						topFrames++
					}
					select {
					case <-pageChangeSignal:
						// fall through; next iteration handles the change
					case <-time.After(200 * time.Millisecond):
					}
					continue
				}

				// Top bar (clock + battery + signal): 2 Hz while awake so battery
				// SOC / signal / charging update promptly and the clock flips
				// within 0.5s of the minute boundary; on wake; or at least once
				// per minute (covers low idle FPS where a time gate would stall
				// the clock). drawTopBar no-ops when nothing visible changed, so
				// the 2 Hz poll is cheap between real updates.
				topBarInterval := time.Minute
				if idleState != STATE_IDLE {
					topBarInterval = 500 * time.Millisecond
				}
				if forceTopBarRedraw || time.Since(lastTopBarRefresh) >= topBarInterval {
					forceTopBarRedraw = false
					drawTopBar(display, topBarFramebuffers[topFrames%2])
					lastTopBarRefresh = time.Now()
				}

				// Update footer less frequently as well, except when showing SMS
				if cfg.ShowSms && isSMS {
					if middleFrames%3 == 0 { // Even SMS footer doesn't need to update every frame
						drawFooter(display, footerFramebuffers[middleFrames%2], localIdx, len(smsPagesImages), isSMS)
					}
				} else if middleFrames%10 == 0 { // Update footer every 10 frames for non-SMS pages
					drawFooter(display, footerFramebuffers[middleFrames%2], localIdx, cfgNumPages, isSMS)
				}

				// Middle: skip clear+render+SPI when the page's data has not
				// changed and nothing is ticker-scrolling. Saves CPU and the
				// SPI bus (largest active-screen power cost after backlight).
				fp := middlePageFingerprint(&cfg, isSMS, localIdx)
				overflowScroll := !isSMS && pageHasOverflowText(&cfg, localIdx)
				needMiddle := forceMiddleRedraw || overflowScroll || fp != lastMiddleFingerprint || showFPS
				if needMiddle {
					forceMiddleRedraw = false
					clearFrame(middleFramebuffers[middleFrames%2], middleFrameWidth, middleFrameHeight)
					// renderMiddle sets anyTextScrolling if any value overflowed
					// and is animating; reset first so the flag is this pass only.
					anyTextScrolling = false
					renderMiddle(middleFramebuffers[middleFrames%2], &cfg, isSMS, localIdx)

					//draw fps - use cached text for better performance
					if showFPS {
						// Update cached FPS text periodically
						if time.Since(lastFPSUpdate) > 100*time.Millisecond {
							lastFPSUpdate = time.Now()
							cachedFPSText = "FPS:" + strconv.Itoa(int(fps)) + ", " + strconv.Itoa(middleFrames)
						}
						if cachedFPSText != "" {
							drawText(middleFramebuffers[middleFrames%2], cachedFPSText, 10, 240, faceTiny, PCAT_RED, false)
						}
					}
					sendMiddle(display, middleFramebuffers[middleFrames%2])
					lastMiddleFingerprint = fp
					middleFrames++
				} else {
					// Keep frame counter moving slowly so %N footer/top cadence
					// and FPS logs still advance while content is static.
					middleFrames++
				}

				// stable‐FPS sleep with signal-based interruption for page changes
				// Apply idle multiplier to FPS during idle state (effectively 0.1 FPS)
				effectiveFPS := desiredFPS
				if idleState == STATE_IDLE {
					effectiveFPS = 1 // This gives us 1 FPS, but we'll multiply the sleep time by 10
				} else if (anyTextScrolling || overflowScroll) && SCROLL_FPS > effectiveFPS {
					// A ticker is mid-scroll and the screen is awake: render fast
					// enough for smooth motion. Reverts to desiredFPS automatically
					// once the text stops overflowing.
					effectiveFPS = SCROLL_FPS
				} else if !needMiddle {
					// Content static: poll for data changes less often. Collectors
					// still update globalData; we just check the fingerprint at
					// ~2 Hz instead of burning 3 full redraws/s for nothing.
					if effectiveFPS > 2 {
						effectiveFPS = 2
					}
				}
				baseSleep := time.Second / time.Duration(effectiveFPS)
				if idleState == STATE_IDLE {
					baseSleep = baseSleep * time.Duration(idleMultiplier) // 1 FPS * 10 = 0.1 FPS
				}
				if delta := (baseSleep - time.Since(start)); delta > 0 {
					sleepDuration := time.Duration(float64(delta) * 0.99)
					// Reuse one timer to avoid time.After heap churn every frame.
					if !frameSleepTimer.Stop() {
						select {
						case <-frameSleepTimer.C:
						default:
						}
					}
					frameSleepTimer.Reset(sleepDuration)
					select {
					case <-pageChangeSignal:
						if !frameSleepTimer.Stop() {
							select {
							case <-frameSleepTimer.C:
							default:
							}
						}
						// Page change triggered, exit sleep immediately
						if showDetailedTiming {
							log.Printf("⚡ FPS sleep interrupted for immediate page change")
						}
					case <-frameSleepTimer.C:
						// Normal sleep completion
					}
				}
			}

			if middleFrames%100 == 0 {
				if autoRotatePages {
					changePageTriggered = true
					// Signal the main loop to interrupt FPS sleep
					signalPageChange()
				}
				now := time.Now()
				fps = 100 / now.Sub(lastUpdate).Seconds()
				log.Printf("FPS: %0.1f, Total Frames: %d\n", fps, middleFrames)
				lastUpdate = now
				log.Printf("Pages: total=%d, current=%d, cfg=%d, sms=%d, showSms=%t",
					totalNumPages, currPageIdx, cfgNumPages, lenSmsPagesImages, cfg.ShowSms)
			}
		} else {
			time.Sleep(50 * time.Millisecond) //not inf loop
		}
	}
}

// DoubleBuffer holds two buffers for double buffering
type DoubleBuffer struct {
	buffers [2]*image.RGBA
	active  int
	mutex   sync.RWMutex
}

// GetActive returns the active buffer
func (db *DoubleBuffer) GetActive() *image.RGBA {
	db.mutex.RLock()
	defer db.mutex.RUnlock()
	return db.buffers[db.active]
}

// BufferManager manages frame buffers
type BufferManager struct {
	topBar *DoubleBuffer
	middle *DoubleBuffer
	footer *DoubleBuffer
	mutex  sync.RWMutex
}

// NewBufferManager creates a new buffer manager
func NewBufferManager() *BufferManager {
	return &BufferManager{
		topBar: &DoubleBuffer{
			buffers: [2]*image.RGBA{
				image.NewRGBA(image.Rect(0, 0, topBarFrameWidth, topBarFrameHeight)),
				image.NewRGBA(image.Rect(0, 0, topBarFrameWidth, topBarFrameHeight)),
			},
			active: 0,
		},
		middle: &DoubleBuffer{
			buffers: [2]*image.RGBA{
				image.NewRGBA(image.Rect(0, 0, middleFrameWidth, middleFrameHeight)),
				image.NewRGBA(image.Rect(0, 0, middleFrameWidth, middleFrameHeight)),
			},
			active: 0,
		},
		footer: &DoubleBuffer{
			buffers: [2]*image.RGBA{
				image.NewRGBA(image.Rect(0, 0, footerFrameWidth, footerFrameHeight)),
				image.NewRGBA(image.Rect(0, 0, footerFrameWidth, footerFrameHeight)),
			},
			active: 0,
		},
	}
}

// GetFrameFromPool gets a frame from the buffer pool
func (bm *BufferManager) GetFrameFromPool(width, height int) *image.RGBA {
	bm.mutex.Lock()
	defer bm.mutex.Unlock()
	// For now, just create a new frame
	return image.NewRGBA(image.Rect(0, 0, width, height))
}

// ReturnFrameToPool returns a frame to the buffer pool
func (bm *BufferManager) ReturnFrameToPool(frame *image.RGBA) {
	// For now, this is a no-op
}

// RenderCache caches rendered elements
type RenderCache struct {
	cache map[string]*image.RGBA
	mutex sync.RWMutex
}

// NewRenderCache creates a new render cache
func NewRenderCache() *RenderCache {
	return &RenderCache{
		cache: make(map[string]*image.RGBA),
	}
}

// DirtyRegionTracker tracks dirty regions for optimization
type DirtyRegionTracker struct {
	regions []image.Rectangle
	mutex   sync.RWMutex
}

// NewDirtyRegionTracker creates a new dirty region tracker
func NewDirtyRegionTracker() *DirtyRegionTracker {
	return &DirtyRegionTracker{
		regions: make([]image.Rectangle, 0),
	}
}

// mainLoopOptimized runs the optimized main loop
func mainLoopOptimized() {
	// For now, fallback to the regular main loop
	mainLoop()
}

// initLegacyBuffers initializes the legacy framebuffers for backward compatibility
func initLegacyBuffers() {
	topBarFramebuffers[0] = image.NewRGBA(image.Rect(0, 0, topBarFrameWidth, topBarFrameHeight))
	topBarFramebuffers[1] = image.NewRGBA(image.Rect(0, 0, topBarFrameWidth, topBarFrameHeight))

	middleFramebuffers[0] = image.NewRGBA(image.Rect(0, 0, middleFrameWidth, middleFrameHeight))
	middleFramebuffers[1] = image.NewRGBA(image.Rect(0, 0, middleFrameWidth, middleFrameHeight))

	footerFramebuffers[0] = image.NewRGBA(image.Rect(0, 0, footerFrameWidth, footerFrameHeight))
	footerFramebuffers[1] = image.NewRGBA(image.Rect(0, 0, footerFrameWidth, footerFrameHeight))
}

// initLegacyTransitionBuffers initializes transition buffers
func initLegacyTransitionBuffers() {
	// Initialize any additional transition buffers if needed
	// This is a placeholder for compatibility
}

// getTopBarFramebuffer returns the top bar framebuffer at the specified index
func getTopBarFramebuffer(index int) *image.RGBA {
	if index < 0 || index >= len(topBarFramebuffers) {
		return topBarFramebuffers[0]
	}
	return topBarFramebuffers[index]
}

// getMiddleFramebuffer returns the middle framebuffer at the specified index
func getMiddleFramebuffer(index int) *image.RGBA {
	if index < 0 || index >= len(middleFramebuffers) {
		return middleFramebuffers[0]
	}
	return middleFramebuffers[index]
}

// getFooterFramebuffer returns the footer framebuffer at the specified index
func getFooterFramebuffer(index int) *image.RGBA {
	if index < 0 || index >= len(footerFramebuffers) {
		return footerFramebuffers[0]
	}
	return footerFramebuffers[index]
}

// initTransitionFrameBuffers initializes pre-allocated transition frame buffers
func initTransitionFrameBuffers() {
	transitionFrames = make([]*image.RGBA, numIntermediatePages)
	for i := 0; i < numIntermediatePages; i++ {
		transitionFrames[i] = image.NewRGBA(image.Rect(0, 0, middleFrameWidth, middleFrameHeight))
	}
	log.Printf("🎬 Initialized %d pre-allocated transition frame buffers", numIntermediatePages)
}

// calculateTransitionFramesAsync calculates all transition frames in the background
func calculateTransitionFramesAsync(stitchedFrame *image.RGBA, easingValues []int) {
	// Safety check
	if stitchedFrame == nil || len(easingValues) != numIntermediatePages {
		log.Printf("❌ calculateTransitionFramesAsync: invalid parameters")
		return
	}

	transitionMutex.Lock()
	if transitionCalculating {
		transitionMutex.Unlock()
		return // Already calculating
	}
	transitionCalculating = true
	transitionFramesReady = false
	transitionMutex.Unlock()

	go func() {
		asyncStartTime := time.Now()
		if showDetailedTiming {
			log.Printf("⚡ Starting async rendering of %d frames", numIntermediatePages-1)
		}

		defer func() {
			asyncEndTime := time.Now()
			asyncTotalDuration := asyncEndTime.Sub(asyncStartTime)
			if showDetailedTiming {
				log.Printf("🎭 Async complete: %.1fms (%d frames, %.1fms avg)",
					float64(asyncTotalDuration.Nanoseconds())/1000000.0,
					numIntermediatePages-1,
					float64(asyncTotalDuration.Nanoseconds())/float64(numIntermediatePages-1)/1000000.0)
			}

			transitionMutex.Lock()
			transitionCalculating = false
			transitionFramesReady = true
			transitionMutex.Unlock()

			// Signal completion
			select {
			case transitionCompleteChannel <- true:
			default:
			}
		}()

		// Clear any existing channel data
		for len(transitionFrameChannel) > 0 {
			<-transitionFrameChannel
		}

		var frameTimes []float64
		// Pre-calculate transition frames starting from frame 1
		for i := 1; i < numIntermediatePages; i++ {
			frameRenderStart := time.Now()

			// Safety bounds check
			if i >= len(transitionFrames) || i >= len(easingValues) {
				log.Printf("❌ Frame index %d out of bounds", i)
				continue
			}

			xPos := easingValues[i]
			copyImageRegion(transitionFrames[i], stitchedFrame, xPos, 0, middleFrameWidth, middleFrameHeight)

			frameRenderEnd := time.Now()
			frameRenderDuration := frameRenderEnd.Sub(frameRenderStart)
			frameTimes = append(frameTimes, float64(frameRenderDuration.Nanoseconds())/1000000.0)

			// Individual frame timing removed for cleaner logs

			// Signal that this frame is ready (non-blocking)
			select {
			case transitionFrameChannel <- i:
			default:
				log.Printf("⚠️ Channel full, frame %d signal dropped", i)
			}
		}

		// Log timing statistics
		if len(frameTimes) > 0 {
			var minTime, maxTime, totalTime float64
			minTime = frameTimes[0]
			maxTime = frameTimes[0]
			for _, t := range frameTimes {
				totalTime += t
				if t < minTime {
					minTime = t
				}
				if t > maxTime {
					maxTime = t
				}
			}
			if showDetailedTiming {
				log.Printf("📊 Async timing: %.1f-%.1fms range, %.1fms total", minTime, maxTime, totalTime)
			}
		}
	}()
}

// Helper function to convert duration to milliseconds with 1 decimal place
func durationToMs(d time.Duration) float64 {
	return float64(d.Nanoseconds()) / 1000000.0
}

// Helper function to format timing as milliseconds string
func formatTiming(d time.Duration) string {
	return fmt.Sprintf("%.1fms", durationToMs(d))
}

// Helper function to signal page change for FPS sleep interruption
func signalPageChange() {
	select {
	case pageChangeSignal <- struct{}{}:
	default: // Channel full, signal already sent
	}
}

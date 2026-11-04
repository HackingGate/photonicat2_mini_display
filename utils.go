package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	evdev "github.com/holoplot/go-evdev"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
)

var (
	fadeMu               sync.Mutex
	fadeCancel           chan struct{}
	swippingScreen       bool
	wasScreenIdle        bool // Track if screen was idle when key was pressed
	wasConsoleScreenIdle bool // Track if screen was idle for console input
)

// Testability seams: these default to the real hardware / system paths and are
// never reassigned at runtime; tests point them at t.TempDir() fixtures so the
// surrounding logic can run without touching /sys or /etc.
var (
	utilCovBacklightPath       = "/sys/class/backlight/backlight/brightness"
	utilCovMovementTriggerPath = "/sys/kernel/photonicat-pm/movement_trigger"
	utilCovEtcConfigPath       = ETC_CONFIG_PATH
	utilCovEtcDebianConfigPath = ETC_DEBIAN_CONFIG_PATH
	utilCovEtcUserConfigPath   = ETC_USER_CONFIG_PATH
	utilCovLocalUserConfigPath = "user_config.json"
	utilCovIsOpenWRT           = isOpenWRT
)

// loadConfig reads and unmarshals the config file.
func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var tempCfg Config
	err = secureUnmarshal(data, &tempCfg)
	return tempCfg, err
}

var (
	fontCache = make(map[string]struct {
		face       font.Face
		fontHeight int
	})
	fontCacheMu sync.Mutex

	// Parsed font files keyed by path, so the multiple size variants of one
	// file (and especially the ~37MB CJK collection) are read and parsed once.
	parsedFontCache   = make(map[string]*opentype.Font)
	parsedFontCacheMu sync.Mutex
)

// parseFontFile reads and parses a TTF/OTF/TTC file, caching the result by path.
func parseFontFile(fontPath string) (*opentype.Font, error) {
	parsedFontCacheMu.Lock()
	if f, ok := parsedFontCache[fontPath]; ok {
		parsedFontCacheMu.Unlock()
		return f, nil
	}
	parsedFontCacheMu.Unlock()

	fontBytes, err := os.ReadFile(fontPath)
	if err != nil {
		return nil, fmt.Errorf("error reading font file: %v", err)
	}

	var ttfFont *opentype.Font
	// Handle TrueType Collections (.ttc files)
	if strings.HasSuffix(fontPath, ".ttc") {
		collection, err := opentype.ParseCollection(fontBytes)
		if err != nil {
			return nil, fmt.Errorf("error parsing font collection: %v", err)
		}
		// Get the first font from the collection
		ttfFont, err = collection.Font(0)
		if err != nil {
			return nil, fmt.Errorf("error getting font from collection: %v", err)
		}
	} else {
		// Handle single font files (.ttf, .otf)
		ttfFont, err = opentype.Parse(fontBytes)
		if err != nil {
			return nil, fmt.Errorf("error parsing font: %v", err)
		}
	}

	parsedFontCacheMu.Lock()
	parsedFontCache[fontPath] = ttfFont
	parsedFontCacheMu.Unlock()
	return ttfFont, nil
}

// preloadFonts warms every configured font face so the first render that
// needs one (e.g. CJK text arriving from the modem or an SMS) never pays the
// file read + parse cost inside the frame loop. Run it in a goroutine.
func preloadFonts() {
	start := time.Now()
	for name := range fonts {
		if _, _, err := getFontFace(name); err != nil {
			log.Printf("preloadFonts: %s: %v", name, err)
		}
	}
	log.Printf("Fonts preloaded in %.0fms", durationToMs(time.Since(start)))
}

// getFontFace loads (or returns cached) font.Face + its height.
func getFontFace(fontName string) (font.Face, int, error) {
	// 1) Check cache
	fontCacheMu.Lock()
	if entry, ok := fontCache[fontName]; ok {
		fontCacheMu.Unlock()
		return entry.face, entry.fontHeight, nil
	}
	fontCacheMu.Unlock()

	// 2) Not cached: load config
	cfg, ok := fonts[fontName]
	if !ok {
		return nil, 0, fmt.Errorf("font %s not found in mapping", fontName)
	}

	// 3) Read & parse the TTF/TTC (cached by path)
	ttfFont, err := parseFontFile(cfg.FontPath)
	if err != nil {
		return nil, 0, err
	}

	// 4) Create the face
	// HintingNone: softest glyph edges (max AA fringe). golang.org/x/image
	// has no HintingSlight — only None / Vertical / Full.
	face, err := opentype.NewFace(ttfFont, &opentype.FaceOptions{
		Size:    cfg.FontSize,
		DPI:     72,
		Hinting: font.HintingNone,
	})
	if err != nil {
		return nil, 0, err
	}

	// 5) Measure height
	metrics := face.Metrics()
	fontHeight := metrics.Ascent.Round() + metrics.Descent.Round()

	// 6) Store in cache
	fontCacheMu.Lock()
	fontCache[fontName] = struct {
		face       font.Face
		fontHeight int
	}{face: face, fontHeight: fontHeight}
	fontCacheMu.Unlock()

	return face, fontHeight, nil
}

// containsChinese checks if a string contains Chinese characters
func containsChinese(text string) bool {
	for _, r := range text {
		if unicode.In(r, unicode.Han) {
			return true
		}
	}
	return false
}

// getFontFaceForText returns the appropriate font face based on text content
func getFontFaceForText(baseFontName string, text string) (font.Face, int, error) {
	if containsChinese(text) {
		// Use Chinese font variant if available
		cjkFontName := baseFontName + "_cjk"
		return getFontFace(cjkFontName)
	}
	// Use regular font for non-Chinese text
	return getFontFace(baseFontName)
}

// Pre-allocated clear buffer for efficient frame clearing
var clearBuffer []uint8
var clearBufferColor color.RGBA = PCAT_BLACK

// screenBgColor is the background frames are cleared to. Defaults to black;
// set from display_template.bg_color on every config (re)merge.
var screenBgColor color.RGBA = PCAT_BLACK

// setScreenBgColor updates the clear color used by clearFrame. Accepts an
// [R,G,B] slice from the config; anything else resets to black. A change
// invalidates the top bar / footer frame caches — their content strings don't
// cover the background, so without this they would keep their old-color
// frames until the clock or page happened to change.
func setScreenBgColor(rgb []int) {
	c := PCAT_BLACK
	if len(rgb) >= 3 {
		c = color.RGBA{uint8(rgb[0]), uint8(rgb[1]), uint8(rgb[2]), 255}
	}
	if c != screenBgColor {
		screenBgColor = c
		cacheTopBarStr = ""
		cacheFooterStr = ""
	}
}

func clearFrame(frame *image.RGBA, width int, height int) {
	pixelsNeeded := width * height * 4

	// (Re)initialize the clear buffer when it is too small or the background
	// color changed since it was filled.
	if len(clearBuffer) < pixelsNeeded || clearBufferColor != screenBgColor {
		// Allocate larger buffer to handle future larger frames
		bufferSize := max(pixelsNeeded, PCAT2_LCD_WIDTH*PCAT2_LCD_HEIGHT*4)
		clearBuffer = make([]uint8, bufferSize)
		pattern := []uint8{screenBgColor.R, screenBgColor.G, screenBgColor.B, 255}
		for i := 0; i < len(clearBuffer); i += 4 {
			copy(clearBuffer[i:i+4], pattern)
		}
		clearBufferColor = screenBgColor
	}

	// Ensure frame buffer is correct size
	if len(frame.Pix) < pixelsNeeded {
		frame.Pix = make([]uint8, pixelsNeeded)
	}

	// Fast bulk copy instead of pixel-by-pixel clearing
	copy(frame.Pix[:pixelsNeeded], clearBuffer[:pixelsNeeded])
}

// Helper function for max since Go doesn't have built-in max for int

// preCalculateEasing pre-computes easing values to avoid math.Pow during transitions
func preCalculateEasing(numFrames int, frameWidth int) []int {
	if len(easingLookup) != numFrames {
		easingLookup = make([]int, numFrames)
		for i := 0; i < numFrames; i++ {
			t := float64(i) / float64(numFrames)
			et3 := 1 - math.Pow(1-t, 4) // Quartic easing
			easingLookup[i] = int(et3 * float64(frameWidth))
		}
	}
	return easingLookup
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func setBacklight(brightness int) {
	mu.Lock()
	defer mu.Unlock()

	// Get effective max brightness (runtime override or config)
	effectiveMaxBrightness := getEffectiveMaxBrightness()

	// clamp into 0..effectiveMaxBrightness
	switch {
	case brightness < cfg.ScreenMinBrightness:
		brightness = cfg.ScreenMinBrightness
	case brightness > effectiveMaxBrightness:
		brightness = effectiveMaxBrightness
	}

	if brightness == lastLogical {
		return
	}
	lastLogical = brightness

	// cancel any pending off-timer if we're going to >0
	if brightness > 0 && offTimer != nil {
		offTimer.Stop()
		offTimer = nil
	}

	// PWM backlight now supports a true physical 0, so logical brightness maps
	// directly to the physical value (no more forcing 0 → 1).
	phys := brightness

	// perform the write
	if err := os.WriteFile(utilCovBacklightPath, []byte(strconv.Itoa(phys)), 0644); err != nil {
		log.Printf("backlight write error: %v", err)
	} else {
		//log.Printf("→ physical backlight %d", phys)
	}

	// if we just handled a logical “0”, confirm the real off after
	// ZERO_BACKLIGHT_DELAY s (guards against a transient 0 during transitions)
	if brightness == 0 {
		offTimer = time.AfterFunc(ZERO_BACKLIGHT_DELAY, func() {
			mu.Lock()
			defer mu.Unlock()
			if lastLogical == 0 {
				// still supposed to be off, so write 0 now
				if err := os.WriteFile(utilCovBacklightPath, []byte("0"), 0644); err != nil {
					log.Printf("backlight final-off error: %v", err)
				} else {
					log.Println("→ physical backlight OFF")
				}
			}
		})
	}
}

// displayAsleep reports whether the display is settled dark: idle state
// reached AND the backlight physically at 0 (screen_min_brightness may keep
// it visibly dim, in which case this stays false). While asleep the render
// loop pauses SPI pushes to save battery; data collectors still run on their
// 1-minute cadence so wake shows fresh values.
func displayAsleep() bool {
	if idleState != STATE_IDLE {
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	return lastLogical == 0
}

// displayWake forces an immediate refresh of time-critical UI data so the
// first frames after backlight-on already show the correct clock and battery
// instead of waiting for the next 1-minute collector tick.
func displayWake() {
	// Battery SOC/charging feeds the top bar; do it synchronously so the
	// forced top-bar redraw below has current numbers.
	collectBatteryData()
	// Invalidate top-bar cache and force a redraw on the next active frame.
	cacheTopBarStr = ""
	forceTopBarRedraw = true
	// Middle may still hold the last pre-sleep pixels; force a full redraw so
	// wake never shows a stale fingerprint match.
	forceMiddleRedraw = true
	// Kick the rest in the background (1-minute collectors already keep most
	// keys warm; this just covers the case where we wake just before a tick).
	go collectNetworkData(cfg)
	go collectWANNetworkSpeed()
	go getInfoFromPcatWeb()
	go collectLinkStatus()
	// Page-sensitive collectors refresh immediately on reschedule.
	signalPingReschedule()
	signalLinuxReschedule()
	signalLinkReschedule()
}

func monitorKeyboard(changePageTriggered *bool) {
	// 1) find the "rk805 pwrkey" device by name
	paths, err := evdev.ListDevicePaths()
	if err != nil {
		log.Printf("ListDevicePaths error: %v", err)
		return
	}

	var devPath string
	for _, ip := range paths {
		if ip.Name == "rk805 pwrkey" {
			devPath = ip.Path
			break
		}
	}
	if devPath == "" {
		log.Println("no EV_KEY device found")
		return
	}

	// 2) open it
	keyboard, err := evdev.Open(devPath)
	if err != nil {
		log.Printf("Open(%s) error: %v", devPath, err)
		return
	}
	defer keyboard.Ungrab()

	// 3) grab for exclusive access
	if err := keyboard.Grab(); err != nil {
		log.Printf("warning: failed to grab device: %v", err)
	}

	// 4) log what we opened
	name, _ := keyboard.Name()
	log.Printf("using input device: %s (%s)", devPath, name)

	for {
		ev, err := keyboard.ReadOne()
		if err != nil {
			log.Printf("read error: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		now := time.Now()
		if ev.Type == evdev.EV_KEY && ev.Code == evdev.KEY_POWER {
			switch ev.Value {
			case 1: // key press
				buttonKeydownTime = now // Record button press timing
				if showDetailedTiming {
					log.Printf("⏱️  POWER pressed (key down) at +0.0ms, checking state = %s", stateName(idleState))
				}
				lastActivityMu.Lock()
				lastActivity = now
				lastActivityMu.Unlock()

				if idleState == STATE_IDLE || idleState == STATE_OFF || idleState == STATE_FADE_OUT {
					log.Println("Screen is idle/fading/off, preparing to wake up without changing page")
					wasScreenIdle = true
				} else if idleState == STATE_ACTIVE || idleState == STATE_FADE_IN {
					log.Println("Screen is active, preparing for page change")
					wasScreenIdle = false
					swippingScreen = true
					*changePageTriggered = true
					signalPageChange()
				}

			case 0: // key release
				buttonKeyupTime = now // Record button release timing
				var keyPressDurationMs float64
				if !buttonKeydownTime.IsZero() {
					keyPressDurationMs = durationToMs(now.Sub(buttonKeydownTime))
				}
				if showDetailedTiming {
					log.Printf("⏱️  POWER released (key up) +%.1fms after keydown, triggering animation if ready, state = %s",
						keyPressDurationMs, stateName(idleState))
				}
				if wasScreenIdle {
					log.Println("Screen was idle when key was pressed, waking up without changing page")
					wasScreenIdle = false // Reset flag
				} else if idleState == STATE_ACTIVE || idleState == STATE_FADE_IN {
					//*changePageTriggered = true
				}
				// just update lastActivity
				lastActivityMu.Lock()
				lastActivity = now
				lastActivityMu.Unlock()

				/* //var lastKeyPress time.Time
				   case 1: // key press
				       log.Println("POWER pressed, state =", stateName(idleState))
				       if idleState == STATE_ACTIVE || idleState == STATE_FADE_IN {
				           swippingScreen = true
				           //*changePageTriggered = true
				       }
				       lastActivityMu.Lock()
				       lastActivity = now
				       lastActivityMu.Unlock()
				       lastKeyPress = now

				   case 0: // key release
				       // only trigger if it wasn't a quick tap (<500ms)
				       if now.Sub(lastKeyPress) > KEYBOARD_DEBOUNCE_TIME {
				           log.Println("POWER released, state =", stateName(idleState))
				           if idleState == STATE_ACTIVE{
				               swippingScreen = true
				               *changePageTriggered = true
				               signalPageChange()
				           }
				           lastActivityMu.Lock()
				           lastActivity = now
				           lastActivityMu.Unlock()
				       }*/
			}
		}
	}
}

func monitorConsoleInput(changePageTriggered *bool) {
	log.Println("Console input monitoring started. Press ENTER key to change screen.")

	for {
		var input string
		_, err := fmt.Scanln(&input)

		// Handle EOF or other input errors gracefully, but also treat empty input as valid
		if err != nil && err.Error() != "unexpected newline" {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Trigger on any input (including empty/just Enter key)
		utilCovConsoleEnter(changePageTriggered)
	}
}

// utilCovConsoleEnter handles a single ENTER keypress from the console
// monitor. Extracted from monitorConsoleInput (whose stdin read loop never
// returns) so the state handling is testable; behaviour is unchanged.
func utilCovConsoleEnter(changePageTriggered *bool) {
	now := time.Now()
	log.Printf("⌨️  KEYBOARD ENTER HIT (state: %s)", stateName(idleState))

	if idleState == STATE_IDLE || idleState == STATE_OFF || idleState == STATE_FADE_OUT {
		log.Println("Screen waking up")
		wasConsoleScreenIdle = true
	} else if idleState == STATE_ACTIVE || idleState == STATE_FADE_IN {
		if wasConsoleScreenIdle {
			log.Println("Screen already active, not changing page")
			wasConsoleScreenIdle = false // Reset flag
		} else {
			log.Println("Triggering page change")
			swippingScreen = true
			*changePageTriggered = true
			signalPageChange()
		}
	}
	lastActivityMu.Lock()
	lastActivity = now
	lastActivityMu.Unlock()
}

func getBacklight() int {
	data, err := os.ReadFile(utilCovBacklightPath)
	if err != nil {
		log.Printf("getBacklight error: %v", err)
		return 0
	}
	brightness, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		log.Printf("getBacklight parse error: %v", err)
		return 0
	}
	return brightness
}

func fadeBacklight(wantValue int, timePeriod time.Duration) {
	// Grab a snapshot of the “current” cancel channel under the fadeMu lock:
	fadeMu.Lock()
	cancelChan := fadeCancel
	fadeMu.Unlock()

	initValue := getBacklight()
	if timePeriod <= 0 || initValue == wantValue {
		setBacklight(wantValue)
		return
	}

	const stepDuration = 40 * time.Millisecond
	steps := int(timePeriod / stepDuration)
	if steps < 1 {
		setBacklight(wantValue)
		return
	}

	diff := wantValue - initValue
	ticker := time.NewTicker(stepDuration)
	defer ticker.Stop()

	for i := 1; i <= steps; i++ {
		select {
		case <-cancelChan:
			// Someone closed fadeCancel → abort immediately
			log.Println("fadeBacklight: cancel requested")
			return
		case <-ticker.C:
			frac := float64(i) / float64(steps)
			b := initValue + int(math.Round(frac*float64(diff)))
			setBacklight(b)
			log.Printf("fadeBacklight: step %d/%d → brightness=%d", i, steps, b)
		}
	}
	// final guarantee
	setBacklight(wantValue)
}

func idleDimmer() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	prevState := STATE_UNKNOWN

	for range ticker.C {
		// 1) Movement/keypress detection
		utilCovMovementKick()

		// 2) Compute idle time
		lastActivityMu.Lock()
		idle := time.Since(lastActivity)
		lastActivityMu.Unlock()

		newState := utilCovIdleStateFor(idle)

		if prevState != newState {
			log.Printf("STATE CHANGED: %s -> %s", stateName(prevState), stateName(newState))
			wasAsleep := prevState == STATE_IDLE
			idleState = newState
			prevState = newState

			// Leaving the dark-idle state: kick a refresh of the collectors
			// that were paused so the screen wakes up with fresh data. Must
			// run after idleState is updated or they would still see
			// STATE_IDLE and skip themselves.
			if wasAsleep && (newState == STATE_FADE_IN || newState == STATE_ACTIVE) {
				displayWake()
			}

			// Update intervals immediately when state changes
			updateIntervals()

			// ── Cancel any existing fade by closing fadeCancel ──────────
			fadeMu.Lock()
			if fadeCancel != nil {
				close(fadeCancel) // signal the currently running fadeBacklight (if any) to stop
				// allocate a brand‑new channel
			}
			fadeCancel = make(chan struct{})
			//myCancel := fadeCancel
			fadeMu.Unlock()

			switch newState {
			case STATE_OFF:
				fadeBacklight(10, OFF_TIMEOUT)
				os.Exit(0)

			case STATE_FADE_IN:
				if !swippingScreen {
					go fadeBacklight(maxBacklight, fadeInDur)
				}
			case STATE_FADE_OUT:
				go fadeBacklight(0, fadeDuration)

			case STATE_ACTIVE:
				setBacklight(maxBacklight)
				swippingScreen = false

			case STATE_IDLE:
				setBacklight(0)
			}
		}

	}
}

// utilCovMovementKick resets the idle timer when the photonicat PM movement
// trigger has fired. Extracted from idleDimmer (an endless ticker loop) so the
// trigger handling is testable; behaviour is unchanged.
func utilCovMovementKick() {
	data, err := os.ReadFile(utilCovMovementTriggerPath)
	if err == nil && strings.TrimSpace(string(data)) == "1" {
		// Reset idle timer, treat screen as already “on”
		now := time.Now()
		lastActivityMu.Lock()
		tempLastActivity := now.Add(-2 * time.Second)
		if tempLastActivity.After(lastActivity) {
			lastActivity = tempLastActivity
		}
		lastActivityMu.Unlock()
	}
}

// utilCovIdleStateFor maps the current idle duration onto a screen state.
// Extracted from idleDimmer so the state machine is testable; it clears
// swippingScreen exactly where the inline switch did.
func utilCovIdleStateFor(idle time.Duration) int {
	switch {
	case !weAreRunning():
		return STATE_OFF
	case idle < fadeInDur:
		if swippingScreen {
			return STATE_ACTIVE
		}
		return STATE_FADE_IN
	case idle < idleTimeout:
		swippingScreen = false
		return STATE_ACTIVE
	case idle < idleTimeout+fadeDuration:
		return STATE_FADE_OUT
	default:
		return STATE_IDLE
	}
}

func stateName(s int) string {
	switch s {
	case STATE_FADE_IN:
		return "FADE_IN"
	case STATE_ACTIVE:
		return "ACTIVE"
	case STATE_FADE_OUT:
		return "FADE_OUT"
	case STATE_IDLE:
		return "IDLE"
	case STATE_OFF:
		return "OFF"
	default:
		return "UNKNOWN"
	}
}

// deepMergeJSON performs a deep merge of two JSON objects.
// For objects: recursively merge keys from src into dst
// For arrays: append src arrays to dst arrays
// For primitives: src overrides dst (unless src is zero value for numeric fields)
func deepMergeJSON(dst, src map[string]interface{}) map[string]interface{} {
	return deepMergeJSONOpt(dst, src, false)
}

// deepMergeJSONOpt is the merge implementation. When replaceArrays is true, src
// arrays REPLACE dst arrays instead of appending. This is required under the
// display_template subtree, where the visual editor / user_config carries the
// complete page element layout (page0..page3) and must override the defaults
// rather than be appended to them (which would duplicate every element).
func deepMergeJSONOpt(dst, src map[string]interface{}, replaceArrays bool) map[string]interface{} {
	for key, srcVal := range src {
		// Skip zero/empty values from src for certain fields to preserve dst defaults
		if srcVal == nil {
			continue
		}

		// Skip zero numeric values for these fields - a zero here is almost
		// always an unset/malformed value, so preserve the dst default instead.
		// Note: screen_min_brightness is intentionally NOT in this list — 0 is a
		// valid, user-selectable value (backlight fully off when idle).
		if key == "screen_max_brightness" ||
			key == "screen_dimmer_time_on_battery_seconds" || key == "screen_dimmer_time_on_dc_seconds" {
			if numVal, ok := srcVal.(float64); ok && numVal == 0 {
				continue // Don't override with zero value
			}
		}

		// Within (and below) the display_template and custom_metrics subtrees,
		// arrays must replace the defaults rather than append to them: both
		// carry complete user-authored lists (page elements / metric sources)
		// that would otherwise be duplicated against shipped defaults.
		childReplaceArrays := replaceArrays || key == "display_template" || key == "custom_metrics" || key == "public_ip_lookup"

		if dstVal, exists := dst[key]; exists {
			// Key exists in both - need to merge
			srcMap, srcIsMap := srcVal.(map[string]interface{})
			dstMap, dstIsMap := dstVal.(map[string]interface{})

			if srcIsMap && dstIsMap {
				// Both are objects - recursively merge
				dst[key] = deepMergeJSONOpt(dstMap, srcMap, childReplaceArrays)
			} else if srcSlice, srcIsSlice := srcVal.([]interface{}); srcIsSlice {
				if dstSlice, dstIsSlice := dstVal.([]interface{}); dstIsSlice {
					if replaceArrays {
						// Inside display_template - REPLACE the layout array.
						dst[key] = srcSlice
						log.Printf("JSON merge: Replaced array '%s' with %d elements", key, len(srcSlice))
					} else {
						// Both are arrays - APPEND src to dst
						dst[key] = append(dstSlice, srcSlice...)
						log.Printf("JSON merge: Appended %d elements to array '%s' (total: %d)",
							len(srcSlice), key, len(dstSlice)+len(srcSlice))
					}
				} else {
					// Type mismatch - src overrides
					dst[key] = srcVal
				}
			} else {
				// Primitive or type mismatch - src overrides
				dst[key] = srcVal
			}
		} else {
			// Key only in src - just copy
			dst[key] = srcVal
		}
	}
	return dst
}

// mergeConfigs rebuilds `cfg` by overlaying userCfg on top of dftCfg using JSON deep merge.
// It returns an error if any validation fails.
func mergeConfigs() error {
	configMutex.Lock()
	defer configMutex.Unlock()

	// Convert both configs to JSON
	dftJSON, err := json.Marshal(dftCfg)
	if err != nil {
		return fmt.Errorf("failed to marshal default config: %v", err)
	}

	userJSON, err := json.Marshal(userCfg)
	if err != nil {
		return fmt.Errorf("failed to marshal user config: %v", err)
	}

	// Parse to generic maps
	var dftMap map[string]interface{}
	var userMap map[string]interface{}

	if err := json.Unmarshal(dftJSON, &dftMap); err != nil {
		return fmt.Errorf("failed to unmarshal default config: %v", err)
	}

	if err := json.Unmarshal(userJSON, &userMap); err != nil {
		return fmt.Errorf("failed to unmarshal user config: %v", err)
	}

	// Deep merge user config into default config
	mergedMap := deepMergeJSON(dftMap, userMap)

	// Convert back to Config struct
	mergedJSON, err := json.Marshal(mergedMap)
	if err != nil {
		return fmt.Errorf("failed to marshal merged config: %v", err)
	}

	if err := json.Unmarshal(mergedJSON, &cfg); err != nil {
		return fmt.Errorf("failed to unmarshal merged config: %v", err)
	}

	log.Println("JSON deep merge completed successfully")

	// Default ping sites if missing or too short to be valid
	if len(cfg.PingSite0) <= 2 {
		cfg.PingSite0 = "1.1.1.1"
	}
	if len(cfg.PingSite1) <= 2 {
		cfg.PingSite1 = "photonicat.com"
	}

	// Ping types: unknown/missing values normalize to ICMP so a stale or
	// hand-edited user_config can never turn a row off.
	cfg.PingType0 = normalizePingType(cfg.PingType0)
	cfg.PingType1 = normalizePingType(cfg.PingType1)

	// config_version describes the shipped default config's schema, so the
	// value from config.json always wins over whatever an old user_config
	// (saved by an earlier firmware's web UI) still carries.
	cfg.ConfigVersion = dftCfg.ConfigVersion

	// 5. Validation
	if cfg.ScreenDimmerTimeOnBatterySeconds < 0 {
		return fmt.Errorf("screen_dimmer_time_on_battery_seconds must be ≥ 0, got %d",
			cfg.ScreenDimmerTimeOnBatterySeconds)
	}
	if cfg.ScreenDimmerTimeOnDCSeconds < 0 {
		return fmt.Errorf("screen_dimmer_time_on_dc_seconds must be ≥ 0, got %d",
			cfg.ScreenDimmerTimeOnDCSeconds)
	}
	if cfg.ScreenMinBrightness < 0 || cfg.ScreenMinBrightness > 100 {
		return fmt.Errorf("screen_min_brightness must be in [0,100], got %d",
			cfg.ScreenMinBrightness)
	}
	if cfg.ScreenMaxBrightness < 0 || cfg.ScreenMaxBrightness > 100 {
		return fmt.Errorf("screen_max_brightness must be in [0,100], got %d",
			cfg.ScreenMaxBrightness)
	}
	if cfg.ScreenMinBrightness > cfg.ScreenMaxBrightness {
		return fmt.Errorf("screen_min_brightness (%d) cannot exceed screen_max_brightness (%d)",
			cfg.ScreenMinBrightness, cfg.ScreenMaxBrightness)
	}
	/*
	   for name, site := range map[string]string{"ping_site0": cfg.PingSite0, "ping_site1": cfg.PingSite1} {
	       if site != "" {
	           if u, err := url.ParseRequestURI(site); err != nil || u.Scheme == "" && u.Host == "" {
	               return fmt.Errorf("invalid %s: %q", name, site)
	           }
	       }
	   }*/

	cfgNumPages = len(cfg.DisplayTemplate.Elements)

	// Apply the (possibly user-themed) background color to the frame clearers.
	setScreenBgColor(cfg.DisplayTemplate.BgColor)

	// Bump the config version so middle-page fingerprints from before this
	// merge never match: element colors are not part of the fingerprint, so a
	// pure recolor (theme change) on a static page would otherwise be skipped.
	configVersion++

	// SMS pages are pre-rasterised with the theme palette baked in, so the
	// bumped configVersion alone only invalidates them — this kicks the SMS
	// goroutine to actually redraw now instead of up to a minute later.
	signalSmsRerender()

	// The GPS page (last pageN, Gps* data keys) starts hidden; the GPS
	// collector reveals it when pcat-manager-web reports the feature enabled.
	detectGpsPage()

	// Wi-Fi QR pages follow the template pages; re-added here because
	// cfgNumPages was just rebuilt from the template (and the show_wifi_qr
	// toggle may have changed).
	refreshWifiQrPagesLocked()

	// Initialize totalNumPages based on ShowSms setting
	if cfg.ShowSms {
		// Will be updated by getSmsPages() goroutine
		totalNumPages = cfgNumPages + 1 // temporary, will be corrected when SMS data is loaded
	} else {
		totalNumPages = cfgNumPages
	}

	return nil
}

// hasShowSmsInUserConfig checks if the user config file explicitly contains show_sms field
func hasShowSmsInUserConfig() bool {
	var userConfigPath string
	localUserConfig := utilCovLocalUserConfigPath

	// Determine which user config file to check
	if _, err := os.Stat(localUserConfig); err == nil {
		userConfigPath = localUserConfig
	} else {
		userConfigPath = utilCovEtcUserConfigPath
	}

	// Read the raw JSON
	raw, err := os.ReadFile(userConfigPath)
	if err != nil {
		return false
	}

	// Parse into a generic map to check for presence of show_sms
	var rawMap map[string]interface{}
	if err := secureUnmarshal(raw, &rawMap); err != nil {
		return false
	}

	_, exists := rawMap["show_sms"]
	return exists
}

// embeddedDefaultConfigJSON returns the package-default layout baked into the
// binary (OpenWrt vs Debian). This is the source of truth for every release.
func embeddedDefaultConfigJSON() []byte {
	if !utilCovIsOpenWRT() {
		return embeddedDebianConfigJSON
	}
	return embeddedOpenWrtConfigJSON
}

// defaultConfigDiskPath is where we mirror the embedded default so operators
// (and older tools) can still read a file under /etc.
func defaultConfigDiskPath() string {
	if !utilCovIsOpenWRT() {
		return utilCovEtcDebianConfigPath
	}
	return utilCovEtcConfigPath
}

// loadConfigFromBytes unmarshals a Config from raw JSON.
func loadConfigFromBytes(data []byte) (Config, error) {
	var c Config
	if err := secureUnmarshal(data, &c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// loadAllConfigsToVariables loads the package default (embedded in the binary),
// refreshes the on-disk default under /etc, then merges user overrides from
// user_config.json (or /etc/...-user_config.json) on top.
//
// Order: embedded default → write /etc default → load user → merge → cfg.
// Re-running this (e.g. after the web editor saves) always re-reads the
// embedded default so a previous merge cannot pollute the next one.
func loadAllConfigsToVariables() {
	// 1) Package default always comes from the binary (version-locked).
	defBytes := embeddedDefaultConfigJSON()
	var err error
	dftCfg, err = loadConfigFromBytes(defBytes)
	if err != nil {
		log.Fatalf("Failed to parse embedded default config: %v", err)
	}
	log.Println("DEFAULT CFG: loaded from embedded package default")

	// 2) Mirror default to /etc so the file on disk always matches this binary.
	//    Never touches user_config — only the system default path.
	diskPath := defaultConfigDiskPath()
	if err := os.WriteFile(diskPath, defBytes, 0644); err != nil {
		// Non-fatal: e.g. read-only root during unit tests / non-root dev runs.
		log.Printf("could not refresh default config at %s: %v (using embedded only)", diskPath, err)
	} else {
		log.Printf("DEFAULT CFG: wrote package default to %s", diskPath)
	}
	localConfigExists = true // we always have a default now

	// 3) User overrides: cwd user_config.json wins for local dev, else /etc.
	userPath := ""
	if _, err := os.Stat(utilCovLocalUserConfigPath); err == nil {
		userPath = utilCovLocalUserConfigPath
		log.Println("User config found at", userPath)
	} else if _, err := os.Stat(utilCovEtcUserConfigPath); err == nil {
		userPath = utilCovEtcUserConfigPath
		log.Println("User config found at", userPath)
	} else {
		// Ensure an empty user config exists under /etc for the web editor.
		if err := os.WriteFile(utilCovEtcUserConfigPath, []byte("{}\n"), 0644); err != nil {
			log.Printf("could not create empty user config: %v", err)
		} else {
			log.Println("Created empty user config at", utilCovEtcUserConfigPath)
		}
		userPath = utilCovEtcUserConfigPath
	}

	userCfg, err = loadConfig(userPath)
	if err != nil {
		log.Printf("USER CFG: read failed (%v), using empty overrides", err)
		userCfg = Config{}
	} else {
		log.Println("USER CFG: READ SUCCESS")
	}

	// 4) cfg = deep-merge(default, user). display_template arrays in user
	//    fully replace the matching pages (visual editor owns layout).
	if err := mergeConfigs(); err != nil {
		log.Printf("MERGE CFG failed: %v — falling back to package default only", err)
		cfg = dftCfg
	} else {
		log.Println("MERGE CFG: SUCCESS (user overrides on top of package default)")
	}
}

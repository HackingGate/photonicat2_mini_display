package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
)

// validateJSON validates that input is proper JSON and doesn't contain suspicious content
func validateJSON(data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("empty JSON data")
	}
	if len(data) > 10*1024*1024 { // 10MB limit
		return fmt.Errorf("JSON data too large")
	}

	// Basic JSON validation
	var temp interface{}
	if err := json.Unmarshal(data, &temp); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	// Check for potentially malicious patterns
	dataStr := string(data)
	suspiciousPatterns := []string{
		"<script", "</script", "javascript:", "eval(", "document.", "window.",
	}
	for _, pattern := range suspiciousPatterns {
		if strings.Contains(strings.ToLower(dataStr), pattern) {
			return fmt.Errorf("suspicious content detected")
		}
	}

	return nil
}

// secureUnmarshal safely unmarshals JSON with validation
func secureUnmarshal(data []byte, v interface{}) error {
	if err := validateJSON(data); err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

var (
	drawMu         sync.Mutex
	configMutex    sync.RWMutex
	defaultConfig  Config                 // loaded from default_config.json
	userOverrides  map[string]interface{} // raw overrides from user_config.json
	userJsonConfig = ""

	// Runtime brightness override system
	runtimeBrightnessMu  sync.RWMutex
	runtimeMaxBrightness *int // nil = use config.json, non-nil = override value

	// Web preview snapshot: only maintained while the screen page is polling.
	// HTTP never reads the live double-buffers (those are mid-draw and race).
	// The render loop publishes a fully composed frame after each complete
	// send — zero cost when nobody is watching (webFrameDemandUntil expired).
	webSnapMu            sync.RWMutex
	webSnapshot          *image.RGBA // last complete full-LCD image
	webSnapCompose       *image.RGBA // scratch used only on the render thread
	webFrameDemandMu     sync.Mutex
	webFrameDemandUntil  time.Time
	webLastTop           *image.RGBA // last fully drawn top/middle/footer (ptrs into double buffers; only read from the render thread at publish time)
	webLastMiddle        *image.RGBA
	webLastFooter        *image.RGBA
	webFrameDemandWindow = 3 * time.Second // keep publishing this long after last HTTP hit
)

// Testability seams: the handlers below hardcode filesystem paths and the PMU
// power-off call. Tests point these at temp dirs / stubs; production defaults
// are identical to the previous literals, so behavior is unchanged.
var (
	httpCovUserConfigPath    = ETC_USER_CONFIG_PATH      // user config read/written by config handlers
	httpCovWebUserConfigPath = "config/user_config.json" // overrides persisted by setUserConfig/setConfig
	httpCovPmuPowerOff       = pmuPowerOff               // seam so tests never power the box off
)

// noteWebFrameDemand marks that a browser is requesting preview frames so the
// render loop will start publishing complete snapshots.
func noteWebFrameDemand() {
	webFrameDemandMu.Lock()
	webFrameDemandUntil = time.Now().Add(webFrameDemandWindow)
	webFrameDemandMu.Unlock()
}

func webFrameDemanded() bool {
	webFrameDemandMu.Lock()
	defer webFrameDemandMu.Unlock()
	return time.Now().Before(webFrameDemandUntil)
}

// rememberWebRegion stores the latest fully-drawn region pointer. Called only
// from the single-threaded render path after a region is finished.
func rememberWebRegion(kind string, frame *image.RGBA) {
	if frame == nil {
		return
	}
	switch kind {
	case "top":
		webLastTop = frame
	case "middle":
		webLastMiddle = frame
	case "footer":
		webLastFooter = frame
	}
}

// tryPublishWebSnapshot composes top+middle+footer into a complete LCD image
// for HTTP. No-op when the web UI is not polling — normal display path pays
// nothing. Safe to call only from the render thread (uses region ptrs without
// locking them; the publish copy is protected by webSnapMu).
func tryPublishWebSnapshot() {
	// Seed the very first snapshot even with no browser watching: without it,
	// the first /screen page load races the render loop — serveFrame only
	// waits 500ms, and a static (frame-skipped) or dimmed screen may not
	// publish in that window, so the page landed on a broken image until a
	// click forced a redraw. webSnapshot is only ever assigned on this
	// (single) render thread, so the nil check is race-free.
	if !webFrameDemanded() && webSnapshot != nil {
		return
	}
	top, mid, foot := webLastTop, webLastMiddle, webLastFooter
	if top == nil || mid == nil || foot == nil {
		return
	}

	if webSnapCompose == nil {
		webSnapCompose = image.NewRGBA(image.Rect(0, 0, PCAT2_LCD_WIDTH, PCAT2_LCD_HEIGHT))
	}
	// Compose offline (render thread only) then swap under a short lock.
	_ = copyImageToImageAt(webSnapCompose, top, 0, 0)
	_ = copyImageToImageAt(webSnapCompose, mid, 0, PCAT2_TOP_BAR_HEIGHT)
	_ = copyImageToImageAt(webSnapCompose, foot, 0, PCAT2_LCD_HEIGHT-PCAT2_FOOTER_HEIGHT)

	webSnapMu.Lock()
	if webSnapshot == nil {
		webSnapshot = image.NewRGBA(image.Rect(0, 0, PCAT2_LCD_WIDTH, PCAT2_LCD_HEIGHT))
	}
	copy(webSnapshot.Pix, webSnapCompose.Pix)
	webSnapMu.Unlock()
}

func serveFrame(c *fiber.Ctx) error {
	// Tell the render loop we want snapshots; it will publish after the next
	// complete frame(s) with no work when demand expires.
	noteWebFrameDemand()

	// Wait briefly for a complete snapshot if none yet (first open of /screen).
	var snapPix []byte
	var stride int
	var rect image.Rectangle
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		webSnapMu.RLock()
		if webSnapshot != nil && len(webSnapshot.Pix) > 0 {
			// Copy under RLock so encode never races a publish, then unlock
			// before the (slow) PNG encode.
			snapPix = make([]byte, len(webSnapshot.Pix))
			copy(snapPix, webSnapshot.Pix)
			stride = webSnapshot.Stride
			rect = webSnapshot.Rect
			webSnapMu.RUnlock()
			break
		}
		webSnapMu.RUnlock()
		if time.Now().After(deadline) {
			return c.Status(fiber.StatusServiceUnavailable).SendString("No complete frame available yet")
		}
		time.Sleep(25 * time.Millisecond)
	}

	img := &image.RGBA{Pix: snapPix, Stride: stride, Rect: rect}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return c.Status(fiber.StatusInternalServerError).SendString("Failed to encode image")
	}

	c.Set("Content-Type", "image/png")
	c.Set("Cache-Control", "no-store")
	c.Set("Content-Length", strconv.Itoa(buf.Len()))
	return c.Send(buf.Bytes())
}

// simple index
func indexHandler(c *fiber.Ctx) error {
	return c.SendFile("assets/html/index.html")
}

// GET  /api/v1/changePage
func changePage(c *fiber.Ctx) error {
	lastActivityMu.Lock()
	httpChangePageTriggered = true
	lastActivity = time.Now() // Set to current time to avoid triggering fade-in
	lastActivityMu.Unlock()

	// Signal the main loop to interrupt FPS sleep
	signalPageChange()

	// Set swippingScreen to prevent backlight fade-in during HTTP page changes
	swippingScreen = true

	// Invalidate pre-calculated data since page is changing via HTTP
	// invalidatePreCalculatedData() // Function temporarily disabled

	return c.JSON(fiber.Map{"status": "page change triggered"})
}

// GET  /api/v1/data.json
func getData(c *fiber.Ctx) error {
	// 1) Build a plain map from the sync.Map
	out := make(map[string]interface{})

	globalData.Range(func(key, value interface{}) bool {
		// assume your keys are strings
		if ks, ok := key.(string); ok {
			out[ks] = value
		}
		return true // continue iteration
	})

	// 2) Return that map as JSON
	return c.JSON(out)
}

// loadUserConfig reads existing file into globalData
func loadUserConfig() string {
	if userJsonConfig != "" {
		return userJsonConfig
	}
	path := httpCovUserConfigPath
	raw, err := os.ReadFile(path)

	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("no existing user config at %s, starting fresh", path)
		} else {
			log.Printf("error reading user config: %v", err)
		}
		userJsonConfig = "{}"
		userCfg = Config{}
		return "{}"
	}

	var m map[string]string
	if err := secureUnmarshal(raw, &m); err != nil {
		log.Printf("error parsing user config JSON: %v", err)
		userJsonConfig = "{}"
		userCfg = Config{}
		return "{}"
	}

	for k, v := range m {
		globalData.Store(k, v)
	}
	log.Printf("loaded %d entries from user config", len(m))
	userJsonConfig = string(raw)
	return userJsonConfig
}

// saveUserConfigToFile writes the userCfg struct to ETC_USER_CONFIG_PATH atomically.
// Returns true on success, false on any error.
func saveUserConfigToFile() bool {
	// 1) Marshal with indentation
	data, err := json.MarshalIndent(userCfg, "", "  ")
	if err != nil {
		log.Printf("could not marshal user config: %v", err)
		return false
	}

	// 2) Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(httpCovUserConfigPath), 0755); err != nil {
		log.Printf("could not create config dir: %v", err)
		return false
	}

	// 3) Write to temp file
	tmpPath := httpCovUserConfigPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		log.Printf("could not write temp user config: %v", err)
		return false
	}

	// 4) Rename temp file into place
	if err := os.Rename(tmpPath, httpCovUserConfigPath); err != nil {
		log.Printf("could not rename temp config file: %v", err)
		return false
	}

	return true
}

// saveUserConfigFromStr validates the JSON string, pretty‑prints it,
// updates userCfg, and saves it. Returns true on success, false on any error.
func saveUserConfigFromStr(str string) bool {
	// 1) Validate & parse JSON into generic interface
	var obj interface{}
	if err := secureUnmarshal([]byte(str), &obj); err != nil {
		log.Printf("invalid JSON, not saving: %v", err)
		return false
	}

	// 2) Pretty‑print with 4‑space indent
	pretty, err := json.MarshalIndent(obj, "", "    ")
	if err != nil {
		log.Printf("could not marshal indent JSON: %v", err)
		return false
	}

	// 3) Write the prettified JSON to disk atomically
	//    (we skip updating userCfg here since you may not have a struct to unmarshal into;
	//     if you do, unmarshal into it before step 1 and assign to userCfg)
	tmpPath := httpCovUserConfigPath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(httpCovUserConfigPath), 0755); err != nil {
		log.Printf("could not create config dir: %v", err)
		return false
	}
	if err := os.WriteFile(tmpPath, pretty, 0644); err != nil {
		log.Printf("could not write temp config: %v", err)
		return false
	}
	if err := os.Rename(tmpPath, httpCovUserConfigPath); err != nil {
		log.Printf("could not rename temp config into place: %v", err)
		return false
	}

	// Reflect the change in the running display immediately (e.g. layout edits
	// from the web visual editor) without requiring a service restart. We
	// re-read BOTH default and user configs from disk and re-merge via the same
	// path used at startup. Re-reading the default each time is what keeps the
	// merge idempotent: a deep merge mutates its destination map, so reusing an
	// in-memory default would let a previous layout leak into the next merge
	// (e.g. resetting user config to "{}" would not revert to defaults).
	userJsonConfig = "" // invalidate cached raw string
	loadAllConfigsToVariables()
	log.Println("user config saved and re-merged into running config")

	return true
}

// POST /api/v1/data
func updateData(c *fiber.Ctx) error {
	// 1. Parse the JSON body into a map[string]string
	var payload map[string]string
	if err := c.BodyParser(&payload); err != nil {
		return c.
			Status(fiber.StatusBadRequest).
			JSON(fiber.Map{"error": "invalid JSON"})
	}

	// 2. Store each entry into the sync.Map
	for k, v := range payload {
		globalData.Store(k, v)
	}

	// 3. Return a success response
	return c.JSON(fiber.Map{"status": "ok"})
}

// getDefaultConfig returns the package default only (embedded layout), not the
// merged running config. The web editor uses this as the base before applying
// user overrides.
func getDefaultConfig(c *fiber.Ctx) error {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return c.JSON(dftCfg)
}

// GET /api/v1/get_user_config.json
func getUserConfig(c *fiber.Ctx) error {
	// 1) Read the file
	data, err := os.ReadFile(httpCovUserConfigPath)
	if err != nil {
		log.Printf("could not read user config: %v", err)
		return c.
			Status(fiber.StatusInternalServerError).
			JSON(fiber.Map{
				"status":  "error",
				"message": "unable to load user config",
			})
	}

	// 2) Return raw JSON, set content-type
	c.Type("application/json", "utf-8")
	return c.Send(data)
}

// saveUserConfigFromWeb handles a JSON payload, validates & saves it,
// and returns appropriate HTTP statuses.
func saveUserConfigFromWeb(c *fiber.Ctx) error {
	body := string(c.Body())

	// Attempt to save; this returns false on any validation/write error.
	if ok := saveUserConfigFromStr(body); !ok {
		// Distinguish “invalid JSON” vs “disk/write error”?
		// If you need that, change saveUserConfigFromStr to return (bool, error).
		// For now, we’ll lump all failures under 400 Bad Request.
		return c.Status(fiber.StatusBadRequest).
			JSON(fiber.Map{"status": "error", "message": "invalid JSON or unable to save config"})
	}

	// Success
	//loadAllConfigsToVariables() //TODO: this is probably not needed, but it's a good idea to reload the configs
	return c.JSON(fiber.Map{"status": "ok"})
}

// POST /api/v1/set_user_config.json
func setUserConfig(c *fiber.Ctx) error {
	// 1) Parse incoming JSON into a generic map
	var payload map[string]interface{}
	if err := c.BodyParser(&payload); err != nil {
		return c.
			Status(fiber.StatusBadRequest).
			JSON(fiber.Map{"error": "invalid JSON"})
	}

	// 2) Merge into the in-memory overrides under lock
	configMutex.Lock()
	userOverrides = deepMerge(userOverrides, payload)
	configMutex.Unlock()

	// 3) Persist back to disk
	raw, err := json.MarshalIndent(userOverrides, "", "  ")
	if err != nil {
		log.Printf("warning: could not marshal user_config.json: %v", err)
		return c.
			Status(fiber.StatusInternalServerError).
			JSON(fiber.Map{"error": "could not save config"})
	}
	if err := os.WriteFile(httpCovWebUserConfigPath, raw, 0644); err != nil {
		log.Printf("warning: could not write user_config.json: %v", err)
		return c.
			Status(fiber.StatusInternalServerError).
			JSON(fiber.Map{"error": "could not save config"})
	}

	// 4) Rebuild merged cfg
	mergeConfigs()

	return c.JSON(fiber.Map{"status": "ok"})
}

// GET /api/v1/get_config.json
func getConfig(c *fiber.Ctx) error {
	configMutex.RLock()
	defer configMutex.RUnlock()
	return c.JSON(cfg)
}

// POST /api/v1/set_config.json
func setConfig(c *fiber.Ctx) error {
	// Parse incoming JSON as generic map
	var payload map[string]interface{}
	if err := c.BodyParser(&payload); err != nil {
		return c.Status(fiber.StatusBadRequest).
			JSON(fiber.Map{"error": "invalid JSON"})
	}

	configMutex.Lock()
	defer configMutex.Unlock()

	// Merge new values into overrides
	userOverrides = deepMerge(userOverrides, payload)

	// Persist userOverrides back to disk
	if raw, err := json.MarshalIndent(userOverrides, "", "  "); err != nil {
		log.Printf("warning: could not marshal user_config.json: %v", err)
	} else if err := os.WriteFile(httpCovWebUserConfigPath, raw, 0644); err != nil {
		log.Printf("warning: could not write user_config.json: %v", err)
	}

	// Rebuild merged cfg
	mergeConfigs()

	return c.JSON(fiber.Map{"status": "ok"})
}

// deepMerge merges src into dest (in-place) for nested maps
// deepMerge merges src into dest (in-place) for nested maps, initializing dest if nil
func deepMerge(dest, src map[string]interface{}) map[string]interface{} {
	if dest == nil {
		dest = make(map[string]interface{}, len(src))
	}
	for k, v := range src {
		if vMap, ok := v.(map[string]interface{}); ok {
			// merge nested map
			nested, found := dest[k].(map[string]interface{})
			if !found || nested == nil {
				nested = make(map[string]interface{}, len(vMap))
			}
			dest[k] = deepMerge(nested, vMap)
		} else {
			// override primitive or slice
			dest[k] = v
		}
	}
	return dest
}

// deepCopy returns a deep copy of a map[string]interface{}
func deepCopy(src map[string]interface{}) map[string]interface{} {
	copy := make(map[string]interface{}, len(src))
	for k, v := range src {
		if m, ok := v.(map[string]interface{}); ok {
			copy[k] = deepCopy(m)
		} else {
			copy[k] = v
		}
	}
	return copy
}

func getStatus(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{"status": "ok"})
}

// POST /api/v1/go_poweroff?confirm=yes — shut the device down via the PMU.
// The confirm parameter is required so a stray POST can't power the box off.
func requestPowerOff(c *fiber.Ctx) error {
	if c.Query("confirm") != "yes" && c.FormValue("confirm") != "yes" {
		return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{
			"status":  "error",
			"message": "add confirm=yes to actually power off",
		})
	}
	log.Println("Power off requested via HTTP")
	go func() {
		if err := httpCovPmuPowerOff(); err != nil {
			log.Printf("Power off failed: %v", err)
		}
	}()
	return c.JSON(fiber.Map{"status": "ok", "message": "powering off"})
}

func resetConfig(c *fiber.Ctx) error {
	cfg = dftCfg
	userCfg = Config{}
	saveUserConfigToFile()
	return c.JSON(fiber.Map{"status": "ok"})
}

// hsvToRgb converts h∈[0,1], s∈[0,1], v∈[0,1] to r,g,b∈[0,1].
func hsvToRgb(h, s, v float64) (r, g, b float64) {
	i := int(h * 6)
	f := h*6 - float64(i)
	p := v * (1 - s)
	q := v * (1 - f*s)
	t := v * (1 - (1-f)*s)
	switch i % 6 {
	case 0:
		r, g, b = v, t, p
	case 1:
		r, g, b = q, v, p
	case 2:
		r, g, b = p, v, t
	case 3:
		r, g, b = p, q, v
	case 4:
		r, g, b = t, p, v
	case 5:
		r, g, b = v, p, q
	}
	return
}

func httpDrawText(c *fiber.Ctx) error {
	// Acquire lock (blocks if another request is drawing)
	now := time.Now().Format("2006-01-02 15:04:05")
	drawMu.Lock()
	defer drawMu.Unlock()

	// 1) Grab the "text" query (empty if missing)
	text := c.Query("text", "")

	// 2) Load your tiny font
	faceTiny, _, err := getFontFace("tiny")
	if err != nil {
		log.Fatalf("Failed to load font: %v", err)
	}

	faceHuge, _, err := getFontFace("huge")
	if err != nil {
		log.Fatalf("Failed to load font: %v", err)
	}

	// 3) Pause your main‐loop so it won’t overwrite
	runMainLoop = false

	// 4) Prepare a blank frame using pool to reduce allocations
	width, height := 172, 320
	frame := GetFrameBuffer(width, height)
	defer ReturnFrameBuffer(frame)

	if text != "" {
		// Draw the provided text centered
		drawText(frame, text, width/2, height/2, faceHuge, PCAT_WHITE, true)
	} else {
		// Seed randomness for a fresh pattern
		rand.Seed(time.Now().UnixNano())
		hueOffset := rand.Float64()
		phase := rand.Float64() * 2 * math.Pi
		freq := 0.01 + rand.Float64()*0.09

		cx, cy := float64(width)/2, float64(height)/2
		for x := 0; x < width; x++ {
			for y := 0; y < height; y++ {
				dx, dy := float64(x)-cx, float64(y)-cy
				angle := math.Atan2(dy, dx)
				hue := math.Mod((angle+math.Pi)/(2*math.Pi)+hueOffset, 1.0)
				dist := math.Hypot(dx, dy)
				val := 0.5 + 0.5*math.Sin(dist*freq+phase)

				rF, gF, bF := hsvToRgb(hue, 1.0, val)
				frame.Set(x, y, color.RGBA{
					R: uint8(rF * 255),
					G: uint8(gF * 255),
					B: uint8(bF * 255),
					A: 255,
				})
			}
		}

		// Overlay current time

		drawText(frame, "no text provided", width/2, height/2-20, faceTiny, PCAT_WHITE, true)
		drawText(frame, now, width/2, height/2, faceTiny, PCAT_WHITE, true)
	}

	// 5) Push to display
	time.Sleep(50 * time.Millisecond) //wait other goroutine to finish, TODO use mutex
	sendFull(display, frame)

	// 6) JSON response
	return c.JSON(fiber.Map{
		"status": "ok",
		"text":   text,
		"time":   now,
	})
}

func makeItRun(c *fiber.Ctx) error {
	setWeAreRunning(true)
	runMainLoop = true
	return c.JSON(fiber.Map{"status": "ok"})
}

func setPingSites(c *fiber.Ctx) error {
	// update in-memory
	configMutex.Lock()
	userCfg.PingSite0 = c.FormValue("ping_site0")
	userCfg.PingSite1 = c.FormValue("ping_site1")
	if t := c.FormValue("ping_type0"); t != "" {
		userCfg.PingType0 = normalizePingType(t)
	}
	if t := c.FormValue("ping_type1"); t != "" {
		userCfg.PingType1 = normalizePingType(t)
	}
	saveUserConfigToFile() // persist
	configMutex.Unlock()

	// Rebuild merged cfg so the rows pick the change up now, not at the next
	// restart. (Takes configMutex itself, so it must run after the Unlock.)
	mergeConfigs()

	return c.JSON(fiber.Map{"status": "ok"})
}

func setScreenDimmerTime(c *fiber.Ctx) error {
	onBatterySeconds := c.FormValue("screen_dimmer_time_on_battery_seconds")
	onDCSeconds := c.FormValue("screen_dimmer_time_on_dc_seconds")

	onBatterySecondsInt, err := strconv.Atoi(onBatterySeconds)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"status": "error", "message": "Invalid screen_dimmer_time_on_battery_seconds"})
	}

	onDCSecondsInt, err := strconv.Atoi(onDCSeconds)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{"status": "error", "message": "Invalid screen_dimmer_time_on_dc_seconds"})
	}

	configMutex.Lock()
	userCfg.ScreenDimmerTimeOnBatterySeconds = onBatterySecondsInt
	userCfg.ScreenDimmerTimeOnDCSeconds = onDCSecondsInt
	saveUserConfigToFile()
	configMutex.Unlock()

	return c.JSON(fiber.Map{"status": "ok"})
}

func setShowSMS(c *fiber.Ctx) error {
	raw := c.FormValue("showSMS")

	valid_values := []string{"true", "false"}

	if !slices.Contains(valid_values, raw) {
		return c.Status(400).JSON(fiber.Map{
			"status":  "error",
			"message": "showSMS must be boolean",
		})
	}

	configMutex.Lock()
	userCfg.ShowSms = (strings.ToLower(raw) == "true")
	saveUserConfigToFile()
	configMutex.Unlock()

	return c.JSON(fiber.Map{"status": "ok", "showSMS": raw})
}

// setShowWifiQr toggles the Wi-Fi QR pages and applies it immediately.
func setShowWifiQr(c *fiber.Ctx) error {
	raw := strings.ToLower(c.FormValue("show_wifi_qr"))
	if raw != "true" && raw != "false" {
		return c.Status(400).JSON(fiber.Map{
			"status":  "error",
			"message": "show_wifi_qr must be boolean",
		})
	}
	on := raw == "true"

	configMutex.Lock()
	userCfg.ShowWifiQr = &on
	saveUserConfigToFile()
	configMutex.Unlock()

	// Rebuild the merged cfg (and the page set) now. Takes configMutex itself.
	mergeConfigs()

	return c.JSON(fiber.Map{"status": "ok", "show_wifi_qr": on})
}

func getMaxBacklight(c *fiber.Ctx) error {
	// Check for runtime override first
	runtimeBrightnessMu.RLock()
	if runtimeMaxBrightness != nil {
		brightness := *runtimeMaxBrightness
		runtimeBrightnessMu.RUnlock()
		return c.JSON(fiber.Map{
			"status":         "ok",
			"max_brightness": brightness,
			"source":         "runtime_override",
		})
	}
	runtimeBrightnessMu.RUnlock()

	// Use config.json value
	configMutex.RLock()
	maxBrightness := cfg.ScreenMaxBrightness
	configMutex.RUnlock()

	return c.JSON(fiber.Map{
		"status":         "ok",
		"max_brightness": maxBrightness,
		"source":         "config",
	})
}

func setMaxBacklight(c *fiber.Ctx) error {
	raw := c.FormValue("max_brightness")
	if raw == "" {
		return c.Status(400).JSON(fiber.Map{
			"status":  "error",
			"message": "max_brightness parameter is required",
		})
	}

	maxBrightness, err := strconv.Atoi(raw)
	if err != nil {
		return c.Status(400).JSON(fiber.Map{
			"status":  "error",
			"message": "max_brightness must be a valid integer",
		})
	}

	if maxBrightness < 0 || maxBrightness > 100 {
		return c.Status(400).JSON(fiber.Map{
			"status":  "error",
			"message": "max_brightness must be between 0 and 100",
		})
	}

	// Set runtime override instead of modifying config
	runtimeBrightnessMu.Lock()
	runtimeMaxBrightness = &maxBrightness
	runtimeBrightnessMu.Unlock()

	return c.JSON(fiber.Map{
		"status":         "ok",
		"max_brightness": maxBrightness,
		"source":         "runtime_override",
		"message":        "Runtime brightness override set (does not modify config.json)",
	})
}

func resetMaxBacklight(c *fiber.Ctx) error {
	// Clear runtime override to return to config.json values
	runtimeBrightnessMu.Lock()
	runtimeMaxBrightness = nil
	runtimeBrightnessMu.Unlock()

	// Get current config value to return
	configMutex.RLock()
	maxBrightness := cfg.ScreenMaxBrightness
	configMutex.RUnlock()

	return c.JSON(fiber.Map{
		"status":         "ok",
		"max_brightness": maxBrightness,
		"source":         "config",
		"message":        "Runtime brightness override cleared, now following config.json",
	})
}

// getEffectiveMaxBrightness returns the runtime override if set, otherwise config value
func getEffectiveMaxBrightness() int {
	runtimeBrightnessMu.RLock()
	if runtimeMaxBrightness != nil {
		brightness := *runtimeMaxBrightness
		runtimeBrightnessMu.RUnlock()
		return brightness
	}
	runtimeBrightnessMu.RUnlock()

	// Return config value
	configMutex.RLock()
	maxBrightness := cfg.ScreenMaxBrightness
	configMutex.RUnlock()
	return maxBrightness
}

func httpServer(port string) {
	app := fiber.New()

	// Routes
	app.Get("/", indexHandler)
	app.Get("/api/v1/go_frame.png", serveFrame)
	app.Get("/api/v1/go_data.json", getData)     //TODO: add content
	app.Post("/api/v1/go_data.json", updateData) //TODO: add content
	app.Get("/api/v1/go_changePage", changePage)
	app.Get("/api/v1/go_display_text.json", httpDrawText)
	app.Get("/api/v1/go_make_it_run", makeItRun)
	//get/set configs (json)
	app.Get("/api/v1/go_get_default_config.json", getDefaultConfig)
	app.Get("/api/v1/go_get_config.json", getConfig)
	app.Get("/api/v1/go_get_user_config.json", getUserConfig)
	app.Post("/api/v1/go_save_user_config.json", saveUserConfigFromWeb)
	app.Post("/api/v1/go_set_user_config.json", setUserConfig)
	app.Get("/api/v1/go_get_status.json", getStatus)
	app.Get("/api/v1/go_reset_config", resetConfig)
	app.Post("/api/v1/go_poweroff", requestPowerOff)

	//get/set individual configs
	app.Post("/api/v1/go_set_ping_sites", setPingSites)
	app.Post("/api/v1/go_set_screen_dimmer_time", setScreenDimmerTime)
	app.Post("/api/v1/go_set_show_sms", setShowSMS)
	app.Post("/api/v1/go_set_show_wifi_qr", setShowWifiQr)
	app.Get("/api/v1/go_get_max_backlight", getMaxBacklight)
	app.Post("/api/v1/go_set_max_backlight", setMaxBacklight)
	app.Post("/api/v1/go_reset_max_backlight", resetMaxBacklight)

	// Custom metrics endpoints
	app.Get("/api/v1/custom_metrics/status", getCustomMetricsStatus)
	app.Post("/api/v1/custom_metrics/data", updateCustomMetricsData)
	app.Post("/api/v1/custom_metrics/:source/execute", executeCustomMetricsSource)

	// Start server, retry if failed
	var ln net.Listener
	var err error
	for {
		ln, err = net.Listen("tcp", port)
		if err != nil {
			log.Printf("cannot bind to %s: %v — retrying in 2s…", port, err)
			time.Sleep(2 * time.Second)
			continue
		}
		break
	}

	log.Println("Successfully bound to", port)
	log.Fatal(app.Listener(ln))
}

// ============================================================================
// Custom Metrics Handlers
// ============================================================================

// getCustomMetricsStatus returns the status of all custom metric sources
func getCustomMetricsStatus(c *fiber.Ctx) error {
	if customMetricsMgr == nil {
		return c.Status(404).JSON(fiber.Map{
			"error": "custom metrics manager not initialized",
		})
	}

	statuses := customMetricsMgr.GetAllStatus()
	return c.JSON(fiber.Map{
		"sources": statuses,
		"count":   len(statuses),
	})
}

// updateCustomMetricsData updates custom metrics data (for HTTP sources)
func updateCustomMetricsData(c *fiber.Ctx) error {
	if customMetricsMgr == nil {
		return c.Status(404).JSON(fiber.Map{
			"error": "custom metrics manager not initialized",
		})
	}

	var data map[string]interface{}
	if err := c.BodyParser(&data); err != nil {
		return c.Status(400).JSON(fiber.Map{
			"error": "invalid JSON",
		})
	}

	// Find HTTP sources and update them
	updated := false
	for _, source := range customMetricsMgr.sources {
		if httpSource, ok := source.(*HTTPSource); ok {
			if err := httpSource.UpdateData(data); err != nil {
				return c.Status(500).JSON(fiber.Map{
					"error": err.Error(),
				})
			}
			updated = true
		}
	}

	if !updated {
		// No HTTP source found, just store the data directly
		for key, value := range data {
			globalData.Store(key, fmt.Sprint(value))
		}
	}

	return c.JSON(fiber.Map{
		"status":  "ok",
		"updated": len(data),
	})
}

// executeCustomMetricsSource triggers immediate execution of a command source
func executeCustomMetricsSource(c *fiber.Ctx) error {
	if customMetricsMgr == nil {
		return c.Status(404).JSON(fiber.Map{
			"error": "custom metrics manager not initialized",
		})
	}

	sourceName := c.Params("source")
	if sourceName == "" {
		return c.Status(400).JSON(fiber.Map{
			"error": "source name is required",
		})
	}

	source := customMetricsMgr.GetSourceByName(sourceName)
	if source == nil {
		return c.Status(404).JSON(fiber.Map{
			"error": fmt.Sprintf("source '%s' not found", sourceName),
		})
	}

	// Command and http_poll sources support immediate execution
	if cmdSource, ok := source.(*CommandSource); ok {
		cmdSource.ExecuteNow()
		return c.JSON(fiber.Map{
			"status": "triggered",
			"source": sourceName,
		})
	}
	if pollSource, ok := source.(*HTTPPollSource); ok {
		pollSource.ExecuteNow()
		return c.JSON(fiber.Map{
			"status": "triggered",
			"source": sourceName,
		})
	}

	return c.Status(400).JSON(fiber.Map{
		"error": fmt.Sprintf("source '%s' does not support immediate execution", sourceName),
	})
}

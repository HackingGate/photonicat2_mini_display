package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/draw"

	xdraw "golang.org/x/image/draw"
	"image/png"
	"image/jpeg"
	"image/gif"
	"io"
	"log"
	"net/http"
	"os"
	"time"
	"bytes"
	"math"
	"math/rand"
	"strings"
	"strconv"
	"path/filepath"
	"regexp"
	gc9307 "github.com/photonicat/periph.io-gc9307"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"

	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
	"github.com/ajstarks/svgo"
	"github.com/llgcode/draw2d/draw2dimg"
)

var (
	cacheTopBarStr string
	cacheTopBar *image.RGBA
	cacheFooterStr string
	cacheFooter *image.RGBA
	// configVersion increments on every config (re)merge; it is part of the
	// middle-page fingerprint so recolors (themes) invalidate skip caches.
	configVersion int
)

//---------------- Drawing Functions ----------------
func drawText(img *image.RGBA, text string, posX, posY int, face font.Face, clr color.Color, center bool) (finishX, finishY int) {
    // Check if image is nil or has invalid bounds
    if img == nil || img.Bounds().Empty() {
        return posX, posY
    }
    
    d := &font.Drawer{
        Dst:  img,
        Src:  image.NewUniform(clr),
        Face: face,
    }

    // Get font metrics once.
    metrics := face.Metrics()

    // Calculate text dimensions.
    textWidth := d.MeasureString(text).Round()
    textHeight := (metrics.Ascent + metrics.Descent).Round()
    var x, y int
    if center {
        // Center horizontally: shift x left by half the text width.
        x = posX - textWidth/2
        // Center vertically: shift y up by half the text height, then add ascent for baseline.
        y = posY - textHeight/2 - metrics.Ascent.Round() 
		//we still use the same y
		//y = posY
    } else {
        x = posX
    }
	y = posY + metrics.Ascent.Round()

    // Bounds checking - only prevent extreme out-of-bounds cases
    bounds := img.Bounds()
    // Only skip if text would be completely outside the image
    if x + textWidth < bounds.Min.X || x > bounds.Max.X {
        return posX, posY
    }
    if y < bounds.Min.Y - metrics.Descent.Round() || y - metrics.Ascent.Round() > bounds.Max.Y {
        return posX, posY
    }

    // Set drawing position and draw the text with error recovery
    d.Dot = fixed.P(x, y)
    
    // Add a recovery mechanism for font rendering panics
    defer func() {
        if r := recover(); r != nil {
            log.Printf("Font rendering panic recovered: %v at position (%d, %d) in bounds %+v", r, x, y, bounds)
        }
    }()
    
    drawStringComposed(d, text)

    // Calculate finishing coordinates.
    finishX = x + textWidth
    finishY = y - metrics.Ascent.Round() + textHeight // Bottom of the text block.

    return
}

// plusMinus is the one glyph the layout asks for that Orbitron does not carry:
// U+00B1 is absent from every weight shipped in assets/fonts, and the missing
// glyph renders as a tofu box, not as nothing.
const plusMinus = '±'

// drawStringComposed draws s at the drawer's pen, building any ± itself when
// the face has no glyph for it. The alternative — borrowing the sign from the
// CJK fallback font — would set a light, round symbol next to Orbitron's
// squared-off ExtraBold digits; composing it from the face's own "+" and "-"
// inherits exactly the right stroke weight, and costs a couple of extra
// DrawString calls on a string that is four characters long.
func drawStringComposed(d *font.Drawer, s string) {
	if _, ok := d.Face.GlyphAdvance(plusMinus); ok || !strings.ContainsRune(s, plusMinus) {
		d.DrawString(s)
		return
	}
	for i, part := range strings.Split(s, string(plusMinus)) {
		if i > 0 {
			drawPlusMinus(d)
		}
		d.DrawString(part)
	}
}

// drawPlusMinus stacks the face's "+" over its "-" at the drawer's pen and
// leaves the pen one ± advance further on. That advance is the width callers
// already measured for the rune (the notdef box's), so composing the glyph
// does not shift anything measured around it.
func drawPlusMinus(d *font.Drawer) {
	dot := d.Dot
	advance := font.MeasureString(d.Face, string(plusMinus))
	plus, _ := font.BoundString(d.Face, "+")
	bar, _ := font.BoundString(d.Face, "-")

	// The bar sits on the baseline, where the digits beside it sit; the plus
	// rides above it, clear by three quarters of the bar's own thickness.
	barDy := -bar.Max.Y
	plusDy := bar.Min.Y + barDy - (bar.Max.Y-bar.Min.Y)*3/4 - plus.Max.Y
	// Centre the pair in the advance, then line the bar up with the plus
	// rather than with the pen: the two glyphs carry different side bearings.
	plusDx := (advance-(plus.Max.X-plus.Min.X))/2 - plus.Min.X
	barDx := plus.Min.X + plusDx - bar.Min.X

	d.Dot = fixed.Point26_6{X: dot.X + plusDx, Y: dot.Y + plusDy}
	d.DrawString("+")
	d.Dot = fixed.Point26_6{X: dot.X + barDx, Y: dot.Y + barDy}
	d.DrawString("-")
	d.Dot = fixed.Point26_6{X: dot.X + advance, Y: dot.Y}
}

// drawVerticalText draws text with each rune stacked on its own line, reading
// top-to-bottom (e.g. "CPU" as C / P / U). It is used for the compact axis-style
// labels beside the CPU and memory bar meters. (x, y) is the top-left of the
// column; letters are horizontally centered within charW. lineH is the vertical
// step between letters.
func drawVerticalText(frame *image.RGBA, text string, x, y, charW, lineH int, face font.Face, clr color.Color) {
	if frame == nil || text == "" {
		return
	}
	d := &font.Drawer{Face: face}
	cy := y
	for _, r := range text {
		s := string(r)
		w := d.MeasureString(s).Round()
		// Center each glyph in the column width.
		gx := x + (charW-w)/2
		drawText(frame, s, gx, cy, face, clr, false)
		cy += lineH
	}
}

//---------------- Horizontal ticker (marquee) scrolling ----------------
//
// Long values (e.g. a Wi-Fi SSID that is wider than the 172px screen) would
// otherwise be clipped at the right edge. drawScrollingText renders such a
// value as a NASDAQ-style ticker: the text pauses briefly, then scrolls left
// at a constant pixel-per-second rate and wraps around seamlessly, so every
// character is eventually readable. Text that already fits is drawn in place,
// unchanged.
//
// Scrolling is driven off wall-clock time (not the frame counter) so the
// motion speed is identical regardless of the render FPS; the main loop only
// controls how *smooth* it looks by choosing how often to re-render.

const (
	// scrollSpeedPxPerSec is how fast the ticker slides left. ~40 px/s reads
	// like a stock ticker: fast enough to get through a long SSID quickly,
	// slow enough to actually read.
	scrollSpeedPxPerSec = 40.0
	// scrollStartPauseMs holds the text still at the start of each loop so the
	// beginning (the most important part) is readable before it moves.
	scrollStartPauseMs = 1200.0
	// scrollGapPx is the blank space between the end of the text and the start
	// of its wrapped-around copy, so the loop seam is visually obvious.
	scrollGapPx = 24
)

// scrollEpoch anchors the ticker time base. Set once on first use so all
// tickers share a phase and elapsed time never depends on process start
// details. time.Now() is fine here (this is the running app, not a workflow).
var scrollEpoch time.Time

// anyTextScrolling is set true by drawScrollingText whenever it renders a
// value that is actually overflowing (and thus animating) during the current
// render pass. The main loop reads and resets it to decide whether to bump the
// frame rate for smoothness. It is written and read from the single render
// goroutine, so no locking is required.
var anyTextScrolling bool

// scrollScratch is a reused clip buffer for drawScrollingText so 30 FPS ticker
// motion does not allocate a fresh image every frame.
var scrollScratch *image.RGBA

// lastMiddleFingerprint / forceMiddleRedraw drive skip-unchanged middle
// rendering: when the page's data keys have not changed and nothing is
// scrolling, the main loop skips clear+render+SPI to save CPU and bus power.
var (
	lastMiddleFingerprint string
	forceMiddleRedraw     bool
)

// appendDataKeyFingerprint writes "|key=value" for a globalData key into b.
func appendDataKeyFingerprint(b *strings.Builder, key string) {
	if key == "" {
		return
	}
	b.WriteByte('|')
	b.WriteString(key)
	b.WriteByte('=')
	if v, ok := globalData.Load(key); ok && v != nil {
		fmt.Fprint(b, v)
	}
}

// middlePageFingerprint builds a compact signature of everything that can
// affect the middle-area pixels for the current page. Equal fingerprints mean
// a full re-render would produce the same image (aside from ticker animation).
func middlePageFingerprint(cfg *Config, isSMS bool, pageIdx int) string {
	if isSMS {
		return "sms:" + strconv.Itoa(pageIdx) + ":" + strconv.Itoa(len(smsPagesImages))
	}
	if cfg == nil {
		return "nil"
	}
	if isWifiQrPage(pageIdx) {
		return wifiQrFingerprint(pageIdx - templatePageCount())
	}
	var b strings.Builder
	b.Grow(256)
	b.WriteString("v")
	b.WriteString(strconv.Itoa(configVersion))
	b.WriteString("p")
	b.WriteString(strconv.Itoa(pageIdx))
	page := cfg.DisplayTemplate.Elements["page"+strconv.Itoa(pageIdx)]
	for _, el := range page {
		if el.Enable == 0 {
			continue
		}
		switch el.Type {
		case "text", "cpu_bars", "hbar":
			appendDataKeyFingerprint(&b, el.DataKey)
			appendDataKeyFingerprint(&b, el.LabelDataKey)
			if el.DataKey != "" {
				appendDataKeyFingerprint(&b, el.DataKey+"_Unit")
			}
			appendDataKeyFingerprint(&b, el.AnchorAfterDataKey)
		case "icon":
			appendDataKeyFingerprint(&b, el.AnchorAfterDataKey)
			b.WriteString("|i:")
			b.WriteString(el.IconPath)
		case "disk_bars":
			appendDataKeyFingerprint(&b, "DiskUsagePercent")
			appendDataKeyFingerprint(&b, "DiskNvmePercent")
			appendDataKeyFingerprint(&b, "DiskNvmePresent")
			appendDataKeyFingerprint(&b, "DiskSDPercent")
			appendDataKeyFingerprint(&b, "DiskSDPresent")
		case "graph":
			// Graph pixels move when a new sample is recorded.
			powerData.mu.RLock()
			n := len(powerData.Samples)
			var last float64
			if n > 0 {
				last = powerData.Samples[n-1].Wattage
			}
			powerData.mu.RUnlock()
			fmt.Fprintf(&b, "|g:%d:%.3f", n, last)
		case "fixed_text", "vtext":
			// Static labels — layout only, no data.
		}
	}
	return b.String()
}

// pageHasOverflowText reports whether any text element on the page is wider
// than its available slot and would therefore run as a ticker. Used to keep
// re-rendering (and bump FPS) even when the underlying string is unchanged.
func pageHasOverflowText(cfg *Config, pageIdx int) bool {
	if cfg == nil {
		return false
	}
	if isWifiQrPage(pageIdx) {
		return wifiQrSsidOverflows(pageIdx - templatePageCount())
	}
	page := cfg.DisplayTemplate.Elements["page"+strconv.Itoa(pageIdx)]
	for _, el := range page {
		if el.Enable == 0 || el.Type != "text" || el.DataKey == "" {
			continue
		}
		v, ok := globalData.Load(el.DataKey)
		if !ok || v == nil {
			continue
		}
		text := strings.TrimSpace(fmt.Sprintf("%v", v))
		if text == "" || text == "-" || isErrorSentinel(text) {
			continue
		}
		// Ping timeout "X" never scrolls.
		if el.DataKey == "Ping0" || el.DataKey == "Ping1" {
			if iv, ok := v.(int64); ok && iv < 0 {
				continue
			}
			if iv, ok := v.(int); ok && iv < 0 {
				continue
			}
		}
		face, _, err := getFontFaceForText(el.Font, text)
		if err != nil {
			continue
		}
		availWidth := PCAT2_LCD_WIDTH - el.Position.X - PCAT2_R_MARGIN
		if el.Size != nil && el.Size.Width > 0 {
			availWidth = el.Size.Width
		} else if el.Size2 != nil && el.Size2.Width > 0 {
			availWidth = el.Size2.Width
		}
		if font.MeasureString(face, text).Round() > availWidth {
			return true
		}
	}
	return false
}

// drawScrollingText draws text at (x, y). availWidth is the horizontal space
// the text may occupy (from x to the clip edge). If the text fits, it is drawn
// normally and false is returned. If it overflows, it is drawn as a clipped,
// wrapping ticker and true is returned. clr is the text color; face its font.
func drawScrollingText(frame *image.RGBA, text string, x, y, availWidth int, face font.Face, clr color.Color) bool {
	return drawScrollingTextShift(frame, text, x, y, availWidth, face, clr, 0)
}

// drawScrollingTextShift is drawScrollingText with a sub-pixel pen shift
// applied to the ticker, so a caller can land the text's ink exactly on a
// column (the Wi-Fi QR page passes minus the first glyph's side-bearing).
func drawScrollingTextShift(frame *image.RGBA, text string, x, y, availWidth int, face font.Face, clr color.Color, shift fixed.Int26_6) bool {
	if frame == nil || text == "" || availWidth <= 0 {
		drawText(frame, text, x, y, face, clr, false)
		return false
	}

	d := &font.Drawer{Face: face}
	textW := d.MeasureString(text).Round()

	// Fits comfortably: draw in place, no scrolling.
	if textW <= availWidth {
		drawText(frame, text, x, y, face, clr, false)
		return false
	}

	if scrollEpoch.IsZero() {
		scrollEpoch = time.Now()
	}

	metrics := face.Metrics()
	ascent := metrics.Ascent.Round()
	regionH := ascent + metrics.Descent.Round()
	if regionH <= 0 {
		regionH = 1
	}

	// One full loop = the text plus a trailing gap; wrapping this distance
	// brings the identical second copy exactly into the first's start.
	loopW := textW + scrollGapPx

	// Time within the current loop: a stationary start pause followed by the
	// slide. Using milliseconds keeps the math integer-friendly.
	scrollMs := (float64(loopW) / scrollSpeedPxPerSec) * 1000.0
	cycleMs := scrollStartPauseMs + scrollMs
	elapsed := math.Mod(float64(time.Since(scrollEpoch).Milliseconds()), cycleMs)

	var offset int // how many px the text is shifted left
	if elapsed > scrollStartPauseMs {
		offset = int(((elapsed - scrollStartPauseMs) / 1000.0) * scrollSpeedPxPerSec)
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= loopW {
		offset = offset % loopW
	}

	// Reuse a clip scratch buffer so smooth ticker FPS does not allocate.
	if scrollScratch == nil || scrollScratch.Bounds().Dx() < availWidth || scrollScratch.Bounds().Dy() < regionH {
		scrollScratch = image.NewRGBA(image.Rect(0, 0, availWidth, regionH))
	} else {
		// Clear only the used region (transparent black).
		pix := scrollScratch.Pix
		rowBytes := availWidth * 4
		for row := 0; row < regionH; row++ {
			off := row * scrollScratch.Stride
			for i := 0; i < rowBytes; i++ {
				pix[off+i] = 0
			}
		}
	}
	region := scrollScratch
	src := image.NewUniform(clr)
	rd := &font.Drawer{Dst: region, Src: src, Face: face}
	baseline := ascent
	rd.Dot = fixed.Point26_6{X: fixed.I(-offset) + shift, Y: fixed.I(baseline)}
	rd.DrawString(text)
	// Second copy trails the first by loopW; it scrolls in from the right as
	// the first leaves on the left.
	rd.Dot = fixed.Point26_6{X: fixed.I(-offset+loopW) + shift, Y: fixed.I(baseline)}
	rd.DrawString(text)

	// Blit the clipped region into the frame at the element position. Use Over
	// so anti-aliased edges blend; the region's transparent pixels leave the
	// background untouched.
	dstRect := image.Rect(x, y, x+availWidth, y+regionH)
	draw.Draw(frame, dstRect, region, image.Point{}, draw.Over)

	anyTextScrolling = true
	return true
}

// remoteImageCacheDir is where downloaded remote icons are stored on disk.
// A var (defaulted to the original path) rather than a const so tests can
// point it at a temp dir; production behavior is unchanged.
var remoteImageCacheDir = "/tmp/pcat_remote_icons"

// fetchRemoteImage downloads an http(s) image URL to a deterministic local
// file (named by the URL hash, keeping the original extension) and returns the
// local path. If the file already exists on disk it is reused without
// re-downloading, so restarts are cheap.
func fetchRemoteImage(url string) (string, error) {
	ext := strings.ToLower(filepath.Ext(url))
	if ext == "" {
		ext = ".svg" // assume SVG when the URL carries no extension
	}
	sum := sha1.Sum([]byte(url))
	dest := filepath.Join(remoteImageCacheDir, hex.EncodeToString(sum[:])+ext)

	if _, err := os.Stat(dest); err == nil {
		return dest, nil // already cached on disk
	}

	if err := os.MkdirAll(remoteImageCacheDir, 0o755); err != nil {
		return "", err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", getUserAgent())
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Write atomically via a temp file so a partial download is never read.
	tmp := dest + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, io.LimitReader(resp.Body, 8<<20)); err != nil { // cap 8 MiB
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	out.Close()
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return dest, nil
}

// placeholderRe matches [key] tokens in fixed_text labels.
var placeholderRe = regexp.MustCompile(`\[(\w+)\]`)

func loadImage(filePath string) (*image.RGBA, int, int, error) {
	// Check if image is in cache (keyed by the original path/URL, so a remote
	// icon is only downloaded and rendered once per process).
	imageCacheMu.RLock()
	cachedImg, inCache := imageCache[filePath]
	imageCacheMu.RUnlock()
	if inCache {
		bounds := cachedImg.Bounds()
		return cachedImg, bounds.Dx(), bounds.Dy(), nil
	}

	// Remote icon support: an http(s):// icon_path is downloaded to a local
	// cache file, then decoded by the normal path below. The firmware itself
	// has no networked file system, so the fetch happens here once.
	localPath := filePath
	if strings.HasPrefix(filePath, "http://") || strings.HasPrefix(filePath, "https://") {
		p, err := fetchRemoteImage(filePath)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("fetch remote image %q: %w", filePath, err)
		}
		localPath = p
	}

	ext := strings.ToLower(filepath.Ext(localPath))

	// Open the file.
	f, err := os.Open(localPath)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()

	var img image.Image

	switch ext {
	case ".png":
		img, err = png.Decode(f)
		if err != nil {
			return nil, 0, 0, err
		}
	case ".jpg", ".jpeg":
		img, err = jpeg.Decode(f)
		if err != nil {
			return nil, 0, 0, err
		}
	case ".gif":
		img, err = gif.Decode(f)
		if err != nil {
			return nil, 0, 0, err
		}
	case ".svg":
		// Supersampled AA cache key (old 1:1 keys are intentionally unused).
		cacheKey := filePath + "_rendered_aa" + strconv.Itoa(barFrameAAScale)
		imageCacheMu.RLock()
		cachedSvg, svgCached := imageCache[cacheKey]
		imageCacheMu.RUnlock()
		if svgCached {
			bounds := cachedSvg.Bounds()
			return cachedSvg, bounds.Dx(), bounds.Dy(), nil
		}

		svgData, err := io.ReadAll(f)
		if err != nil {
			return nil, 0, 0, err
		}
		icon, err := oksvg.ReadIconStream(bytes.NewReader(svgData))
		if err != nil {
			return nil, 0, 0, err
		}
		w := int(icon.ViewBox.W)
		h := int(icon.ViewBox.H)
		if w <= 0 || h <= 0 {
			return nil, 0, 0, fmt.Errorf("svg %s: invalid viewBox", filePath)
		}
		rgba := rasterizeIconAA(icon, w, h, barFrameAAScale)
		imageCacheMu.Lock()
		imageCache[cacheKey] = rgba
		imageCache[filePath] = rgba
		imageCacheMu.Unlock()
		return rgba, w, h, nil
	default:
		return nil, 0, 0, fmt.Errorf("unsupported image format: %s", ext)
	}

	// Convert the decoded image to RGBA if needed.
	bounds := img.Bounds()
	rgba := image.NewRGBA(bounds)
	draw.Draw(rgba, bounds, img, bounds.Min, draw.Src)
	// Cache the image.
	imageCacheMu.Lock()
	imageCache[filePath] = rgba
	imageCacheMu.Unlock()
	return rgba, bounds.Dx(), bounds.Dy(), nil
}


// copyImageToFrameBuffer converts an image.RGBA to a 1D []color.RGBA slice.
func copyImageToFrameBuffer(img *image.RGBA, frame []color.RGBA) {
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			idx := y*width + x
			frame[idx] = img.RGBAAt(x, y)
		}
	}
}

func sendTopBar(display gc9307.Device, frame *image.RGBA) {
	// Early exit for nil frames
	if frame == nil {
		return
	}
	
	// Early exit for empty frames
	if frame.Bounds().Empty() {
		return
	}
	
	if displayWrapper != nil {
		displayWrapper.FillRectangleWithImageOptimized(0, 0, topBarSendWidth, topBarSendHeight, frame)
	} else {
		display.FillRectangleWithImage(0, 0, topBarSendWidth, topBarSendHeight, frame)
	}
	// Remember for HTTP snapshot (only published when web UI is polling).
	rememberWebRegion("top", frame)
	tryPublishWebSnapshot()
}

func sendFooter(display gc9307.Device, frame *image.RGBA) {
	// Early exit for nil frames
	if frame == nil {
		return
	}
	
	// Early exit for empty frames
	if frame.Bounds().Empty() {
		return
	}
	
	if displayWrapper != nil {
		displayWrapper.FillRectangleWithImageOptimized(0, footerSendY, footerSendWidth, footerSendHeight, frame)
	} else {
		display.FillRectangleWithImage(0, footerSendY, footerSendWidth, footerSendHeight, frame)
	}
	rememberWebRegion("footer", frame)
	tryPublishWebSnapshot()
}

// cropToContent scans the given frame and returns a sub-image that contains only non-background pixels.
func cropToContent(frame *image.RGBA, bgColor color.Color) *image.RGBA {
	bounds := frame.Bounds()
	minX, minY := bounds.Max.X, bounds.Max.Y
	maxX, maxY := bounds.Min.X, bounds.Min.Y

	// Loop over all pixels in the image.
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if !isBackground(frame.At(x, y), bgColor) {
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}

	// No content found? Return an empty image.
	if minX > maxX || minY > maxY {
		return image.NewRGBA(image.Rect(0, 0, 0, 0))
	}

	// Create the cropping rectangle.
	cropRect := image.Rect(minX, minY, maxX+1, maxY+1)
	// Use SubImage to create a new image containing only the cropped area.
	return frame.SubImage(cropRect).(*image.RGBA)
}

// isBackground compares a pixel to the given background color.
func isBackground(c color.Color, bg color.Color) bool {
	_, _, _, a1 := c.RGBA()
	//r2, g2, b2, a2 := bg.RGBA()
	return a1 == 0
}


func sendMiddlePartial(display gc9307.Device, frame *image.RGBA) {
	// Crop the frame to the region with content.
	croppedFrame := cropToContent(frame, color.Black) // assuming black is the background
	if croppedFrame.Bounds().Empty() {
		// Nothing to send.
		return
	}

	// Send the cropped frame to the display.
	// Here we use the cropped image's dimensions.
	if displayWrapper != nil {
		displayWrapper.FillRectangleWithImageOptimized(
			int16(croppedFrame.Bounds().Min.X),
			int16(croppedFrame.Bounds().Min.Y),
			int16(croppedFrame.Bounds().Dx()),
			int16(croppedFrame.Bounds().Dy()),
			croppedFrame,
		)
	} else {
		display.FillRectangleWithImage(
			int16(croppedFrame.Bounds().Min.X),
			int16(croppedFrame.Bounds().Min.Y),
			int16(croppedFrame.Bounds().Dx()),
			int16(croppedFrame.Bounds().Dy()),
			croppedFrame,
		)
	}
}

// Pre-calculated constants for send functions performance
var (
	// Middle area
	middleSendY      = int16(PCAT2_TOP_BAR_HEIGHT)
	middleSendWidth  = int16(PCAT2_LCD_WIDTH)
	middleSendHeight = int16(PCAT2_LCD_HEIGHT - PCAT2_TOP_BAR_HEIGHT - PCAT2_FOOTER_HEIGHT)
	
	// Top bar area
	topBarSendWidth  = int16(PCAT2_LCD_WIDTH)
	topBarSendHeight = int16(PCAT2_TOP_BAR_HEIGHT)
	
	// Footer area
	footerSendY      = int16(PCAT2_LCD_HEIGHT - PCAT2_FOOTER_HEIGHT)
	footerSendWidth  = int16(PCAT2_LCD_WIDTH)
	footerSendHeight = int16(PCAT2_FOOTER_HEIGHT)
	
	// Full area
	fullSendWidth    = int16(PCAT2_LCD_WIDTH)
	fullSendHeight   = int16(PCAT2_LCD_HEIGHT)
)

// sendMiddle sends the middle frame area with performance optimizations
func sendMiddle(display gc9307.Device, frame *image.RGBA) {
	// Early exit for nil frames
	if frame == nil {
		return
	}
	
	// Early exit for empty frames
	bounds := frame.Bounds()
	if bounds.Empty() {
		return
	}
	
	// Use optimized path with pre-calculated constants
	if displayWrapper != nil {
		displayWrapper.FillRectangleWithImageOptimized(0, middleSendY, middleSendWidth, middleSendHeight, frame)
	} else {
		display.FillRectangleWithImage(0, middleSendY, middleSendWidth, middleSendHeight, frame)
	}
	// Middle is the last region of a normal frame; publish a complete snapshot
	// for /api/v1/go_frame.png when the web UI is open (no-op otherwise).
	rememberWebRegion("middle", frame)
	tryPublishWebSnapshot()
}

// Global variable to store the last sent middle frame for comparison
var lastMiddleFrame *image.RGBA

// areFramesIdentical performs a fast comparison of two frames
func areFramesIdentical(frame1, frame2 *image.RGBA) bool {
	if frame1 == nil || frame2 == nil {
		return frame1 == frame2
	}
	
	bounds1 := frame1.Bounds()
	bounds2 := frame2.Bounds()
	
	// Quick bounds check
	if bounds1 != bounds2 {
		return false
	}
	
	// Quick pixel data length check
	if len(frame1.Pix) != len(frame2.Pix) {
		return false
	}
	
	// Fast byte-by-byte comparison of pixel data
	pix1 := frame1.Pix
	pix2 := frame2.Pix
	
	// Compare in chunks for better performance
	chunkSize := 1024 // Compare 1KB chunks at a time
	for i := 0; i < len(pix1); i += chunkSize {
		end := i + chunkSize
		if end > len(pix1) {
			end = len(pix1)
		}
		
		// Compare chunk
		for j := i; j < end; j++ {
			if pix1[j] != pix2[j] {
				return false
			}
		}
	}
	
	return true
}

// sendMiddleOptimized sends middle frame only if it has changed from the last frame
func sendMiddleOptimized(display gc9307.Device, frame *image.RGBA) {
	// Early exit for nil frames
	if frame == nil {
		return
	}
	
	// Early exit for empty frames
	bounds := frame.Bounds()
	if bounds.Empty() {
		return
	}
	
	// Skip sending if frame is identical to the last one
	if lastMiddleFrame != nil && areFramesIdentical(frame, lastMiddleFrame) {
		return // Frame unchanged, skip send
	}
	
	// Send the frame
	sendMiddle(display, frame)
	
	// Store a copy of this frame for next comparison
	// Note: This creates a memory copy - consider using a hash instead for memory efficiency
	if lastMiddleFrame == nil || !lastMiddleFrame.Bounds().Eq(bounds) {
		lastMiddleFrame = image.NewRGBA(bounds)
	}
	copy(lastMiddleFrame.Pix, frame.Pix)
}

func sendFull(display gc9307.Device, frame *image.RGBA) {
	// Early exit for nil frames
	if frame == nil {
		return
	}
	
	// Early exit for empty frames
	if frame.Bounds().Empty() {
		return
	}
	
	if displayWrapper != nil {
		displayWrapper.FillRectangleWithImageOptimized(0, 0, fullSendWidth, fullSendHeight, frame)
	} else {
		display.FillRectangleWithImage(0, 0, fullSendWidth, fullSendHeight, frame)
	}
}

// Function to display time on frame buffer
func testClock(frame *image.RGBA) {
    
    // Get current time and format it
    currDateTime := time.Now()
    currHour := currDateTime.Hour()
    currMinute := currDateTime.Minute()
    currSecond := currDateTime.Second()
    currMilli := currDateTime.Nanosecond() / 1000000 // Convert nanoseconds to milliseconds
    currDay := currDateTime.Day()
    currMonth := currDateTime.Month()
    currYear := currDateTime.Year()

    // Format the time as hh:mm:ss:SSS
    timeStr := fmt.Sprintf("%02d:%02d:%02d:%03d", currHour, currMinute, currSecond, currMilli)
    dateStr := fmt.Sprintf("%04d-%02d-%02d", currYear, currMonth, currDay)
    
    // Get font face for big time display
    face, _, err := getFontFace("big")
    if err != nil {
        fmt.Println("Error loading font:", err)
        return
    }

    // Clear the frame to black (optional, or use a background color)
    draw.Draw(frame, frame.Bounds(), &image.Uniform{color.Black}, image.Point{}, draw.Src)

    // Set the text color to white
    textColor := color.RGBA{255, 229, 0, 255}
    randomColor := color.RGBA{
        R: uint8(rand.Intn(256)),
        G: uint8(rand.Intn(256)),
        B: uint8(rand.Intn(256)),
        A: uint8(rand.Intn(256)),
    }

    // Draw the formatted time string onto the image
    drawText(frame, dateStr, 0, 0, face, textColor, false)
    drawText(frame, timeStr, 0, 30, face, randomColor, false)
}

// rasterizeIconAA draws an oksvg icon at destW×destH with optional
// supersample antialiasing (aaScale≥2 → render big, box-filter down).
func rasterizeIconAA(icon *oksvg.SvgIcon, destW, destH, aaScale int) *image.RGBA {
	if aaScale < 1 {
		aaScale = 1
	}
	if destW < 1 {
		destW = 1
	}
	if destH < 1 {
		destH = 1
	}
	bigW, bigH := destW*aaScale, destH*aaScale
	rgba := image.NewRGBA(image.Rect(0, 0, bigW, bigH))
	// Transparent clear (Pix is already zero / alpha 0).
	icon.SetTarget(0, 0, float64(bigW), float64(bigH))
	scanner := rasterx.NewScannerGV(bigW, bigH, rgba, rgba.Bounds())
	dasher := rasterx.NewDasher(bigW, bigH, scanner)
	icon.Draw(dasher, 1.0)
	if aaScale == 1 {
		return rgba
	}
	return downscaleBox(rgba, destW, destH)
}

// tintIconImage recolors a rasterized icon to clr. Brightness is scaled
// against the asset yellow (#FDE021, luma ≈211): full-yellow pixels become
// exactly clr, black detail (cpu/mem pin fills) stays black, and antialiased
// edges keep their ramp. Pixels are premultiplied RGBA, so scaling by the
// premultiplied luma also carries the alpha ramp through correctly.
func tintIconImage(src *image.RGBA, clr color.RGBA) *image.RGBA {
	const refLuma = 211.0
	dst := image.NewRGBA(src.Bounds())
	for i := 0; i+3 < len(src.Pix); i += 4 {
		a := src.Pix[i+3]
		if a == 0 {
			continue
		}
		luma := 0.299*float64(src.Pix[i]) + 0.587*float64(src.Pix[i+1]) + 0.114*float64(src.Pix[i+2])
		f := luma / refLuma
		if f > 1 {
			f = 1
		}
		dst.Pix[i] = uint8(float64(clr.R)*f + 0.5)
		dst.Pix[i+1] = uint8(float64(clr.G)*f + 0.5)
		dst.Pix[i+2] = uint8(float64(clr.B)*f + 0.5)
		dst.Pix[i+3] = a
	}
	return dst
}

// drawSVGTinted is drawSVG with an optional recolor: a nil tint draws the
// SVG's own colors. Tinted rasters are cached alongside the plain ones.
func drawSVGTinted(frame *image.RGBA, svgPath string, x0, y0, targetWidth, targetHeight int, tint *color.RGBA) error {
	// Resolve intrinsic size when the caller leaves a dimension at 0.
	svgFile, err := os.Open(svgPath)
	if err != nil {
		return err
	}
	defer svgFile.Close()

	svgData, err := io.ReadAll(svgFile)
	if err != nil {
		return err
	}

	icon, err := oksvg.ReadIconStream(bytes.NewReader(svgData))
	if err != nil {
		return err
	}
	if targetWidth == 0 {
		targetWidth = int(icon.ViewBox.W)
	}
	if targetHeight == 0 {
		targetHeight = int(icon.ViewBox.H)
	}
	if targetWidth < 1 || targetHeight < 1 {
		return fmt.Errorf("drawSVG %s: invalid target %dx%d", svgPath, targetWidth, targetHeight)
	}

	// Cache key includes AA scale so old 1:1 bitmaps are not reused.
	cacheKey := fmt.Sprintf("%s_%d_%d_aa%d", svgPath, targetWidth, targetHeight, barFrameAAScale)
	if tint != nil {
		cacheKey += fmt.Sprintf("_t%02x%02x%02x", tint.R, tint.G, tint.B)
	}
	if cachedImg, ok := svgCache[cacheKey]; ok {
		copyImageToImageAt(frame, cachedImg, x0, y0)
		return nil
	}

	img := rasterizeIconAA(icon, targetWidth, targetHeight, barFrameAAScale)
	if tint != nil {
		img = tintIconImage(img, *tint)
	}
	svgCache[cacheKey] = img
	copyImageToImageAt(frame, img, x0, y0)
	return nil
}

func drawSVG(frame *image.RGBA, svgPath string, x0, y0, targetWidth, targetHeight int) error {
	return drawSVGTinted(frame, svgPath, x0, y0, targetWidth, targetHeight, nil)
}

// renderSvgBytes rasterizes in-memory SVG data at its intrinsic size, so
// generated graphics (signal bars, boot progress bar) never touch the
// filesystem. If cacheKey is non-empty the rendered image is stored in
// imageCache under that key for reuse.
func renderSvgBytes(svgData []byte, cacheKey string) (*image.RGBA, error) {
	return renderSvgBytesAA(svgData, cacheKey, 1)
}

// renderSvgBytesAA rasterizes SVG at aaScale× resolution then box-filters
// down to the intrinsic size. aaScale>=2 softens curved strokes (outer bar
// frames) via supersampling; aaScale==1 is a plain 1:1 rasterize.
func renderSvgBytesAA(svgData []byte, cacheKey string, aaScale int) (*image.RGBA, error) {
	if aaScale < 1 {
		aaScale = 1
	}
	if cacheKey != "" {
		imageCacheMu.RLock()
		if cached, ok := imageCache[cacheKey]; ok {
			imageCacheMu.RUnlock()
			return cached, nil
		}
		imageCacheMu.RUnlock()
	}
	icon, err := oksvg.ReadIconStream(bytes.NewReader(svgData))
	if err != nil {
		return nil, err
	}
	w := int(icon.ViewBox.W)
	h := int(icon.ViewBox.H)
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("renderSvgBytesAA: invalid viewBox %dx%d", w, h)
	}
	bigW, bigH := w*aaScale, h*aaScale
	rgba := image.NewRGBA(image.Rect(0, 0, bigW, bigH))
	icon.SetTarget(0, 0, float64(bigW), float64(bigH))
	scanner := rasterx.NewScannerGV(bigW, bigH, rgba, rgba.Bounds())
	dasher := rasterx.NewDasher(bigW, bigH, scanner)
	icon.Draw(dasher, 1.0)

	out := rgba
	if aaScale > 1 {
		out = downscaleBox(rgba, w, h)
	}
	if cacheKey != "" {
		imageCacheMu.Lock()
		imageCache[cacheKey] = out
		imageCacheMu.Unlock()
	}
	return out, nil
}

// downscaleBox averages src into destW×destH with a box filter (used for
// supersample antialiasing of SVG strokes).
func downscaleBox(src *image.RGBA, destW, destH int) *image.RGBA {
	sb := src.Bounds()
	srcW, srcH := sb.Dx(), sb.Dy()
	if destW <= 0 || destH <= 0 || srcW <= 0 || srcH <= 0 {
		return image.NewRGBA(image.Rect(0, 0, destW, destH))
	}
	out := image.NewRGBA(image.Rect(0, 0, destW, destH))
	// Integer box sizes; leftover edge pixels are included in the last cell.
	for dy := 0; dy < destH; dy++ {
		y0 := dy * srcH / destH
		y1 := (dy + 1) * srcH / destH
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for dx := 0; dx < destW; dx++ {
			x0 := dx * srcW / destW
			x1 := (dx + 1) * srcW / destW
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var rSum, gSum, bSum, aSum, n uint32
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					off := src.PixOffset(sb.Min.X+x, sb.Min.Y+y)
					rSum += uint32(src.Pix[off+0])
					gSum += uint32(src.Pix[off+1])
					bSum += uint32(src.Pix[off+2])
					aSum += uint32(src.Pix[off+3])
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			off := out.PixOffset(dx, dy)
			out.Pix[off+0] = uint8(rSum / n)
			out.Pix[off+1] = uint8(gSum / n)
			out.Pix[off+2] = uint8(bSum / n)
			out.Pix[off+3] = uint8(aSum / n)
		}
	}
	return out
}

// barFrameAAScale is the supersample factor for outer chart frames (2× is a
// good balance of edge smoothness vs cost on the small LCD).
const barFrameAAScale = 2

// barInset keeps fills inside the grey stroke so AA frame edges stay clean.
const barInset = 1

// clampBarRadius keeps a corner radius legal for a given box height.
func clampBarRadius(radius, h int) int {
	if radius < 0 {
		radius = 0
	}
	if h > 0 && radius > h/2 {
		radius = h / 2
	}
	return radius
}

// getBarOuterFrame returns a cached anti-aliased outer frame: black fill +
// thin grey rounded stroke. Shared by CPU bars and mem/disk hbars so the
// expensive supersampled rasterize runs once per (w,h,radius).
func getBarOuterFrame(w, h, radius int) *image.RGBA {
	radius = clampBarRadius(radius, h)
	// The background colour is part of the key: the rendered frame bakes it in,
	// so a theme change must not be served the previous theme's cached bitmap.
	bg := barBgHex()
	cacheKey := "gen:barframe:aa" + strconv.Itoa(barFrameAAScale) + ":" +
		strconv.Itoa(w) + "x" + strconv.Itoa(h) + ":r" + strconv.Itoa(radius) +
		":bg" + bg

	imageCacheMu.RLock()
	if img, ok := imageCache[cacheKey]; ok {
		imageCacheMu.RUnlock()
		return img
	}
	imageCacheMu.RUnlock()

	var buf bytes.Buffer
	canvas := svg.New(&buf)
	canvas.Start(w, h)
	// Half-pixel inset keeps the 1px stroke from being clipped at the edge.
	canvas.Roundrect(0, 0, w-1, h-1, radius, radius,
		"fill:"+bg+";stroke:"+barTrackHex+";stroke-width:1")
	canvas.End()

	img, err := renderSvgBytesAA(buf.Bytes(), cacheKey, barFrameAAScale)
	if err != nil {
		log.Printf("getBarOuterFrame: %v", err)
		// Fallback empty black rect so callers still have a buffer.
		fallback := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(fallback, fallback.Bounds(), image.NewUniform(screenBgColor), image.Point{}, draw.Src)
		return fallback
	}
	return img
}

// getBarOuterStroke returns a cached AA stroke-only overlay (no fill) so fills
// cannot cover the grey frame edge; always composited last.
func getBarOuterStroke(w, h, radius int) *image.RGBA {
	radius = clampBarRadius(radius, h)
	cacheKey := "gen:barstroke:aa" + strconv.Itoa(barFrameAAScale) + ":" +
		strconv.Itoa(w) + "x" + strconv.Itoa(h) + ":r" + strconv.Itoa(radius)

	imageCacheMu.RLock()
	if img, ok := imageCache[cacheKey]; ok {
		imageCacheMu.RUnlock()
		return img
	}
	imageCacheMu.RUnlock()

	var buf bytes.Buffer
	canvas := svg.New(&buf)
	canvas.Start(w, h)
	canvas.Roundrect(0, 0, w-1, h-1, radius, radius,
		"fill:none;stroke:"+barTrackHex+";stroke-width:1")
	canvas.End()

	img, err := renderSvgBytesAA(buf.Bytes(), cacheKey, barFrameAAScale)
	if err != nil {
		log.Printf("getBarOuterStroke: %v", err)
		return image.NewRGBA(image.Rect(0, 0, w, h))
	}
	return img
}

// getBarInnerMask returns a cached AA white rounded-rect used to clip fills so
// they never spill past the frame's inner corner curve.
func getBarInnerMask(w, h, radius, inset int) *image.RGBA {
	radius = clampBarRadius(radius, h)
	if inset < 0 {
		inset = 0
	}
	innerR := radius - inset
	if innerR < 0 {
		innerR = 0
	}
	cacheKey := "gen:barmask:aa" + strconv.Itoa(barFrameAAScale) + ":" +
		strconv.Itoa(w) + "x" + strconv.Itoa(h) +
		":r" + strconv.Itoa(radius) + ":i" + strconv.Itoa(inset)

	imageCacheMu.RLock()
	if img, ok := imageCache[cacheKey]; ok {
		imageCacheMu.RUnlock()
		return img
	}
	imageCacheMu.RUnlock()

	iw := w - 2*inset
	ih := h - 2*inset
	if iw < 1 || ih < 1 {
		empty := image.NewRGBA(image.Rect(0, 0, w, h))
		imageCacheMu.Lock()
		imageCache[cacheKey] = empty
		imageCacheMu.Unlock()
		return empty
	}

	var buf bytes.Buffer
	canvas := svg.New(&buf)
	canvas.Start(w, h)
	// Opaque white interior = keep fill; transparent outside = clip.
	canvas.Roundrect(inset, inset, iw-1, ih-1, innerR, innerR, "fill:#FFFFFF")
	canvas.End()

	img, err := renderSvgBytesAA(buf.Bytes(), cacheKey, barFrameAAScale)
	if err != nil {
		log.Printf("getBarInnerMask: %v", err)
		// Fallback: solid rect inset (no rounded clip).
		fallback := image.NewRGBA(image.Rect(0, 0, w, h))
		fillRect(fallback, inset, inset, iw, ih, color.RGBA{255, 255, 255, 255})
		return fallback
	}
	return img
}

// applyAlphaMask multiplies dst's RGBA by mask alpha so fills follow the
// rounded interior (and soft AA edge) of the frame.
func applyAlphaMask(dst, mask *image.RGBA) {
	if dst == nil || mask == nil {
		return
	}
	b := dst.Bounds().Intersect(mask.Bounds())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			di := dst.PixOffset(x, y)
			mi := mask.PixOffset(x, y)
			ma := uint32(mask.Pix[mi+3])
			if ma == 0 {
				dst.Pix[di+0] = 0
				dst.Pix[di+1] = 0
				dst.Pix[di+2] = 0
				dst.Pix[di+3] = 0
				continue
			}
			if ma == 255 {
				continue
			}
			dst.Pix[di+0] = uint8(uint32(dst.Pix[di+0]) * ma / 255)
			dst.Pix[di+1] = uint8(uint32(dst.Pix[di+1]) * ma / 255)
			dst.Pix[di+2] = uint8(uint32(dst.Pix[di+2]) * ma / 255)
			dst.Pix[di+3] = uint8(uint32(dst.Pix[di+3]) * ma / 255)
		}
	}
}

// composeBarChart builds: black AA frame → clipped yellow fills → grey stroke
// on top so nothing paints outside the rounded frame.
func composeBarChart(w, h, radius int, paintFills func(layer *image.RGBA, inset, innerW, innerH, innerR int)) *image.RGBA {
	radius = clampBarRadius(radius, h)
	inset := barInset
	innerW := w - 2*inset
	innerH := h - 2*inset
	innerR := radius - inset
	if innerR < 0 {
		innerR = 0
	}

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	base := getBarOuterFrame(w, h, radius)
	draw.Draw(img, img.Bounds(), base, image.Point{}, draw.Src)

	if paintFills != nil && innerW > 0 && innerH > 0 {
		fillLayer := image.NewRGBA(image.Rect(0, 0, w, h))
		paintFills(fillLayer, inset, innerW, innerH, innerR)
		applyAlphaMask(fillLayer, getBarInnerMask(w, h, radius, inset))
		draw.Draw(img, img.Bounds(), fillLayer, image.Point{}, draw.Over)
	}

	// Stroke last: restores the AA grey edge over any fill that reached it.
	draw.Draw(img, img.Bounds(), getBarOuterStroke(w, h, radius), image.Point{}, draw.Over)
	return img
}

// fillRect draws a solid axis-aligned rectangle (no AA — fills are sharp on
// purpose; only the outer frame is supersampled).
func fillRect(dst *image.RGBA, x, y, w, h int, c color.RGBA) {
	if w <= 0 || h <= 0 || dst == nil {
		return
	}
	rect := image.Rect(x, y, x+w, y+h).Intersect(dst.Bounds())
	if rect.Empty() {
		return
	}
	draw.Draw(dst, rect, image.NewUniform(c), image.Point{}, draw.Src)
}

// cropImageAt crops the given src image starting at (x0, y0) with the specified width and height.
// It returns a new *image.RGBA whose bounds begin at (0,0).
func cropImageAt(src *image.RGBA, x0, y0, width, height int) *image.RGBA {
	// Get source image bounds.
	srcBounds := src.Bounds()
	// Optionally, clamp the cropping rectangle if it exceeds src bounds.
	if x0 < srcBounds.Min.X {
		x0 = srcBounds.Min.X
	}
	if y0 < srcBounds.Min.Y {
		y0 = srcBounds.Min.Y
	}
	if x0+width > srcBounds.Max.X {
		width = srcBounds.Max.X - x0
	}
	if y0+height > srcBounds.Max.Y {
		height = srcBounds.Max.Y - y0
	}
	// Define the source rectangle to crop.
	srcRect := image.Rect(x0, y0, x0+width, y0+height)
	// Create a new RGBA image with bounds starting at (0,0).
	cropped := image.NewRGBA(image.Rect(0, 0, width, height))
	// Copy the source rectangle into the new image.
	draw.Draw(cropped, cropped.Bounds(), src, srcRect.Min, draw.Src)
	return cropped
}

// copyImageToImageAt copies an image to an image at a specified offset. frame is the destination image, img is the source image. x0, y0 is the offset.
func copyImageToImageAt(frame *image.RGBA, img *image.RGBA, x0, y0 int) error {
	// Validate input parameters first.
	if frame == nil || img == nil {
		return fmt.Errorf("nil image provided")
	}

	targetWidth := img.Bounds().Dx()
	targetHeight := img.Bounds().Dy()

	// Check bounds.
	if x0 < 0 || y0 < 0 {
		return fmt.Errorf("x, y is negative: %d,%d", x0, y0)
	}

	imgBounds := img.Bounds()
	frameBounds := frame.Bounds()
	frameStride := frame.Stride
	imgStride := img.Stride
	
	// Ultra-fast path: bulk memory copy for opaque images with optimal alignment
	if isFullyOpaque(img) && x0 == 0 && targetWidth == frameBounds.Dx() && targetWidth == imgBounds.Dx() {
		// Perfect alignment - copy entire rows at once
		srcStart := (imgBounds.Min.Y * imgStride) + (imgBounds.Min.X * 4)
		dstStart := (y0 * frameStride)
		rowSize := targetWidth * 4 // 4 bytes per RGBA pixel
		
		for y := 0; y < targetHeight; y++ {
			if y0+y >= frameBounds.Min.Y && y0+y < frameBounds.Max.Y {
				srcOffset := srcStart + (y * imgStride)
				dstOffset := dstStart + (y * frameStride)
				
				// Add bounds checking to prevent panic
				if srcOffset >= 0 && srcOffset+rowSize <= len(img.Pix) &&
				   dstOffset >= 0 && dstOffset+rowSize <= len(frame.Pix) {
					copy(frame.Pix[dstOffset:dstOffset+rowSize], img.Pix[srcOffset:srcOffset+rowSize])
				} else {
					// Log error and use safe fallback
					log.Printf("⚠️ Ultra-fast path bounds error: y=%d, src[%d:%d] (cap:%d), dst[%d:%d] (cap:%d)",
						y, srcOffset, srcOffset+rowSize, len(img.Pix),
						dstOffset, dstOffset+rowSize, len(frame.Pix))
					
					// Safe pixel-by-pixel fallback for this row
					for x := 0; x < targetWidth; x++ {
						srcX := imgBounds.Min.X + x
						dstX := x0 + x
						if srcX >= imgBounds.Min.X && srcX < imgBounds.Max.X &&
						   dstX >= frameBounds.Min.X && dstX < frameBounds.Max.X {
							frame.SetRGBA(dstX, y0+y, img.RGBAAt(srcX, imgBounds.Min.Y+y))
						}
					}
				}
			}
		}
		return nil
	}
	
	// Fast path for fully opaque images - row-wise copy when possible
	if isFullyOpaque(img) {
		for y := 0; y < targetHeight; y++ {
			srcY := imgBounds.Min.Y + y
			dstY := y0 + y
			
			if dstY >= frameBounds.Min.Y && dstY < frameBounds.Max.Y {
				// Calculate byte offsets for this row
				srcRowStart := (srcY * imgStride) + (imgBounds.Min.X * 4)
				dstRowStart := (dstY * frameStride) + (x0 * 4)
				rowByteSize := targetWidth * 4
				
				// Bounds check for destination and buffer capacity
				if x0 >= frameBounds.Min.X && x0+targetWidth <= frameBounds.Max.X &&
				   srcRowStart >= 0 && srcRowStart+rowByteSize <= len(img.Pix) &&
				   dstRowStart >= 0 && dstRowStart+rowByteSize <= len(frame.Pix) {
					// Safe to copy entire row at once
					copy(frame.Pix[dstRowStart:dstRowStart+rowByteSize], 
						 img.Pix[srcRowStart:srcRowStart+rowByteSize])
				} else {
					// Log bounds error if it's a buffer capacity issue
					if srcRowStart+rowByteSize > len(img.Pix) || dstRowStart+rowByteSize > len(frame.Pix) {
						log.Printf("⚠️ Fast path bounds error: y=%d, src[%d:%d] (cap:%d), dst[%d:%d] (cap:%d)",
							y, srcRowStart, srcRowStart+rowByteSize, len(img.Pix),
							dstRowStart, dstRowStart+rowByteSize, len(frame.Pix))
					}
					// Pixel-by-pixel fallback for edge cases
					for x := 0; x < targetWidth; x++ {
						srcX := imgBounds.Min.X + x
						dstX := x0 + x
						
						if srcX >= imgBounds.Min.X && srcX < imgBounds.Max.X &&
						   dstX >= frameBounds.Min.X && dstX < frameBounds.Max.X {
							frame.SetRGBA(dstX, dstY, img.RGBAAt(srcX, srcY))
						}
					}
				}
			}
		}
		return nil
	}

	// Fallback for images with transparency - use alpha blending
	for y := 0; y < targetHeight; y++ {
		for x := 0; x < targetWidth; x++ {
			sample := img.RGBAAt(x, y)
			// Skip fully transparent pixels.
			if sample.A == 0 {
				continue
			}

			// Get the destination pixel.
			dst := frame.RGBAAt(x0+x, y0+y)
			if sample.A == 255 {
				// Fully opaque: copy sample pixel directly.
				frame.SetRGBA(x0+x, y0+y, sample)
			} else {
				// Mix sample and destination pixels.
				a := uint16(sample.A)
				invA := uint16(255 - sample.A)
				outR := (uint16(sample.R)*a + uint16(dst.R)*invA) / 255
				outG := (uint16(sample.G)*a + uint16(dst.G)*invA) / 255
				outB := (uint16(sample.B)*a + uint16(dst.B)*invA) / 255
				// For the alpha channel, use the over operator:
				// outA = sample.A + dst.A*(255-sample.A)/255
				outA := uint8(uint16(sample.A) + (uint16(dst.A)*invA)/255)
				frame.SetRGBA(x0+x, y0+y, color.RGBA{
					R: uint8(outR),
					G: uint8(outG),
					B: uint8(outB),
					A: outA,
				})
			}
		}
	}

	return nil
}

// stitchFramesOptimized combines two frames horizontally in a single operation for maximum performance
func stitchFramesOptimized(dst *image.RGBA, leftFrame, rightFrame *image.RGBA) error {
	// Validate inputs
	if dst == nil || leftFrame == nil || rightFrame == nil {
		return fmt.Errorf("nil image provided to stitchFramesOptimized")
	}
	
	leftBounds := leftFrame.Bounds()
	rightBounds := rightFrame.Bounds()
	dstBounds := dst.Bounds()
	
	leftWidth := leftBounds.Dx()
	leftHeight := leftBounds.Dy()
	rightWidth := rightBounds.Dx()
	rightHeight := rightBounds.Dy()
	
	// Ensure frames are the same height and fit in destination
	if leftHeight != rightHeight {
		return fmt.Errorf("frames must have same height: left=%d, right=%d", leftHeight, rightHeight)
	}
	if leftWidth + rightWidth > dstBounds.Dx() || leftHeight > dstBounds.Dy() {
		return fmt.Errorf("combined frames exceed destination bounds")
	}
	
	// Ultra-fast path: if both frames have simple bounds starting at (0,0), use bulk copy
	if leftBounds.Min.X == 0 && leftBounds.Min.Y == 0 && rightBounds.Min.X == 0 && rightBounds.Min.Y == 0 &&
	   leftFrame.Stride == leftWidth*4 && rightFrame.Stride == rightWidth*4 && dst.Stride == (leftWidth+rightWidth)*4 {
		
		// Interleave copy: copy rows from both frames simultaneously
		for y := 0; y < leftHeight; y++ {
			leftRowStart := y * leftWidth * 4
			rightRowStart := y * rightWidth * 4
			dstRowStart := y * (leftWidth + rightWidth) * 4
			
			// Copy left frame row
			copy(dst.Pix[dstRowStart:dstRowStart+leftWidth*4], 
				 leftFrame.Pix[leftRowStart:leftRowStart+leftWidth*4])
			
			// Copy right frame row immediately after
			copy(dst.Pix[dstRowStart+leftWidth*4:dstRowStart+leftWidth*4+rightWidth*4], 
				 rightFrame.Pix[rightRowStart:rightRowStart+rightWidth*4])
		}
		return nil
	}
	
	// Standard path with stride calculations (fallback for complex bounds)
	leftStride := leftFrame.Stride
	rightStride := rightFrame.Stride
	dstStride := dst.Stride
	
	// Copy both frames with proper stride handling
	for y := 0; y < leftHeight; y++ {
		// Source row offsets
		leftSrcRow := (leftBounds.Min.Y + y) * leftStride + leftBounds.Min.X * 4
		rightSrcRow := (rightBounds.Min.Y + y) * rightStride + rightBounds.Min.X * 4
		
		// Destination row offsets
		dstLeftRow := y * dstStride
		dstRightRow := y * dstStride + leftWidth * 4
		
		// Bounds checking
		if leftSrcRow + leftWidth*4 <= len(leftFrame.Pix) &&
		   rightSrcRow + rightWidth*4 <= len(rightFrame.Pix) &&
		   dstLeftRow + leftWidth*4 <= len(dst.Pix) &&
		   dstRightRow + rightWidth*4 <= len(dst.Pix) {
			
			// Copy left frame row
			copy(dst.Pix[dstLeftRow:dstLeftRow+leftWidth*4], 
				 leftFrame.Pix[leftSrcRow:leftSrcRow+leftWidth*4])
			
			// Copy right frame row immediately after
			copy(dst.Pix[dstRightRow:dstRightRow+rightWidth*4], 
				 rightFrame.Pix[rightSrcRow:rightSrcRow+rightWidth*4])
		}
	}
	
	return nil
}

// isFullyOpaque checks if an image is fully opaque (no transparency)
func isFullyOpaque(img *image.RGBA) bool {
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if img.RGBAAt(x, y).A < 255 {
				return false
			}
		}
	}
	return true
}

// copyImageRegion efficiently copies a region from src to dst using bulk memory operations
func copyImageRegion(dst *image.RGBA, src *image.RGBA, x0, y0, width, height int) {
	srcBounds := src.Bounds()
	dstBounds := dst.Bounds()
	
	// Clamp to valid bounds
	if x0 < srcBounds.Min.X {
		width -= srcBounds.Min.X - x0
		x0 = srcBounds.Min.X
	}
	if y0 < srcBounds.Min.Y {
		height -= srcBounds.Min.Y - y0
		y0 = srcBounds.Min.Y
	}
	
	if x0+width > srcBounds.Max.X {
		width = srcBounds.Max.X - x0
	}
	if y0+height > srcBounds.Max.Y {
		height = srcBounds.Max.Y - y0
	}
	
	// Early return if invalid region
	if width <= 0 || height <= 0 {
		return
	}
	
	// Ultra-fast bulk memory copy using direct Pix access
	srcStride := src.Stride
	dstStride := dst.Stride
	rowByteSize := width * 4 // 4 bytes per RGBA pixel
	
	for y := 0; y < height; y++ {
		srcY := y0 + y
		dstY := y
		
		if srcY >= srcBounds.Min.Y && srcY < srcBounds.Max.Y && 
		   dstY >= dstBounds.Min.Y && dstY < dstBounds.Max.Y {
			
			// Calculate byte offsets for this row
			srcOffset := (srcY * srcStride) + (x0 * 4)
			dstOffset := (dstY * dstStride)
			
			// Bounds check to ensure we don't go out of bounds
			if srcOffset >= 0 && srcOffset+rowByteSize <= len(src.Pix) &&
			   dstOffset >= 0 && dstOffset+rowByteSize <= len(dst.Pix) {
				// Ultra-fast row copy using built-in copy function
				copy(dst.Pix[dstOffset:dstOffset+rowByteSize], 
					 src.Pix[srcOffset:srcOffset+rowByteSize])
			} else {
				// Fallback to pixel-by-pixel for edge cases (shouldn't happen in normal use)
				for x := 0; x < width; x++ {
					srcX := x0 + x
					dstX := x
					
					if srcX >= srcBounds.Min.X && srcX < srcBounds.Max.X &&
					   dstX >= dstBounds.Min.X && dstX < dstBounds.Max.X {
						dst.SetRGBA(dstX, dstY, src.RGBAAt(srcX, srcY))
					}
				}
			}
		}
	}
}

func drawRoundedRect(gc *draw2dimg.GraphicContext, x, y, w, h, r float64) {
	// Start at the top-left corner, offset by the radius.
	gc.MoveTo(x+r, y)
	// Draw top edge.
	gc.LineTo(x+w-r, y)
	// Top-right arc.
	gc.ArcTo(x+w-r, y+r, r, r, -90, 90)
	// Right edge.
	gc.LineTo(x+w, y+h-r)
	// Bottom-right arc.
	gc.ArcTo(x+w-r, y+h-r, r, r, 0, 90)
	// Bottom edge.
	gc.LineTo(x+r, y+h)
	// Bottom-left arc.
	gc.ArcTo(x+r, y+h-r, r, r, 90, 90)
	// Left edge.
	gc.LineTo(x, y+r)
	// Top-left arc.
	gc.ArcTo(x+r, y+r, r, r, 180, 90)
	gc.Close()
}

func drawRect(img *image.RGBA, x0, y0, width, height int, c color.Color) {
    // Convert the color.Color to a color.RGBA.
    r, g, b, a := c.RGBA()
    // The RGBA() method returns values in the range [0, 65535],
    // so we need to shift them to [0, 255].
    col := color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}

    for x := x0; x < x0+width; x++ {
        for y := y0; y < y0+height; y++ {
            img.SetRGBA(x, y, col)
        }
    }
}

func drawSignalStrength(frame *image.RGBA, x0, y0 int, strength float64) {
	xBarSize := 5
	yBarSize := 15
	barSpace := 1
	numBars := 4
	yMinHeight := 6
	strengthInt := int(math.Ceil(strength * 4))
	cacheKey := "gen:strength-" + strconv.Itoa(strengthInt)

	imageCacheMu.RLock()
	img, cached := imageCache[cacheKey]
	imageCacheMu.RUnlock()
	if !cached {
		var buf bytes.Buffer
		canvas := svg.New(&buf)
		canvas.Start(xBarSize*numBars+barSpace*(numBars-1), yBarSize+yMinHeight)

		for i := 0; i < numBars; i++ {
			if i < strengthInt {
				canvas.Roundrect(i*xBarSize+i*barSpace, yBarSize/4*(4-i), xBarSize, yBarSize/4*i+yMinHeight, 2, 2, "fill:white")
			}else{
				fillColorHex := fmt.Sprintf("#%02X%02X%02X", PCAT_GREY.R, PCAT_GREY.G, PCAT_GREY.B)
				canvas.Roundrect(i*xBarSize+i*barSpace, yBarSize/4*(4-i), xBarSize, yBarSize/4*i+yMinHeight, 2, 2, "fill:" + fillColorHex)
			}
		}
		canvas.End()

		var err error
		img, err = renderSvgBytes(buf.Bytes(), cacheKey)
		if err != nil {
			panic(err)
		}
	}
	copyImageToImageAt(frame, img, x0, y0)
}

// Bar-chart styling shared by the CPU and memory meters. Colors reuse the boot
// progress bar's palette (yellow fill on a grey track); the corner radii echo
// its rounded look, scaled down for the small in-page bars.
var (
	barFillHex  = fmt.Sprintf("#%02X%02X%02X", PCAT_YELLOW.R, PCAT_YELLOW.G, PCAT_YELLOW.B)
	barTrackHex = fmt.Sprintf("#%02X%02X%02X", PCAT_GREY.R, PCAT_GREY.G, PCAT_GREY.B)
)

// barBgHex is the unfilled track colour of the CPU/disk/memory meters. It
// follows the theme's screen background rather than being hardcoded black:
// the bars sit directly on the page, so a black track on a themed background
// (e.g. ember's [26,7,2]) drew as a dark rectangle punched out of the page.
//
// It is a function, not a var, because setScreenBgColor can change the
// background at runtime when a theme is applied — a value computed once at
// init would keep the startup colour forever.
func barBgHex() string {
	return fmt.Sprintf("#%02X%02X%02X", screenBgColor.R, screenBgColor.G, screenBgColor.B)
}

const (
	// Shared outer-frame corner radius for CPU bars and the mem hbar so both
	// charts match visually.
	hbarRadius = 5 // also used by drawCpuBars outer frame
)

// elementFillColor resolves a bar chart's fill from the element's optional
// [R,G,B] color (set by web color themes), defaulting to the classic yellow.
func elementFillColor(element DisplayElement) color.RGBA {
	if len(element.Color) >= 3 {
		return color.RGBA{uint8(element.Color[0]), uint8(element.Color[1]), uint8(element.Color[2]), 255}
	}
	return PCAT_YELLOW
}

// drawCpuBars renders a framed box of vertical bars — one per CPU core — at
// (x0, y0) with total size w×h. Fills are clipped to the rounded interior and
// the grey stroke is drawn last so nothing sits outside the frame; full
// height fills follow the top inner corner curve via the clip mask.
func drawCpuBars(frame *image.RGBA, x0, y0, w, h int, usages []float64, fill color.RGBA) {
	numCores := len(usages)
	if numCores == 0 || w <= 0 || h <= 0 {
		return
	}

	// "v5": inner clip mask + stroke-on-top (no fill outside rounded frame).
	var keyBuf strings.Builder
	keyBuf.WriteString("gen:cpubars:v5:")
	fmt.Fprintf(&keyBuf, "c%02x%02x%02x:", fill.R, fill.G, fill.B)
	keyBuf.WriteString(strconv.Itoa(w))
	keyBuf.WriteByte('x')
	keyBuf.WriteString(strconv.Itoa(h))
	for _, u := range usages {
		bucket := int(u) / 5
		keyBuf.WriteByte(':')
		keyBuf.WriteString(strconv.Itoa(bucket))
	}
	cacheKey := keyBuf.String()

	imageCacheMu.RLock()
	img, cached := imageCache[cacheKey]
	imageCacheMu.RUnlock()
	if !cached {
		img = composeBarChart(w, h, hbarRadius, func(layer *image.RGBA, inset, innerW, innerH, innerR int) {
			const gap = 1
			avail := innerW - gap*(numCores-1)
			if avail < numCores {
				avail = innerW
			}
			for i := 0; i < numCores; i++ {
				barStart := i * avail / numCores
				barEnd := (i + 1) * avail / numCores
				barW := barEnd - barStart
				if barW < 1 {
					continue
				}
				bx := inset + barStart + i*gap
				u := usages[i]
				if u < 0 {
					u = 0
				} else if u > 100 {
					u = 100
				}
				fillH := 0
				if u > 0 {
					fillH = int(math.Round(float64(innerH) * u / 100.0))
					if fillH < 1 {
						fillH = 1
					}
					if fillH > innerH {
						fillH = innerH
					}
				}
				if fillH > 0 {
					// From bottom of the inner box; top edge is clipped to the
					// rounded mask so full bars match the frame's top corners.
					fy := inset + (innerH - fillH)
					fillRect(layer, bx, fy, barW, fillH, fill)
				}
			}
		})

		imageCacheMu.Lock()
		imageCache[cacheKey] = img
		imageCacheMu.Unlock()
	}
	copyImageToImageAt(frame, img, x0, y0)
}

// diskBar is one labelled usage bar in the disk_bars slot.
type diskBar struct {
	label string
	pct   float64
}

// diskBarLabelFonts lists candidate label faces, largest first. The face is
// picked per layout so three bars shrink their text instead of overflowing.
var diskBarLabelFonts = []string{"unit", "tiny", "micro"}

// drawDiskBars renders onboard (root/eMMC) usage plus the NVMe drive and SD
// card when they are present, as up to three horizontal bars sharing the slot
// width with a small gap. Each bar carries its name ("eMMC"/"NVMe"/"SD")
// centered on it. An SD card that is in the slot but not mounted has no usage
// to report and draws as an empty bar. Height should match the mem hbar. Works
// the same on OpenWrt and Debian.
func drawDiskBars(frame *image.RGBA, x0, y0, w, h int, fill color.RGBA) {
	if w <= 0 || h <= 0 {
		return
	}
	bars := []diskBar{{"eMMC", diskBarPercent("DiskUsagePercent")}}
	if diskBarPresent("DiskNvmePresent") {
		bars = append(bars, diskBar{"NVMe", diskBarPercent("DiskNvmePercent")})
	}
	if diskBarPresent("DiskSDPresent") {
		bars = append(bars, diskBar{"SD", diskBarPercent("DiskSDPercent")})
	}

	const gap = 4
	n := len(bars)
	avail := w - gap*(n-1)
	// Too narrow to split: fall back to the onboard bar alone at full width.
	if avail < 2*n {
		bars, n, avail = bars[:1], 1, w
	}

	labels := make([]string, n)
	widths := make([]int, n)
	xs := make([]int, n)
	for i := range bars {
		start, end := i*avail/n, (i+1)*avail/n
		labels[i] = bars[i].label
		widths[i] = end - start
		xs[i] = x0 + start + i*gap
	}
	face, haveFace := pickBarLabelFace(labels, widths, h)
	for i := range bars {
		drawHBar(frame, xs[i], y0, widths[i], h, bars[i].pct, "", fill)
		if haveFace {
			drawTinyBarLabel(frame, xs[i]+widths[i]/2, y0+h/2, labels[i], face)
		}
	}
}

// diskBarPercent reads a 0-100 usage value from globalData, 0 when missing.
func diskBarPercent(key string) float64 {
	if v, ok := globalData.Load(key); ok && v != nil {
		switch n := v.(type) {
		case float64:
			return n
		case int:
			return float64(n)
		}
	}
	return 0
}

// diskBarPresent reads a disk presence flag from globalData.
func diskBarPresent(key string) bool {
	if v, ok := globalData.Load(key); ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

// pickBarLabelFace returns the largest face from diskBarLabelFonts that stays
// within barH tall and leaves labelSidePad px of clear track on both sides of
// every label in its own bar, so the names always fit. Falls back to the
// smallest candidate when even that is too big; ok is false when no face could
// be loaded at all.
func pickBarLabelFace(labels []string, widths []int, barH int) (face font.Face, ok bool) {
	const labelSidePad = 3
	for _, name := range diskBarLabelFonts {
		f, fh, err := getFontFace(name)
		if err != nil {
			continue
		}
		face, ok = f, true
		fits := fh <= barH
		for i, l := range labels {
			if !fits {
				break
			}
			if font.MeasureString(f, l).Ceil()+2*labelSidePad > widths[i] {
				fits = false
			}
		}
		if fits {
			return f, true
		}
	}
	return face, ok
}

// drawTinyBarLabel centers a label with a 1px black aura so it stays readable
// on yellow fill and empty track (disk bar names).
func drawTinyBarLabel(frame *image.RGBA, cx, cy int, label string, face font.Face) {
	if frame == nil || label == "" || face == nil {
		return
	}
	drawTextWithAura(frame, label, cx, cy, face, PCAT_WHITE, PCAT_BLACK)
}

// hbarFillSVG builds the fill for the horizontal bar in the given hex color.
// full=true → all corners use radius fr (seats into the rounded frame at 100%).
// full=false → left corners rounded to match the track; right edge is sharp.
func hbarFillSVG(canvasW, canvasH, x, y, fillW, fillH, fr int, full bool, fillHex string) []byte {
	var buf bytes.Buffer
	canvas := svg.New(&buf)
	canvas.Start(canvasW, canvasH)
	if fillW <= 0 || fillH <= 0 {
		canvas.End()
		return buf.Bytes()
	}
	if full || fr <= 0 {
		canvas.Roundrect(x, y, fillW, fillH, fr, fr, "fill:"+fillHex)
		canvas.End()
		return buf.Bytes()
	}
	// Left-rounded, right-sharp (y grows downward).
	x0, y0 := x, y
	x1, y1 := x+fillW, y+fillH
	r := fr
	d := fmt.Sprintf(
		"M%d %d H%d V%d H%d A%d %d 0 0 1 %d %d V%d A%d %d 0 0 1 %d %d Z",
		x0+r, y0,
		x1,
		y1,
		x0+r,
		r, r, x0, y1-r,
		y0+r,
		r, r, x0+r, y0,
	)
	canvas.Path(d, "fill:"+fillHex)
	canvas.End()
	return buf.Bytes()
}

// drawHBar renders a framed horizontal progress bar at (x0, y0) of size w×h.
// The fill (classic yellow, or a theme color) is rounded to the inner corner
// radius (right edge matches the frame when full), clipped to the interior,
// with the grey stroke drawn last. Optional label is centered with a 1px
// black aura (e.g. "3.2/16GB").
func drawHBar(frame *image.RGBA, x0, y0, w, h int, pct float64, label string, fill color.RGBA) {
	if w <= 0 || h <= 0 {
		return
	}
	if pct < 0 {
		pct = 0
	} else if pct > 100 {
		pct = 100
	}

	fillHex := fmt.Sprintf("#%02X%02X%02X", fill.R, fill.G, fill.B)
	bucket := int(pct)
	// "v3": inner mask clip + stroke-on-top + matching inner fill radius.
	cacheKey := "gen:hbar:v3:c" + fillHex + ":" + strconv.Itoa(w) + "x" + strconv.Itoa(h) + ":" + strconv.Itoa(bucket)

	imageCacheMu.RLock()
	img, cached := imageCache[cacheKey]
	imageCacheMu.RUnlock()
	if !cached {
		img = composeBarChart(w, h, hbarRadius, func(layer *image.RGBA, inset, innerW, innerH, innerR int) {
			fillW := 0
			if pct > 0 && innerW > 0 {
				fillW = int(math.Round(float64(innerW) * pct / 100.0))
				if fillW < 1 {
					fillW = 1
				}
				if fillW > innerW {
					fillW = innerW
				}
			}
			if fillW <= 0 || innerH <= 0 {
				return
			}
			// Full (100%): all corners match the frame's inner radius.
			// Partial: left seats into the track curve; right edge is always
			// sharp so mid-track ends don't look inconsistently rounded.
			full := fillW >= innerW
			fr := innerR
			if fr > fillW/2 {
				fr = fillW / 2
			}
			if fr > innerH/2 {
				fr = innerH / 2
			}

			svgData := hbarFillSVG(w, h, inset, inset, fillW, innerH, fr, full, fillHex)
			shape := "full"
			if !full {
				shape = "sharpR"
			}
			fillKey := "gen:hbarfill:aa" + strconv.Itoa(barFrameAAScale) + ":c" + fillHex + ":" +
				strconv.Itoa(w) + "x" + strconv.Itoa(h) +
				":fw" + strconv.Itoa(fillW) + ":fh" + strconv.Itoa(innerH) +
				":r" + strconv.Itoa(fr) + ":i" + strconv.Itoa(inset) + ":" + shape
			fillImg, err := renderSvgBytesAA(svgData, fillKey, barFrameAAScale)
			if err != nil {
				fillRect(layer, inset, inset, fillW, innerH, fill)
			} else {
				draw.Draw(layer, layer.Bounds(), fillImg, image.Point{}, draw.Over)
			}
		})

		imageCacheMu.Lock()
		imageCache[cacheKey] = img
		imageCacheMu.Unlock()
	}
	copyImageToImageAt(frame, img, x0, y0)

	if label != "" {
		face, _, err := getFontFace("unit")
		if err != nil {
			face, _, err = getFontFace("tiny")
		}
		if err == nil {
			drawTextWithAura(frame, label, x0+w/2, y0+h/2, face, PCAT_WHITE, PCAT_BLACK)
		}
	}
}

// drawTextWithAura draws text centered on (cx, cy) with a 1px outline (aura)
// in auraClr around the foreground fg. Used for overlays on busy backgrounds
// like the memory bar fill.
func drawTextWithAura(img *image.RGBA, text string, cx, cy int, face font.Face, fg, auraClr color.Color) {
	if img == nil || text == "" || face == nil {
		return
	}
	d := &font.Drawer{Face: face}
	tw := d.MeasureString(text).Round()
	metrics := face.Metrics()
	ascent := metrics.Ascent.Round()
	descent := metrics.Descent.Round()
	th := ascent + descent
	// Top-left of the text box centered on (cx, cy); baseline = top + ascent.
	x := cx - tw/2
	baseline := cy - th/2 + ascent

	// 8-neighbour outline, then the fill on top.
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			if dx == 0 && dy == 0 {
				continue
			}
			drawTextAtBaseline(img, text, x+dx, baseline+dy, face, auraClr)
		}
	}
	drawTextAtBaseline(img, text, x, baseline, face, fg)
}

// drawTextAtBaseline draws text with its baseline at (x, baselineY) — no
// extra ascent offset (unlike drawText which treats y as top-ish).
func drawTextAtBaseline(img *image.RGBA, text string, x, baselineY int, face font.Face, clr color.Color) {
	if img == nil || text == "" || face == nil {
		return
	}
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(clr),
		Face: face,
		Dot:  fixed.P(x, baselineY),
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("drawTextAtBaseline panic recovered: %v", r)
		}
	}()
	d.DrawString(text)
}

// renderAndCache rasterizes the SVG buffer under cacheKey and blits it. Small
// helper used by the early-return path in drawCpuBars.
func renderAndCache(buf *bytes.Buffer, cacheKey string, frame *image.RGBA, x0, y0 int) {
	img, err := renderSvgBytes(buf.Bytes(), cacheKey)
	if err != nil {
		log.Printf("renderAndCache: render error: %v", err)
		return
	}
	copyImageToImageAt(frame, img, x0, y0)
}

// batteryBodyRadius is a modest corner radius so the top-bar battery reads
// soft without looking like a capsule.
const batteryBodyRadius = 3

// drawBattery paints the top-bar battery glyph: AA rounded body + nub, SOC
// empty-region shade (preserving edge alpha), percent text, optional bolt.
// Results are cached per size/SOC/charging so the supersample is rare.
func drawBattery(w, h int, soc float64, isCharging bool, x0, y0 int) *image.RGBA {
	if w <= 0 || h <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}
	if soc < 0 {
		soc = 0
	} else if soc > 100 {
		soc = 100
	}
	socBucket := int(soc) // 1% cache granularity matches the label

	// "v5" after the body moved off the top edge and the digits started
	// centering on their ink box instead of their advance width.
	cacheKey := "gen:batt:aa" + strconv.Itoa(barFrameAAScale) + ":" +
		strconv.Itoa(w) + "x" + strconv.Itoa(h) +
		":s" + strconv.Itoa(socBucket) +
		":c" + strconv.FormatBool(isCharging) + ":v5"
	imageCacheMu.RLock()
	if cached, ok := imageCache[cacheKey]; ok {
		imageCacheMu.RUnlock()
		return cached
	}
	imageCacheMu.RUnlock()

	terminalWidth := 3
	terminalH := 6
	if terminalH > h {
		terminalH = h
	}
	bodyW := w - terminalWidth
	if bodyW < 1 {
		bodyW = w
		terminalWidth = 0
	}

	var colorMain, colorShaded color.RGBA
	if soc < 20 {
		colorMain = PCAT_RED
	} else if isCharging {
		colorMain = PCAT_GREEN
	} else {
		colorMain = PCAT_WHITE
	}
	colorShaded = PCAT_GREY

	r := batteryBodyRadius
	if r > h/2 {
		r = h / 2
	}
	mainHex := fmt.Sprintf("#%02X%02X%02X", colorMain.R, colorMain.G, colorMain.B)

	// The body is a pixel shorter than the canvas so the AA edge of the curve
	// isn't clipped. That spare row belongs above the body, not below: the
	// glyph is blitted at the top bar's origin, so leaving it at the bottom
	// hangs the battery a pixel over the clock and wifi icon beside it.
	bodyTop := 1
	if h < 3 {
		bodyTop = 0
	}
	bodyH := h - bodyTop

	// Supersampled rounded body + terminal nub (same AA path as chart frames).
	var buf bytes.Buffer
	canvas := svg.New(&buf)
	canvas.Start(w, h)
	canvas.Roundrect(0, bodyTop, bodyW-1, bodyH, r, r, "fill:"+mainHex)
	if terminalWidth > 0 {
		ty := bodyTop + (bodyH-terminalH)/2
		tr := 1
		if tr > terminalH/2 {
			tr = terminalH / 2
		}
		canvas.Roundrect(bodyW, ty, terminalWidth-1, terminalH, tr, tr, "fill:"+mainHex)
	}
	canvas.End()

	img, err := renderSvgBytesAA(buf.Bytes(), "", barFrameAAScale)
	if err != nil {
		log.Printf("drawBattery SVG: %v", err)
		img = image.NewRGBA(image.Rect(0, 0, w, h))
		fillRect(img, 0, 0, bodyW, h, colorMain)
		if terminalWidth > 0 {
			fillRect(img, bodyW, h/2-terminalH/2, terminalWidth, terminalH, colorMain)
		}
	}

	// Empty capacity: recolour non-transparent pixels from startShadeX rightward
	// so the AA edge soft-alpha is kept (just tinted grey).
	startShadeX := int(math.Round((soc / 100.0) * float64(bodyW)))
	if startShadeX < 0 {
		startShadeX = 0
	}
	if startShadeX < w {
		for y := 0; y < h; y++ {
			for x := startShadeX; x < w; x++ {
				c := img.RGBAAt(x, y)
				if c.A == 0 {
					continue
				}
				// Premultiplied-ish: keep edge alpha, swap RGB to shaded.
				img.SetRGBA(x, y, color.RGBA{
					R: colorShaded.R,
					G: colorShaded.G,
					B: colorShaded.B,
					A: c.A,
				})
			}
		}
	}

	face, _, err := getFontFace("clock")
	if err != nil {
		fmt.Println("Error loading font:", err)
		imageCacheMu.Lock()
		imageCache[cacheKey] = img
		imageCacheMu.Unlock()
		return img
	}

	textColor := PCAT_BLACK
	if soc < 20 {
		textColor = PCAT_WHITE
	}
	batteryText := strconv.Itoa(socBucket)
	drawChargingBlot := isCharging && socBucket < 100

	// Measure text + optional bolt, then center the whole group in the body
	// (excluding the terminal nub). Both axes use *ink* bounds, not font
	// metrics: Orbitron has a tall em-box so metric-only vertical centering
	// sits too low, and its digits carry uneven side bearings, so centering by
	// advance width leaves values like "70" about 2px left of centre.
	d := &font.Drawer{Face: face}
	textW := d.MeasureString(batteryText).Round()
	metrics := face.Metrics()
	ascent := metrics.Ascent.Round()
	descent := metrics.Descent.Round()

	// Ink box of the whole string: X runs right from the pen origin and stays in
	// 26.6 fixed point so centering rounds once, at the end; Y is relative to
	// the baseline (Min.Y ≤ 0 above, Max.Y ≥ 0 below).
	inkTop, inkBot := 0, 0 // pixels relative to baseline; top is ≤ 0
	var inkLeft, inkRight fixed.Int26_6
	inkSet := false
	pen := fixed.Int26_6(0)
	for _, r := range batteryText {
		b, adv, ok := face.GlyphBounds(r)
		if !ok {
			continue
		}
		top := b.Min.Y.Ceil() // more negative = higher above baseline
		bot := b.Max.Y.Ceil()
		left := pen + b.Min.X
		right := pen + b.Max.X
		pen += adv
		if !inkSet {
			inkTop, inkBot = top, bot
			inkLeft, inkRight = left, right
			inkSet = true
			continue
		}
		if top < inkTop {
			inkTop = top
		}
		if bot > inkBot {
			inkBot = bot
		}
		if left < inkLeft {
			inkLeft = left
		}
		if right > inkRight {
			inkRight = right
		}
	}
	if !inkSet || inkBot <= inkTop {
		// Fallback to em-box / advance width if bounds are unavailable.
		inkTop = -ascent
		inkBot = descent
		inkLeft, inkRight = 0, fixed.I(textW)
	}
	var chargingBolt *image.RGBA
	boltW, boltH := 0, 0
	const boltGap = 1
	if drawChargingBlot {
		var berr error
		if soc < 20 {
			chargingBolt, _, _, berr = loadImage(assetsPrefix + "/assets/svg/blotWhite.svg")
		} else {
			chargingBolt, _, _, berr = loadImage(assetsPrefix + "/assets/svg/blotBlack.svg")
		}
		if berr != nil {
			fmt.Println("Error loading charging bolt:", berr)
			drawChargingBlot = false
		} else {
			boltW = chargingBolt.Bounds().Dx()
			boltH = chargingBolt.Bounds().Dy()
		}
	}

	// Pen origin that leaves equal clearance either side of the ink (digits plus
	// bolt and its gap): solving gapLeft == gapRight gives
	// pen = (drawnBodyW - inkLeft - inkRight)/2. Centre on the body as *drawn*
	// — the Roundrect above is bodyW-1 wide — or the label lands half a pixel
	// right of the glyph it sits in.
	drawnBodyW := bodyW - 1
	if drawnBodyW < 1 {
		drawnBodyW = bodyW
	}
	groupRight := inkRight
	if drawChargingBlot {
		groupRight += fixed.I(boltGap + boltW)
	}
	startX := ((fixed.I(drawnBodyW) - inkLeft - groupRight) / 2).Round()

	// Place ink center on the body mid-line:
	// baseY + (inkTop+inkBot)/2 = bodyTop + bodyH/2. Rounded rather than
	// truncated, or an odd body height parks the digits half a pixel high.
	baseY := int(math.Round(float64(bodyTop) + float64(bodyH)/2 - float64(inkTop+inkBot)/2))
	if baseY+inkTop < bodyTop {
		baseY = bodyTop - inkTop
	}
	if baseY+inkBot > bodyTop+bodyH {
		baseY = bodyTop + bodyH - inkBot
	}

	drawTextAtBaseline(img, batteryText, startX, baseY, face, textColor)
	if drawChargingBlot && chargingBolt != nil {
		// Vertically center bolt on the same ink mid-line as the digits.
		inkMid := baseY + (inkTop+inkBot)/2
		boltY := inkMid - boltH/2
		if boltY < 0 {
			boltY = 0
		}
		copyImageToImageAt(img, chargingBolt, startX+inkRight.Ceil()+boltGap, boltY)
	}

	imageCacheMu.Lock()
	imageCache[cacheKey] = img
	imageCacheMu.Unlock()
	return img
}


func drawTopBar(display gc9307.Device, frame *image.RGBA) {
	if renderTopBar(frame) {
		sendTopBar(display, frame)
	}
}

// isCellular reports whether a top-bar networkStr denotes a mobile uplink —
// "5"/"4"/"3" for a known generation, "c" when the modem is the egress but its
// generation is unknown. All of them draw signal bars.
func isCellular(networkStr string) bool {
	switch networkStr {
	case "5", "4", "3", "c":
		return true
	}
	return false
}

// renderTopBar draws the top bar into frame without touching the display.
// Returns false when nothing was rendered (cache hit or a load error), in
// which case the frame contents are not meaningful for sending.
func renderTopBar(frame *image.RGBA) bool {
	var timeStr string
	var networkStr string
	// A placeholder until both the timezone is known (the daemon can start
	// before the boot has produced /tmp/localtime - see timezone.go) and the
	// clock has been set (year check): never show a wrong time.
	currDateTime, tzKnown := displayNow()
	if !tzKnown || currDateTime.Year() < 2025 {
		timeStr = "--:--"
	} else {
		timeStr = fmt.Sprintf("%02d:%02d", currDateTime.Hour(), currDateTime.Minute())
	}

	gatewayDevice, _ := globalData.Load("GatewayDevice")
	activeEgress, _ := globalData.Load("ActiveEgress")
	// Radio generation as "5"/"4"/"3", already normalized from modem_mode with
	// carrier as the fallback (see cellGeneration) — carrier alone reads
	// "Other" on an RM500Q-GL, which used to leave the top bar with no icon.
	cellGen, _ := globalData.Load("CellGeneration")
	cellGenStr, _ := cellGen.(string)

	// Prefer the precise active egress (which can also be "wifi" in Smart WAN
	// mode) over the coarse wired/mobile gateway hint.
	// networkStr: "5"/"4"/"3" cellular, "w" ethernet, "i" WiFi (Smart WAN),
	// "" (empty) when no WAN is secured yet.
	if activeEgress == "wifi" {
		networkStr = "i"
	} else if activeEgress == "wan" || activeEgress == "lan" {
		networkStr = "w"
	} else if activeEgress == "mobile" || gatewayDevice == "mobile" {
		// Mobile is carrying the traffic, so the bars are meaningful even when
		// the modem won't say which generation it is on: fall back to "c" and
		// draw the bars with no generation digit rather than nothing at all.
		networkStr = cellGenStr
		if networkStr == "" {
			networkStr = "c"
		}
	}else if gatewayDevice == "wired"{
		networkStr = "w"
	}else{
		// No WAN secured yet (e.g. at startup before pcat-manager-web reports
		// a real egress, or when it reports none). Show no network icon rather
		// than falsely claiming ethernet.
		networkStr = ""
	}
	signalStrength := 0.43
	// Resolve the cellular signal level for the cache key so the bar refreshes
	// when it changes. (WiFi-as-WAN draws a static icon, so it needs no level.)
	if isCellular(networkStr) {
		if v, ok := globalData.Load("ModemSignalStrength"); ok {
			if vi, ok := v.(int); ok {
				signalStrength = float64(vi) / 100.0
			}
		}
	}
	// configVersion joins the key for the same reason as the footer: a theme
	// change must repaint now, not whenever the clock minute happens to roll.
	magicStr := timeStr + " " + strconv.Itoa(int(signalStrength*100)) + " " + networkStr + " " +
		strconv.Itoa(int(battSOC)) + " " + strconv.FormatBool(battChargingStatus) + " " +
		strconv.Itoa(configVersion)

	if cacheTopBarStr == magicStr {
		return false //no need to refresh
	}

	topBarFrameWidth := PCAT2_LCD_WIDTH
	topBarFrameHeight := PCAT2_TOP_BAR_HEIGHT

	clearFrame(frame, topBarFrameWidth, topBarFrameHeight)
	
	faceClock, _, err := getFontFace("clock")
	faceTiny, _, err := getFontFace("tiny")
	if err != nil {
		fmt.Println("Error loading font:", err)
		return false
	}
	fiveGonTop :=true


	x0 := PCAT2_L_MARGIN
	y0 := PCAT2_T_MARGIN

	//draw time
	drawText(frame, timeStr, x0+2, y0-3, faceClock, PCAT_WHITE, false)	

	if networkStr == "w"{
		//draw wired
		eth, _, _, err := loadImage(assetsPrefix+"/assets/svg/eth.svg")
		if err != nil {
			fmt.Println("Error loading eth:", err)
			return false
		}
		copyImageToImageAt(frame, eth, x0+80, y0+2)

	}else if networkStr == "i"{
		// WiFi as WAN (Smart WAN): draw the WiFi icon, same as wired draws eth.svg.
		wifi, _, _, err := loadImage(assetsPrefix+"/assets/svg/wifi.svg")
		if err != nil {
			fmt.Println("Error loading wifi:", err)
			return false
		}
		copyImageToImageAt(frame, wifi, x0+80, y0+2)
	}else if isCellular(networkStr) {
		// signalStrength was already resolved above from ModemSignalStrength.
		//draw signal strength
		// "c" means cellular of unknown generation — draw the bars alone.
		genStr := networkStr
		if genStr == "c" {
			genStr = ""
		}
		if fiveGonTop {
			drawSignalStrength(frame, x0+80, y0, signalStrength)
			drawText(frame, genStr, x0+78, y0-6, faceTiny, PCAT_WHITE, false)
		}else{
			drawSignalStrength(frame, x0+70, y0, signalStrength)
			drawText(frame, genStr, x0+94, y0-3, faceTiny, PCAT_WHITE, false)
		}
	}else if networkStr == "u"{
		nolink, _, _, err := loadImage(assetsPrefix+"/assets/svg/nolink.svg")
		if err != nil {
			fmt.Println("Error loading nolink:", err)
			return false
		}
		copyImageToImageAt(frame, nolink, x0+80, y0+2)
	}

	//draw Battery
	socValue, _ := globalData.Load("BatterySoc") //it is int
	socInt, _ := socValue.(int)
	socFloat := float64(socInt) // now convert int to float64
	charging, _ := globalData.Load("BatteryCharging")
	chargingBool, ok := charging.(bool) // Type assertion to bool
	if !ok {
		chargingBool = false // Default value if assertion fails
	}
	// A pixel taller and a pixel narrower than the glyph used to be, starting a
	// row higher; drawBattery centres the percent text in whatever body those
	// dimensions leave.
	if fiveGonTop {
		img := drawBattery(49, 20, socFloat, chargingBool, x0, y0)
		copyImageToImageAt(frame, img, x0+108, y0-1)
	}else{
		img := drawBattery(44, 19, socFloat, chargingBool, x0, y0)
		copyImageToImageAt(frame, img, x0+113, y0-1)
	}
	cacheTopBar = frame
	cacheTopBarStr = magicStr
	return true
}

func saveFrameToPng(frame *image.RGBA, filename string) {
	outFile, err := os.Create(filename)
	if err != nil {
		panic(err)
	}
	defer outFile.Close()
	png.Encode(outFile, frame)
	fmt.Println("Frame saved to", filename)
}

func renderMiddle(frame *image.RGBA, cfg *Config, isSMS bool, pageIdx int) {
	// Safety check for frame validity
	if frame == nil || frame.Bounds().Empty() {
		log.Printf("renderMiddle: invalid frame bounds %+v", frame)
		return
	}

	if isSMS {
		// Bounds check for SMS pages
		if pageIdx >= 0 && pageIdx < len(smsPagesImages) && smsPagesImages[pageIdx] != nil {
			copyImageToImageAt(frame, smsPagesImages[pageIdx], 0, 0)
		} else {
			log.Printf("renderMiddle: invalid SMS page index %d", pageIdx)
		}
		return
	}

	if isWifiQrPage(pageIdx) {
		drawWifiQrPage(frame, pageIdx-templatePageCount())
		return
	}

	page := cfg.DisplayTemplate.Elements["page"+strconv.Itoa(pageIdx)]

	// Process each element.
	for _, element := range page {
		// Check if the element is enabled.
		if element.Enable == 0 {
			continue
		}

		switch element.Type {
		case "text":
			// Determine the text to display first (moved up to use for font selection)
			textValue, exists := globalData.Load(element.DataKey)
			var textToDisplay string
			var isPingTimeout bool = false
			
			if exists {
				if textValue == nil {
					textToDisplay = ""
				} else {
					// Special handling for ping timeout (-2) and errors
					if (element.DataKey == "Ping0" || element.DataKey == "Ping1") {
						// Handle both int64 and int types for ping values
						var pingVal int64
						var validPingValue bool
						
						if val, ok := textValue.(int64); ok {
							pingVal = val
							validPingValue = true
						} else if val, ok := textValue.(int); ok {
							pingVal = int64(val)
							validPingValue = true
						}
						
						if validPingValue {
							if pingVal == -2 || pingVal == -1 {
								// Both timeout (-2) and other failures (-1) show red X
								textToDisplay = "X"
								isPingTimeout = true
							} else if pingVal >= 0 {
								textToDisplay = fmt.Sprintf("%d", pingVal)
							} else {
								// Any other negative value should not happen, but show as red X
								textToDisplay = "X"
								isPingTimeout = true
							}
						} else {
							textToDisplay = ""
						}
					} else {
						textToDisplay = fmt.Sprintf("%v", textValue)
						// Failed custom-metric fetches store sentinels like
						// "ERROR"/"TIMEOUT"; treat as empty (no raw sentinel).
						if isErrorSentinel(textToDisplay) {
							textToDisplay = ""
						}
					}
				}
			} else {
				textToDisplay = ""
			}
			// A lone "-" (or blank) means "nothing to show" — leave the slot
			// empty (e.g. page0 RemainingTime) rather than drawing a dash.
			textToDisplay = strings.TrimSpace(textToDisplay)
			if textToDisplay == "" || textToDisplay == "-" {
				continue
			}

			// Get the font face for the main text (choose Chinese font if needed)
			face, _, err := getFontFaceForText(element.Font, textToDisplay)
			if err != nil {
				log.Printf("Error getting font face for %s: %v", element.Font, err)
				continue
			}
			// Get the font face for the units.
			unitFace, _, err := getFontFace(element.UnitsFont)
			if err != nil {
				log.Printf("Error getting font face for %s: %v", element.UnitsFont, err)
				continue
			}

			// Convert the color array (assumed to be [R,G,B]) to a color.RGBA.
			var clr color.RGBA
			if len(element.Color) >= 3 {
				clr = color.RGBA{
					R: uint8(element.Color[0]),
					G: uint8(element.Color[1]),
					B: uint8(element.Color[2]),
					A: 255,
				}
			} else {
				// Default color: white.
				clr = color.RGBA{255, 255, 255, 255}
			}

			// Use red color for ping timeouts
			if isPingTimeout {
				clr = PCAT_RED
			}

			// Draw the main text.
			// The drawText function uses the provided y plus the font ascent as the baseline.
			mainAscent := face.Metrics().Ascent.Round()
			// element.Position.Y acts as the top of the text area.
			mainBaseline := element.Position.Y + mainAscent

			// Available horizontal space for this value: from its start x to the
			// right screen edge (minus the standard right margin), unless the
			// element pins an explicit width via "size".width. Values wider than
			// this (long SSIDs, IPv6 addresses, ISP names, …) scroll as a ticker
			// instead of being clipped; values that fit are drawn in place.
			// A centered value grows symmetrically from Position.X, so its
			// budget is the whole panel between the margins, not the distance
			// from its left edge to the right margin.
			centered := element.Align == "center"
			availWidth := PCAT2_LCD_WIDTH - element.Position.X - PCAT2_R_MARGIN
			if centered {
				availWidth = PCAT2_LCD_WIDTH - PCAT2_L_MARGIN - PCAT2_R_MARGIN
			}
			if element.Size != nil && element.Size.Width > 0 {
				availWidth = element.Size.Width
			} else if element.Size2 != nil && element.Size2.Width > 0 {
				availWidth = element.Size2.Width
			}

			mainW := font.MeasureString(face, textToDisplay).Round()
			// A ping timeout is a single "X"; never scroll it. Only overflowing
			// values scroll, and a scrolling value carries no trailing units.
			if !isPingTimeout && mainW > availWidth {
				scrollX := element.Position.X
				if centered {
					scrollX -= availWidth / 2
				}
				drawScrollingText(frame, textToDisplay, scrollX, element.Position.Y, availWidth, face, clr)
				break
			}

			xMain, _ := drawText(frame, textToDisplay, element.Position.X, element.Position.Y, face, clr, centered)

			// Draw the units (skip them for timeout, which renders a bare "X").
			if !isPingTimeout {
				unitText := element.Units
				//check if there is a override unit
				theKey := element.DataKey + "_Unit"
				if _, ok := globalData.Load(theKey); ok {
					unitTextVal, _ := globalData.Load(theKey)
					unitText = unitTextVal.(string)
				}

				// Default: trailing, 1px past the value, sharing its baseline.
				unitAscent := unitFace.Metrics().Ascent.Round()
				unitX := xMain + 1
				unitY := mainBaseline - unitAscent + element.UnitsDy

				if element.UnitsBelow > 0 {
					// Stacked: the unit gets its own line under the value, with
					// UnitsBelow px of clear space between the two. Measured
					// off the glyphs' ink boxes, not their em boxes — a 62px
					// digit's em box runs well past the bottom of the "3", so
					// baseline arithmetic alone would open a gap that looks
					// nothing like the number asked for. Kept in 26.6 fixed
					// point and rounded once at the end: rounding the two ink
					// edges separately costs a pixel each way.
					valueInk, _ := font.BoundString(face, textToDisplay)
					unitInk, _ := font.BoundString(unitFace, unitText)
					// The value's floor is its baseline, even when the glyphs
					// stop short of it: a placeholder "--" is all mid-height
					// ink, and measuring that literally would ride the unit
					// 15px up the panel, then drop it again the moment a fix
					// arrives and the dashes become digits.
					valueBottom := valueInk.Max.Y
					if valueBottom < 0 {
						valueBottom = 0
					}
					unitBaseline := (fixed.I(mainBaseline) + valueBottom +
						fixed.I(element.UnitsBelow) - unitInk.Min.Y).Round()
					unitX = element.Position.X
					unitY = unitBaseline - unitAscent + element.UnitsDy
				}

				drawText(frame, unitText, unitX, unitY, unitFace, clr, centered && element.UnitsBelow > 0)
			}
		
		case "icon":
			// Destination size (defaults applied after load for non-SVG).
			var sz Size
			if element.Size != nil {
				sz = *element.Size
			} else if element.Size2 != nil {
				sz = *element.Size2
			}

			iconX := element.Position.X
			// When anchored to a text data_key, stick the icon to the right of
			// that value's rendered width so it tracks variable-width text
			// (e.g. "0:10" vs "12:30") instead of sitting at a fixed x.
			if element.AnchorAfterDataKey != "" {
				anchorText := ""
				if v, ok := globalData.Load(element.AnchorAfterDataKey); ok && v != nil {
					anchorText = fmt.Sprintf("%v", v)
				}
				// The icon labels the anchored value; when there is no value
				// yet (startup) or nothing to show ("" / "-"), draw neither.
				if anchorText == "" || anchorText == "-" {
					continue
				}
				anchorFontName := element.AnchorFont
				if anchorFontName == "" {
					anchorFontName = "reg"
				}
				if anchorFace, _, ferr := getFontFaceForText(anchorFontName, anchorText); ferr == nil {
					textW := font.MeasureString(anchorFace, anchorText).Round()
					iconX = element.Position.X + textW + element.AnchorGap
				}
			}

			// Optional per-element recolor (set by web color themes): tint the
			// icon while keeping dark detail and antialiasing.
			var tint *color.RGBA
			if len(element.Color) >= 3 {
				tint = &color.RGBA{uint8(element.Color[0]), uint8(element.Color[1]), uint8(element.Color[2]), 255}
			}

			fullPath := assetsPrefix + "/" + element.IconPath
			// SVGs: rasterize at the requested size so size.width/height actually
			// scale (loadImage only renders at intrinsic viewBox size).
			if strings.HasSuffix(strings.ToLower(element.IconPath), ".svg") {
				tw, th := sz.Width, sz.Height
				if err := drawSVGTinted(frame, fullPath, iconX, element.Position.Y, tw, th, tint); err != nil {
					log.Printf("Error drawing SVG icon %s: %v", element.IconPath, err)
				}
				continue
			}

			iconImg, iw, ih, err := loadImage(fullPath)
			if err != nil {
				log.Printf("Error loading icon from %s: %v", element.IconPath, err)
				continue
			}
			if tint != nil {
				tintKey := fmt.Sprintf("%s_t%02x%02x%02x", fullPath, tint.R, tint.G, tint.B)
				if cached, ok := svgCache[tintKey]; ok {
					iconImg = cached
				} else {
					iconImg = tintIconImage(iconImg, *tint)
					svgCache[tintKey] = iconImg
				}
			}
			if sz.Width == 0 {
				sz.Width = iw
			}
			if sz.Height == 0 {
				sz.Height = ih
			}
			pt := image.Pt(iconX, element.Position.Y)
			rect := image.Rect(pt.X, pt.Y, pt.X+sz.Width, pt.Y+sz.Height)
			draw.Draw(frame, rect, iconImg, image.Point{}, draw.Over)
		case "fixed_text":
			face, _, err := getFontFace(element.Font)
			if err != nil {
				log.Printf("Error getting font face for %s: %v", element.Font, err)
				continue
			}
			// pick a default white if no color
			var clr color.RGBA
			if len(element.Color) >= 3 {
				clr = color.RGBA{uint8(element.Color[0]), uint8(element.Color[1]), uint8(element.Color[2]), 255}
			} else {
				clr = color.RGBA{255, 255, 255, 255}
			}
		
			// replace [key] → cfg.Field
			label := placeholderRe.ReplaceAllStringFunc(element.Label, func(tok string) string {
				key := tok[1 : len(tok)-1] // strip brackets
				switch key {
				case "ping_site0":
					return pingSiteDisplay(cfg.PingSite0, cfg.PingType0)
				case "ping_site1":
					return pingSiteDisplay(cfg.PingSite1, cfg.PingType1)
				case "ping_type0":
					return normalizePingType(cfg.PingType0)
				case "ping_type1":
					return normalizePingType(cfg.PingType1)
				/*case "screen_dimmer_time_on_battery_seconds":
					return strconv.Itoa(cfg.ScreenDimmerTimeOnBatterySeconds)
				case "screen_dimmer_time_on_dc_seconds":
					return strconv.Itoa(cfg.ScreenDimmerTimeOnDCSeconds)*/
				// add more fields here if you ever parameterize them in fixed_text
				default:
					return tok
				}
			})
			if strings.Contains(element.Label, "[ping_site") {
				// Saved layouts from before the substitution carried its own
				// prefix still wrap the placeholder in "ping: " — drop the
				// stale copy (see collapsePingLabel).
				label = collapsePingLabel(label)
			}


			drawText(frame, label, element.Position.X, element.Position.Y, face, clr, element.Align == "center")

		case "vtext":
			// Vertical (stacked-letter) fixed label, e.g. "CPU"/"MEM" beside a
			// bar meter. Font, color and position come from the element; the
			// per-letter line height defaults to the font height but can be
			// tuned via size.height, and the column width via size.width.
			face, fh, err := getFontFace(element.Font)
			if err != nil {
				log.Printf("Error getting font face for %s: %v", element.Font, err)
				continue
			}
			var clr color.RGBA
			if len(element.Color) >= 3 {
				clr = color.RGBA{uint8(element.Color[0]), uint8(element.Color[1]), uint8(element.Color[2]), 255}
			} else {
				clr = color.RGBA{255, 255, 255, 255}
			}
			charW := 10
			lineH := fh
			if element.Size != nil {
				if element.Size.Width > 0 {
					charW = element.Size.Width
				}
				if element.Size.Height > 0 {
					lineH = element.Size.Height
				}
			}
			drawVerticalText(frame, element.Label, element.Position.X, element.Position.Y, charW, lineH, face, clr)

		case "graph":
			// Handle graph elements
			if element.GraphConfig == nil {
				log.Printf("Graph element missing graph_config")
				continue
			}
			
			// Determine the size for the graph
			var sz Size
			if element.Size != nil {
				sz = *element.Size
			} else if element.Size2 != nil {
				sz = *element.Size2
			} else {
				// Default graph size
				sz = Size{Width: 60, Height: 25}
			}
			
			// Set time frame if specified
			if element.GraphConfig.TimeFrameMins > 0 {
				setPowerGraphTimeFrame(element.GraphConfig.TimeFrameMins)
			}
			
			// Draw the graph based on type
			switch element.GraphConfig.GraphType {
			case "power":
				drawPowerGraph(frame, element.Position.X, element.Position.Y, sz.Width, sz.Height)
			case "gps_compass":
				// Heading tape: reads the raw course (not the preformatted
				// GpsCourse string) so it can rotate smoothly, and greys out
				// when there is no fix.
				course, hasFix := 0.0, false
				if v, ok := globalData.Load("GpsCourseDeg"); ok && v != nil {
					if f, ok := v.(float64); ok {
						course, hasFix = f, true
					}
				}
				drawGpsCompass(frame, element.Position.X, element.Position.Y,
					sz.Width, sz.Height, course, hasFix, elementFillColor(element))
			default:
				log.Printf("Unknown graph type: %s", element.GraphConfig.GraphType)
			}

		case "cpu_bars":
			// Framed 8-bar (per-core) CPU meter. Size comes from the element;
			// the per-core usages come from the CpuUsages data key.
			sz := Size{Width: 80, Height: 40}
			if element.Size != nil {
				sz = *element.Size
			} else if element.Size2 != nil {
				sz = *element.Size2
			}
			var usages []float64
			if v, ok := globalData.Load(element.DataKey); ok && v != nil {
				if u, ok := v.([]float64); ok {
					usages = u
				}
			}
			// Nothing sampled yet (startup): draw an empty framed box so the
			// slot isn't blank, using a default 8 cores at 0%.
			if len(usages) == 0 {
				usages = make([]float64, 8)
			}
			drawCpuBars(frame, element.Position.X, element.Position.Y, sz.Width, sz.Height, usages, elementFillColor(element))

		case "hbar":
			// Framed horizontal progress bar driven by a 0-100 data key
			// (e.g. MemUsagePercent). Optional LabelDataKey (e.g. MemUsage
			// "3.2/16") plus Units ("GB") is drawn centered over the bar.
			sz := Size{Width: 100, Height: 12}
			if element.Size != nil {
				sz = *element.Size
			} else if element.Size2 != nil {
				sz = *element.Size2
			}
			pct := 0.0
			if v, ok := globalData.Load(element.DataKey); ok && v != nil {
				switch n := v.(type) {
				case float64:
					pct = n
				case int:
					pct = float64(n)
				}
			}
			label := ""
			if element.LabelDataKey != "" {
				if v, ok := globalData.Load(element.LabelDataKey); ok && v != nil {
					switch s := v.(type) {
					case string:
						label = s
					default:
						label = fmt.Sprintf("%v", s)
					}
				}
			}
			if label != "" && element.Units != "" {
				label = label + element.Units
			}
			drawHBar(frame, element.Position.X, element.Position.Y, sz.Width, sz.Height, pct, label, elementFillColor(element))

		case "disk_bars":
			// Onboard + optional NVMe usage bars, same height as mem, no text.
			// Full-width single bar when NVMe is absent; left/right split when
			// present. Size comes from the element (defaults match mem bar).
			sz := Size{Width: 134, Height: 26}
			if element.Size != nil {
				sz = *element.Size
			} else if element.Size2 != nil {
				sz = *element.Size2
			}
			drawDiskBars(frame, element.Position.X, element.Position.Y, sz.Width, sz.Height, elementFillColor(element))

		default:
			log.Printf("Unknown element type: %s", element.Type)
		}
	}
}

func drawFooter(display gc9307.Device, frame *image.RGBA, currPage int, numOfPages int, isSMS bool) {
	if renderFooter(frame, currPage, numOfPages, isSMS) {
		sendFooter(display, frame)
	}
}

// renderFooter draws the footer into frame without touching the display.
// Returns false on a cache hit or load error.
func renderFooter(frame *image.RGBA, currPage int, numOfPages int, isSMS bool) bool {
	// configVersion is part of the key so a theme change repaints the footer
	// immediately. Without it the key only moves when the page index or count
	// changes, so a recolor left the old footer (background included) on screen
	// until the user pressed the button to page forward.
	// Wi-Fi QR pages are not counted in the page dots. On one of them the
	// footer shows its radio line ("5.745GHz • 80MHz • ch149") instead, and
	// falls back to the dots only when no live radio data matched.
	radioLine := ""
	if !isSMS {
		radioLine = wifiQrFooterLine(currPage)
		numOfPages -= wifiQrPageCount()
		if numOfPages < 1 {
			numOfPages = 1
		}
	}
	magicStr := strconv.Itoa(currPage) + " " + strconv.Itoa(numOfPages) + " " +
		strconv.FormatBool(isSMS) + " " + strconv.Itoa(configVersion) + " " + radioLine
	if cacheFooterStr == magicStr {
		return false //no need to refresh
	}
	faceMicro, _, err := getFontFace("micro")
	if err != nil {
		log.Printf("Error getting font face for %s: %v", "tiny", err)
		return false
	}

	footerFrameWidth := PCAT2_LCD_WIDTH
	footerFrameHeight := PCAT2_FOOTER_HEIGHT
	clearFrame(frame, footerFrameWidth, footerFrameHeight)

	if isSMS {
		footerText := "SMS: " + strconv.Itoa(currPage+1) + "/" + strconv.Itoa(numOfPages)
		drawText(frame, footerText, 172/2, 2, faceMicro, PCAT_WHITE, true)

	} else if radioLine != "" {
		// Same micro font and row as the SMS footer, in light grey,
		// centred by ink like the info line above it.
		drawTextCenteredInk(frame, radioLine, PCAT2_LCD_WIDTH/2, 0, 2, faceMicro, color.RGBA{200, 200, 200, 255})
	}else{
		cir, _, _, err := loadImage(assetsPrefix+"/assets/svg/dotCircle.svg")
		if err != nil {
			log.Printf("Error loading circle_dot from %s: %v", "assets/svg/dotCircle.svg", err)
			return false
		}
		dot, _, _, err := loadImage(assetsPrefix+"/assets/svg/dotSolid.svg")
		if err != nil {
			log.Printf("Error loading dot from %s: %v", "assets/svg/dotSolid.svg", err)
			return false
		}

		whiteDotRadius := 8
		greyDotRadius := 4
		xPart := 10 + whiteDotRadius * 2
		yOffset := 2
		x0 := (PCAT2_LCD_WIDTH - (numOfPages-1)*xPart) / 2  - whiteDotRadius

		for i := 0; i < numOfPages; i++ {
			if i == currPage {
				copyImageToImageAt(frame, cir, x0+i*xPart, yOffset)
			}else{
				copyImageToImageAt(frame, dot, x0+i*xPart + greyDotRadius, yOffset + greyDotRadius)
			}
		}
	}
	//make a frame cache
	cacheFooter = frame
	cacheFooterStr = magicStr
	return true
}

// drawRoundedBar fills a w x h rounded rectangle of the given color into dst
// starting at (0,0). The corner radius is clamped to min(r, w/2, h/2), which
// matches how SVG clamps rx/ry on narrow rects.
func drawRoundedBar(dst *image.RGBA, w, h, r int, clr color.RGBA) {
	if r > w/2 {
		r = w / 2
	}
	if r > h/2 {
		r = h / 2
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// Inside a corner box, keep the pixel only if it is within the
			// corner circle; everywhere else the rect is solid.
			cx, cy := x, y
			if x < r {
				cx = r
			} else if x >= w-r {
				cx = w - r - 1
			}
			if y < r {
				cy = r
			} else if y >= h-r {
				cy = h - r - 1
			}
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy > r*r {
				continue
			}
			dst.SetRGBA(x, y, clr)
		}
	}
}

// showWelcome plays the "cat waking up" boot animation (welcomeAnim.go) for
// the given duration. Rendering is pipelined: a goroutine rasterizes the next
// frame into one of two buffers while the SPI push of the previous frame is
// still in flight, so the effective rate is max(render, push) instead of
// their sum. Everything is derived from wall-clock time, so a slow frame
// never stretches the animation past its duration.
func showWelcome(display gc9307.Device, width, height int, duration time.Duration) {
	dur := duration.Seconds()
	if dur <= 0 {
		dur = welcomeAnimDur
	}
	timeScale := welcomeAnimDur / dur

	// Prove the animation pipeline works before committing the boot screen
	// to it; fall back to the static welcome otherwise.
	first, err := welcomeAnimFrameInto(nil, 0, width, height)
	if err != nil {
		log.Printf("Welcome animation unavailable (%v); showing static welcome", err)
		showWelcomeStatic(display, width, height)
		return
	}
	sendFull(display, first)

	const frameInterval = time.Second / 60 // pace renders; LCD tops out around 60 Hz anyway

	frames := make(chan *image.RGBA) // unbuffered: hand-off keeps the two buffers race-free
	go func() {
		defer close(frames)
		var bufs [2]*image.RGBA
		start := time.Now()
		for i := 0; ; i++ {
			frameStart := time.Now()
			t := frameStart.Sub(start).Seconds() * timeScale
			if t >= welcomeAnimDur {
				return
			}
			f, err := welcomeAnimFrameInto(bufs[i%2], t, width, height)
			if err != nil {
				log.Printf("Welcome animation frame failed: %v", err)
				return
			}
			bufs[i%2] = f
			frames <- f
			if d := frameInterval - time.Since(frameStart); d > 0 {
				time.Sleep(d)
			}
		}
	}()

	sent := 0
	animStart := time.Now()
	for f := range frames {
		sendFull(display, f)
		sent++
	}
	if el := time.Since(animStart); sent > 0 && el > 0 {
		log.Printf("Welcome animation: %d frames in %.2fs (%.1f fps)", sent, el.Seconds(), float64(sent)/el.Seconds())
	}

	// Land exactly on the resting pose, hold a beat, then hand over to the
	// first page with a crossfade instead of a one-frame snap.
	if f, err := welcomeAnimFrameInto(nil, welcomeAnimDur, width, height); err == nil {
		sendFull(display, f)
		time.Sleep(200 * time.Millisecond)
		transitionWelcomeToPage(display, f)
	}
}

// transitionWelcomeToPage dissolves the welcome animation's resting pose into
// the first page: the cat grows a little while fading out, and page 0 zooms
// in from slightly small to full size underneath it. The main render loop is
// still parked on wg.Wait() while this runs, so composing page sections here
// races nothing; the loop's first real frame repeats the same content, which
// makes the hand-off seamless.
func transitionWelcomeToPage(display gc9307.Device, catFrame *image.RGBA) {
	const dur = 700 * time.Millisecond
	W, H := PCAT2_LCD_WIDTH, PCAT2_LCD_HEIGHT

	// Compose the full first-page frame with the same section renderers the
	// loop uses. Caches are cleared before (so the sections really render
	// into our buffers) and after (so the loop's first pass draws its own).
	target := image.NewRGBA(image.Rect(0, 0, W, H))
	cacheTopBarStr = ""
	top := image.NewRGBA(image.Rect(0, 0, W, PCAT2_TOP_BAR_HEIGHT))
	if renderTopBar(top) {
		copyImageToImageAt(target, top, 0, 0)
	}
	mid := image.NewRGBA(image.Rect(0, 0, middleFrameWidth, middleFrameHeight))
	clearFrame(mid, middleFrameWidth, middleFrameHeight)
	renderMiddle(mid, &cfg, false, 0)
	copyImageToImageAt(target, mid, 0, PCAT2_TOP_BAR_HEIGHT)
	cacheFooterStr = ""
	foot := image.NewRGBA(image.Rect(0, 0, W, PCAT2_FOOTER_HEIGHT))
	if renderFooter(foot, 0, cfgNumPages, false) {
		copyImageToImageAt(target, foot, 0, H-PCAT2_FOOTER_HEIGHT)
	}
	cacheTopBarStr = ""
	cacheFooterStr = ""

	frame := image.NewRGBA(image.Rect(0, 0, W, H))
	pageLayer := image.NewRGBA(image.Rect(0, 0, W, H))
	catLayer := image.NewRGBA(image.Rect(0, 0, W, H))

	start := time.Now()
	for {
		t := float64(time.Since(start)) / float64(dur)
		if t >= 1 {
			break
		}
		e := t * t * (3 - 2*t) // smoothstep

		// Page 0: zoom 74% -> 100% while fading in.
		s := 0.74 + 0.26*e
		w := int(float64(W) * s)
		h := int(float64(H) * s)
		pr := image.Rect((W-w)/2, (H-h)/2, (W-w)/2+w, (H-h)/2+h)
		clearFrame(pageLayer, W, H)
		xdraw.ApproxBiLinear.Scale(pageLayer, pr, target, target.Bounds(), xdraw.Src, nil)

		// Cat: grow to 130% while dissolving.
		cs := 1 + 0.30*e
		cw := int(float64(W) * cs)
		ch := int(float64(H) * cs)
		cr := image.Rect((W-cw)/2, (H-ch)/2, (W-cw)/2+cw, (H-ch)/2+ch)
		for i := range catLayer.Pix {
			catLayer.Pix[i] = 0
		}
		xdraw.ApproxBiLinear.Scale(catLayer, cr, catFrame, catFrame.Bounds(), xdraw.Src, nil)

		clearFrame(frame, W, H)
		draw.DrawMask(frame, frame.Bounds(), pageLayer, image.Point{},
			image.NewUniform(color.Alpha{A: uint8(255 * e)}), image.Point{}, draw.Over)
		draw.DrawMask(frame, frame.Bounds(), catLayer, image.Point{},
			image.NewUniform(color.Alpha{A: uint8(255 * (1 - e))}), image.Point{}, draw.Over)
		sendFull(display, frame)
	}
	sendFull(display, target)
}

// showWelcomeStatic is the pre-animation welcome screen: static logo plus a
// progress bar drawn directly in Go. Kept as the fallback path should the
// animation ever fail to render, so it deliberately avoids the SVG rasterizer
// and every error path that could abort it once the logo is already on screen.
func showWelcomeStatic(display gc9307.Device, width, height int) {
	radiusBarCorner := 5
	spaceBetweenLogoAndBar := 28
	barWidth := 82
	barX := width/2 - barWidth/2
	barHeight := 8
	barTrackColor := color.RGBA{0x62, 0x74, 0x82, 255} // #627482
	barFillColor := color.RGBA{0xFD, 0xE0, 0x21, 255}  // #FDE021
	sleepPerPixel := 15 * time.Millisecond             // ~1.2s total sweep

	frame := image.NewRGBA(image.Rect(0, 0, width, height))
	clearFrame(frame, width, height)
	welcomeLogo, w, h, err := loadImage(assetsPrefix+"/assets/svg/welcome.svg")
	if err != nil {
		log.Printf("Error loading welcome logo from %s: %v", "assets/svg/welcome.svg", err)
		return
	}
	logoY := height/2 - (h+spaceBetweenLogoAndBar+barHeight)/2
	x0 := width/2 - w/2
	y0 := logoY
	log.Printf("Welcome logo at: x0: %d, y0: %d, w: %d, h: %d", x0, y0, w, h)
	copyImageToImageAt(frame, welcomeLogo, x0, y0)

	// The bar is rasterized directly rather than through an SVG round-trip, so
	// there is no error path left that can abort the sweep after the logo has
	// already been pushed to the display.
	bar := image.NewRGBA(image.Rect(0, 0, barWidth, barHeight))
	drawRect(bar, 0, 0, barWidth, barHeight, color.RGBA{0, 0, 0, 255}) // opaque black corners
	drawRoundedBar(bar, barWidth, barHeight, radiusBarCorner, barTrackColor)
	barY := logoY + spaceBetweenLogoAndBar + h
	copyImageToImageAt(frame, bar, barX, barY)
	sendFull(display, frame)

	// Animate the yellow fill by pushing only the bar strip (barWidth x
	// barHeight pixels) per step instead of a full frame, so the sweep is paced
	// by sleepPerPixel rather than by the SPI clock.
	sweepStart := time.Now()
	for i := 1; i <= barWidth; i++ {
		drawRoundedBar(bar, i, barHeight, radiusBarCorner, barFillColor)
		copyImageToImageAt(frame, bar, barX, barY)
		display.FillRectangleWithImage(int16(barX), int16(barY), int16(barWidth), int16(barHeight), bar)
		time.Sleep(sleepPerPixel)
	}
	log.Printf("Welcome static bar completed in %.0fms", durationToMs(time.Since(sweepStart)))
}

func showCiao(display gc9307.Device, width, height int, duration time.Duration) {
	spaceBetweenLogoAndText := 28
	textHeight := 12
	frame := image.NewRGBA(image.Rect(0, 0, width, height))
	clearFrame(frame, width, height)
	//clear display
	sendFull(display, frame)
	ciaoLogo, w, h, err := loadImage(assetsPrefix+"/assets/svg/ciao.svg")
	if err != nil {
		log.Printf("Error loading ciao logo from %s: %v", "assets/svg/ciao.svg", err)
		return
	}
	logoY := height/2 - (h+spaceBetweenLogoAndText+textHeight)/2
	copyImageToImageAt(frame, ciaoLogo, width/2 - w/2, logoY ) 
	text := "Powering Off..."
	faceUnit, _, err := getFontFace("unit")
	if err != nil {
		log.Printf("Error getting font face for %s: %v", "unit", err)
		return
	}
	drawText(frame, text, width/2, logoY + h + spaceBetweenLogoAndText, faceUnit, PCAT_WHITE, true)
	sendFull(display, frame)

}

func showCiaoInstant(display gc9307.Device, width, height int) {
	spaceBetweenLogoAndText := 28
	textHeight := 12
	frame := image.NewRGBA(image.Rect(0, 0, width, height))
	clearFrame(frame, width, height)
	
	// Load and display shutdown screen
	ciaoLogo, w, h, err := loadImage(assetsPrefix+"/assets/svg/ciao.svg")
	if err != nil {
		log.Printf("Error loading ciao logo from %s: %v", "assets/svg/ciao.svg", err)
		return
	}
	logoY := height/2 - (h+spaceBetweenLogoAndText+textHeight)/2
	copyImageToImageAt(frame, ciaoLogo, width/2 - w/2, logoY)
	text := "Powering Off..."
	faceUnit, _, err := getFontFace("unit")
	if err != nil {
		log.Printf("Error getting font face for %s: %v", "unit", err)
		return
	}
	drawText(frame, text, width/2, logoY + h + spaceBetweenLogoAndText, faceUnit, PCAT_WHITE, true)
	sendFull(display, frame)
	
	// Instantly dim to minimum brightness
	setBacklight(0)
}

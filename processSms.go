package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	//"os/exec"
	"io"
	"log"
	"net/http"

	"github.com/golang/freetype"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

type SMS struct {
	Index     int    `json:"index"`
	Sender    string `json:"sender"`
	Timestamp string `json:"timestamp"`
	Reference int    `json:"reference,omitempty"`
	Part      int    `json:"part,omitempty"`
	Total     int    `json:"total,omitempty"`
	Content   string `json:"content"`
	Status    string `json:"status,omitempty"`
	Direction int    `json:"direction,omitempty"`
	To        string `json:"to,omitempty"`
}

var (
	lastSmsJsonContent string
	lastNumPages       int
	// lastSmsConfigVersion records the configVersion the cached SMS images
	// were rendered at. SMS pages are rasterised once and then blitted, with
	// the theme palette from smsThemeColors() baked into the pixels, so a
	// recolor with unchanged message text has to invalidate them explicitly —
	// otherwise the SMS pages keep the old palette until a new message
	// arrives. -1 means "nothing rendered yet".
	lastSmsConfigVersion         = -1
	lastSuccessfulSmsJsonContent string

	// smsRerender wakes getSmsPages out of its 1-minute wait so a theme change
	// repaints the SMS pages now instead of at the next poll. Buffered depth 1
	// with a non-blocking send: coalescing several rapid theme saves into one
	// re-render is exactly the desired behaviour.
	smsRerender = make(chan struct{}, 1)
	
	// Memory pool for SMS images to reduce allocations
	smsImagePool = sync.Pool{
		New: func() interface{} {
			return image.NewRGBA(image.Rect(0, 0, 172, 270))
		},
	}

	smsFontOnce sync.Once
	smsFont     *truetype.Font
)

const (
	// titleTimeLayout is how a message's timestamp is packed into the title line
	// ("sender|2006-01-02 15:04") before it is rendered.
	titleTimeLayout = "2006-01-02 15:04"
	// senderGap is the blank space kept between the sender and the right-aligned
	// timestamp, in pixels.
	senderGap = 6
)

// smsTimeLabel dates a message: the clock alone for today, an age in days
// ("1d ago" ... "6d ago") through the past week, and a numeric date beyond
// that - month-day within this year, full ISO date for an older one. The clock
// time stays on every label because this row is the only place a message's time
// is shown.
func smsTimeLabel(msgAt, now time.Time) string {
	clock := msgAt.Format("15:04")
	// Both days are rebuilt in UTC purely to count calendar days between them;
	// subtracting local midnights would be 23h or 25h across a DST change and
	// could date yesterday's message as today's.
	day := time.Date(msgAt.Year(), msgAt.Month(), msgAt.Day(), 0, 0, 0, 0, time.UTC)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	daysAgo := int(today.Sub(day).Hours() / 24)

	switch {
	case daysAgo <= 0:
		// Today - and anything stamped ahead of us, which a modem clock that is
		// briefly off can produce.
		return clock
	case daysAgo < 7:
		return fmt.Sprintf("%dd ago %s", daysAgo, clock)
	case msgAt.Year() == now.Year():
		return msgAt.Format("01-02") + " " + clock
	default:
		return msgAt.Format("2006-01-02") + " " + clock
	}
}

// fitSenderWidth shortens a sender to fit maxWidth pixels, keeping its head and
// its last two characters ("+861380**00") so a phone number stays recognisable
// at both ends. Trimming is per rune, so a CJK sender is never cut mid
// character.
func fitSenderWidth(sender string, drawer *font.Drawer, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	textWidth := func(s string) int { return int(drawer.MeasureString(s) >> 6) }
	if textWidth(sender) <= maxWidth {
		return sender
	}
	runes := []rune(sender)
	if len(runes) <= 4 {
		// Too short to elide usefully; drop trailing runes until it fits.
		for len(runes) > 0 && textWidth(string(runes)) > maxWidth {
			runes = runes[:len(runes)-1]
		}
		return string(runes)
	}
	tail := string(runes[len(runes)-2:])
	head := runes[:len(runes)-2]
	for len(head) > 0 {
		head = head[:len(head)-1]
		candidate := string(head) + "**" + tail
		if textWidth(candidate) <= maxWidth {
			return candidate
		}
	}
	return ""
}

// getSmsFont parses the SMS font once and reuses it for every SMS redraw.
func getSmsFont() *truetype.Font {
	smsFontOnce.Do(func() {
		fontPath := assetsPrefix + "/assets/fonts/NotoSansMonoCJK-VF.ttf.ttc"
		log.Println("sms using font:", fontPath)
		fontBytes, err := os.ReadFile(fontPath)
		if err != nil {
			log.Printf("Error loading SMS font: %v", err)
			return
		}
		fnt, err := truetype.Parse(fontBytes)
		if err != nil {
			log.Printf("Error parsing SMS font: %v", err)
			return
		}
		smsFont = fnt
	})
	return smsFont
}

// signalSmsRerender asks getSmsPages to rebuild the cached SMS page images on
// its next loop iteration rather than waiting out the poll interval. Safe to
// call from any goroutine, and a no-op if a re-render is already pending.
func signalSmsRerender() {
	select {
	case smsRerender <- struct{}{}:
	default:
	}
}

func collectAndDrawSms(cfg *Config) int {
	jsonContent := getJsonContent(cfg)

	if len(jsonContent) < 50 { //dummy message
		jsonContent = fmt.Sprintf("{\"msg\":[{\"sender\":\"System\",\"timestamp\":\"%s\",\"content\":\"No SMS - 无消息\"}]}", time.Now().Format("2006-01-02 15:04:05"))
	}

	// Re-render when the messages change OR when the config was remerged (a
	// theme change): the palette is baked into the cached images.
	if jsonContent == lastSmsJsonContent && lastSmsConfigVersion == configVersion {
		log.Println("collectAndDrawSms: No new SMS, lastNumPages:", lastNumPages)
		return lastNumPages
	}

	lastSmsJsonContent = jsonContent
	lastSmsConfigVersion = configVersion
	lastNumPages = 0

	rawImgs, err := drawSmsFrJson(jsonContent, false, false)
	if err != nil {
		log.Println("Error drawing SMS:", err)
		return 0
	}

	// Return old SMS images to pool to prevent memory leaks
	for _, oldImg := range smsPagesImages {
		if oldImg != nil {
			// Clear the image before returning to pool
			draw.Draw(oldImg, oldImg.Bounds(), &image.Uniform{color.Black}, image.Point{}, draw.Src)
			smsImagePool.Put(oldImg)
		}
	}

	// prepare the global slice
	smsPagesImages = make([]*image.RGBA, len(rawImgs))
	for i, img := range rawImgs {
		// try a direct cast
		rgba, ok := img.(*image.RGBA)
		if !ok {
			log.Printf("Image %d is not *image.RGBA, converting…", i)
			// convert by drawing into a new RGBA
			b := img.Bounds()
			r := image.NewRGBA(b)
			draw.Draw(r, b, img, b.Min, draw.Src)
			rgba = r
		}
		smsPagesImages[i] = rgba
	}
	numPages := len(smsPagesImages)
	lastNumPages = numPages

	return numPages
}

func getJsonContent(cfg *Config) string {
	// Use config value or default to 10 if not specified
	smsLimit := 10
	if cfg != nil && cfg.SmsLimitForScreen > 0 {
		smsLimit = cfg.SmsLimitForScreen
	}

	// 1. Make the request
	url := fmt.Sprintf("http://localhost/api/v2/sms/list.json?n=%d", smsLimit)
	resp, err := localHTTPClient.Get(url)
	if err != nil {
		log.Printf("GET /sms/list.json failed: %v", err)
		// pcat-manager-web is down: try reading the messages straight from
		// ModemManager before falling back to cached data.
		if fallback := getSmsJsonFromModemManager(smsLimit); fallback != "" {
			lastSuccessfulSmsJsonContent = fallback
			return fallback
		}
		if lastSuccessfulSmsJsonContent != "" {
			log.Printf("Using cached SMS data due to request failure")
			return lastSuccessfulSmsJsonContent
		}
		return ""
	}
	defer resp.Body.Close()

	// 2. Check HTTP status
	if resp.StatusCode != http.StatusOK {
		log.Printf("unexpected HTTP status: %s", resp.Status)
		if fallback := getSmsJsonFromModemManager(smsLimit); fallback != "" {
			lastSuccessfulSmsJsonContent = fallback
			return fallback
		}
		// Return last successful result if we have one, otherwise return empty
		if lastSuccessfulSmsJsonContent != "" {
			log.Printf("Using cached SMS data due to HTTP error")
			return lastSuccessfulSmsJsonContent
		}
		return ""
	}

	// 3. Read the body
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("reading response body failed: %v", err)
		// Return last successful result if we have one, otherwise return empty
		if lastSuccessfulSmsJsonContent != "" {
			log.Printf("Using cached SMS data due to read error")
			return lastSuccessfulSmsJsonContent
		}
		return ""
	}

	result := string(data)
	// Cache successful result
	if len(result) > 10 { // Basic validation that we got actual data
		lastSuccessfulSmsJsonContent = result
	}
	
	return result
}

// smsThemeColors resolves the SMS page's palette from the active layout.
//
// The SMS pages are drawn here in Go rather than from display_template
// elements, so the web UI's color themes -- which work by rewriting each
// element's "color" -- never reached them: a themed device showed every cfg
// page in its new palette and the SMS pages still in hardcoded yellow/white.
// There is no SMS entry in the template to read, so the palette is taken from
// the themed pages themselves, using the same role split the web UI applies
// (see themeRole in screen.html): big value fonts carry "primary", small info
// text carries "data". Received text takes primary, the sender line data.
//
// Falls back to the classic yellow/white when nothing is themed, so an
// unthemed device looks exactly as before.
func smsThemeColors() (recv color.RGBA, title color.RGBA) {
	recv, title = PCAT_YELLOW, PCAT_WHITE

	configMutex.RLock()
	defer configMutex.RUnlock()

	// Scan pages in order and take the first element of each role that
	// carries an explicit colour. Any themed page yields the palette; the
	// GPS page is included, so it works whichever pages exist.
	bigFonts := map[string]bool{"huge": true, "gigantic": true, "big": true, "reg": true}
	for i := 0; i < templatePageCount(); i++ {
		els, ok := cfg.DisplayTemplate.Elements["page"+strconv.Itoa(i)]
		if !ok {
			continue
		}
		for _, el := range els {
			if len(el.Color) < 3 || el.Type == "icon" {
				continue
			}
			c := color.RGBA{uint8(el.Color[0]), uint8(el.Color[1]), uint8(el.Color[2]), 255}
			if bigFonts[el.Font] {
				return c, title
			}
		}
	}
	return recv, title
}

func drawSmsFrJson(jsonContent string, savePng bool, drawPageNum bool) (imgs []image.Image, err error) {

	var smsData struct {
		Msg []SMS `json:"msg"`
	}
	if err := secureUnmarshal([]byte(jsonContent), &smsData); err != nil {
		log.Printf("Error parsing JSON: %v\n", err)
		return nil, err
	}

	// Load font (parsed once per process; the CJK collection is ~37MB)
	fnt := getSmsFont()
	if fnt == nil {
		return nil, fmt.Errorf("SMS font unavailable")
	}

	// Setup constants
	width, height := 172, 270
	fontSize := 12.0
	fontSizeTitle := 11.0
	lineSpacing := 1.2
	maxWidth := width - 8   // Adjusted for padding
	maxHeight := height - 8 // Adjusted for padding
	topPadding := 3.0
	xStart := 4
	layout := "2006-01-02 15:04:05"

	// Palette for this render, from the active theme (classic yellow/white
	// when the device is unthemed). Resolved once per call rather than per
	// line: the layout cannot change mid-frame and it takes the config lock.
	smsRecvColor, smsTitleColor := smsThemeColors()
	// Sent messages keep their own colour: the sent/received distinction is
	// carried by colour alone here, so tinting both to the theme would erase
	// it. Unchanged from before (light green).
	smsSentColor := color.RGBA{144, 238, 144, 255}

	// Create font context
	fc := freetype.NewContext()
	fc.SetDPI(72)
	fc.SetFont(fnt)
	fc.SetFontSize(fontSize)
	// Color will be set dynamically during rendering based on sender
	fc.SetHinting(font.HintingFull)

	fcTitle := freetype.NewContext()
	fcTitle.SetDPI(72)
	fcTitle.SetFont(fnt)
	fcTitle.SetFontSize(fontSizeTitle)
	// Color will be set dynamically during rendering based on sender
	fcTitle.SetHinting(font.HintingFull)

	// Sort messages by timestamp descending
	sort.Slice(smsData.Msg, func(i, j int) bool {
		ti, err1 := time.Parse(layout, smsData.Msg[i].Timestamp)
		tj, err2 := time.Parse(layout, smsData.Msg[j].Timestamp)
		if err1 != nil || err2 != nil {
			return smsData.Msg[i].Timestamp > smsData.Msg[j].Timestamp
		}
		return ti.After(tj)
	})

	// Prepare pagination
	type Line struct {
		Text        string
		IsTitle     bool
		IsSentByMe  bool
		MessageSender string
	}

	type Page struct {
		Lines []Line
	}

	var pages []Page
	currentPage := Page{}
	currentHeight := topPadding

	faceMeasure := truetype.NewFace(fnt, &truetype.Options{
		Size:    fontSize,
		DPI:     72,
		Hinting: font.HintingFull,
	})

	for _, msg := range smsData.Msg {
		timestamp, _ := time.Parse(layout, msg.Timestamp)
		formattedTime := timestamp.Format(titleTimeLayout)
		
		// Change display: show "> number" instead of "me" for sent messages
		var displaySender string
		if msg.Sender == "me" && msg.To != "" {
			displaySender = "> " + msg.To
		} else {
			displaySender = msg.Sender
		}
		
		title := fmt.Sprintf("%s|%s", displaySender, formattedTime)
		message := msg.Content
		
		// Check if this is a sent message from "me" with status "SENT"
		isSentByMe := msg.Sender == "me" && msg.Status == "SENT"

		lines := wrapText(message, maxWidth, faceMeasure)

		// Add title
		titleHeight := fontSizeTitle * lineSpacing
		if currentHeight+titleHeight > float64(maxHeight) {
			// Save current page
			pages = append(pages, currentPage)
			// Reset for new page
			currentPage = Page{}
			currentHeight = topPadding
		}
		currentPage.Lines = append(currentPage.Lines, Line{Text: title, IsTitle: true, IsSentByMe: isSentByMe, MessageSender: msg.Sender})
		currentHeight += titleHeight

		// Add message lines
		for _, line := range lines {
			lineHeight := fontSize * lineSpacing
			if currentHeight+lineHeight > float64(maxHeight) {
				// Save current page
				pages = append(pages, currentPage)
				// Reset for new page
				currentPage = Page{}
				currentHeight = topPadding
				// Repeat title on new page
				currentPage.Lines = append(currentPage.Lines, Line{Text: title, IsTitle: true, IsSentByMe: isSentByMe, MessageSender: msg.Sender})
				currentHeight += titleHeight
			}
			currentPage.Lines = append(currentPage.Lines, Line{Text: line, IsTitle: false, IsSentByMe: isSentByMe, MessageSender: msg.Sender})
			currentHeight += lineHeight
		}
		// Add spacing after message
		spacingHeight := fontSize * lineSpacing
		if currentHeight+spacingHeight <= float64(maxHeight) {
			currentPage.Lines = append(currentPage.Lines, Line{Text: "", IsTitle: false, IsSentByMe: false, MessageSender: ""})
			currentHeight += spacingHeight
		}
	}

	// Add the last page if it has content
	if len(currentPage.Lines) > 0 {
		pages = append(pages, currentPage)
	}

	// Render pages to PNG
	for i, page := range pages {
		fmt.Printf("Rendering page %d\n", i)
		// Use pooled image buffer
		img := smsImagePool.Get().(*image.RGBA)
		// Clear the image before use
		draw.Draw(img, img.Bounds(), &image.Uniform{screenBgColor}, image.Point{}, draw.Src)

		fc.SetDst(img)
		fc.SetClip(img.Bounds())
		fcTitle.SetDst(img)
		fcTitle.SetClip(img.Bounds())

		y := topPadding
		for _, line := range page.Lines {
			if line.Text == "" {
				y += fontSize * lineSpacing
				continue
			}
			if line.IsTitle {
				lineTitle := strings.Split(line.Text, "|")
				sender := lineTitle[0]
				timeDisplay := lineTitle[1]
				if t, err := time.Parse(titleTimeLayout, lineTitle[1]); err == nil {
					timeDisplay = smsTimeLabel(t, time.Now())
				}

				// Set color for sender based on whether message is sent by me
				if line.IsSentByMe {
					// Light green color for sent messages
					fcTitle.SetSrc(image.NewUniform(smsSentColor))
				} else {
					// Received: the theme's info-text colour (white unthemed)
					fcTitle.SetSrc(image.NewUniform(smsTitleColor))
				}

				// Place the timestamp first: it is right-aligned and its width
				// varies with the label ("14:32" vs "Jan 2, 2025 14:32"), so the
				// sender is fitted to whatever room is left rather than to a
				// fixed character count.
				faceTitle := truetype.NewFace(fnt, &truetype.Options{
					Size:    fontSizeTitle,
					DPI:     72,
					Hinting: font.HintingFull,
				})
				drawer := &font.Drawer{Face: faceTitle}
				adv := drawer.MeasureString(timeDisplay)
				timeWidth := int(adv >> 6)
				rightMargin := width - xStart - 5
				timeX := rightMargin - timeWidth

				// Draw sender left-aligned, shortened so it never runs into the
				// timestamp (senderGap keeps them visibly apart).
				sender = fitSenderWidth(sender, drawer, timeX-xStart-senderGap)
				ptSender := freetype.Pt(xStart, int(y+fontSizeTitle))
				_, err := fcTitle.DrawString(sender, ptSender)
				if err != nil {
					fmt.Printf("Error drawing sender: %v\n", err)
					return nil, err
				}
				ptTime := freetype.Pt(timeX, int(y+fontSizeTitle))
				_, err = fcTitle.DrawString(timeDisplay, ptTime)
				if err != nil {
					fmt.Printf("Error drawing time: %v\n", err)
					return nil, err
				}
				y += fontSizeTitle * lineSpacing
			} else {
				// Set color based on whether message is sent by me
				if line.IsSentByMe {
					// Light green color for sent messages
					fc.SetSrc(image.NewUniform(smsSentColor))
				} else {
					// Received: the theme's primary colour (yellow unthemed)
					fc.SetSrc(image.NewUniform(smsRecvColor))
				}
				
				pt := freetype.Pt(xStart, int(y+fontSize))
				_, err := fc.DrawString(line.Text, pt)
				if err != nil {
					fmt.Printf("Error drawing string: %v\n", err)
					return nil, err
				}
				y += fontSize * lineSpacing
			}
		}
		imgs = append(imgs, img)

	}
	margin := 4 // margin from the right and bottom
	pageNumFontSize := 10.0
	total := len(imgs)
	for i, im := range imgs {
		if drawPageNum {
			pageStr := fmt.Sprintf("%d/%d", i+1, total)
			facePN := truetype.NewFace(fnt, &truetype.Options{Size: pageNumFontSize, DPI: 72, Hinting: font.HintingFull})
			dr := &font.Drawer{
				Dst:  im.(draw.Image), // assert back to draw.Image
				Src:  image.NewUniform(color.RGBA{200, 200, 200, 255}),
				Face: facePN,
			}
			w := int(dr.MeasureString(pageStr) >> 6)
			x := width/2 - w/2 //center
			y := height - margin/2
			dr.Dot = fixed.P(x, y)
			dr.DrawString(pageStr)
		}

		if savePng {
			fname := fmt.Sprintf("page_%d.png", i)
			f, err := os.Create(fname)
			if err != nil {
				fmt.Printf("Error creating file: %v\n", err)
				continue
			}
			if err := png.Encode(f, im); err != nil {
				fmt.Printf("Error encoding PNG: %v\n", err)
			}
			f.Close()
			fmt.Printf("Generated %s\n", fname)
		}
	}

	return imgs, nil
}

// isCJK reports whether r belongs to a CJK script.
func isCJK(r rune) bool {
	return unicode.In(r,
		unicode.Han,      // Chinese characters
		unicode.Hiragana, // Japanese hiragana
		unicode.Katakana, // Japanese katakana
		unicode.Hangul)   // Korean hangul
}

// wrapText splits text into lines that fit within maxWidth.
// - English/Latin words only break at spaces.
// - A single word that is too wide gets hyphenated.
// - CJK characters may break anywhere, and never get spaces around them.
func wrapText(text string, maxWidth int, face font.Face) []string {
	// Helper to detect CJK runes
	isCJK := func(r rune) bool {
		return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
	}

	// 1) Tokenize into either:
	//    - runs of non-CJK (i.e. potential words)
	//    - single CJK runes
	var tokens []string
	var buf []rune
	flush := func() {
		if len(buf) > 0 {
			tokens = append(tokens, string(buf))
			buf = buf[:0]
		}
	}

	for _, r := range text {
		if unicode.IsSpace(r) {
			flush()
		} else if isCJK(r) {
			flush()
			tokens = append(tokens, string(r))
		} else {
			buf = append(buf, r)
		}
	}
	flush()

	// 2) Build lines
	var lines []string
	drawer := &font.Drawer{Face: face}
	current := ""

	for _, tok := range tokens {
		// decide separator: only a space if both neighbors are non-CJK
		sep := ""
		if current != "" {
			first := []rune(tok)[0]
			last := []rune(current)[len([]rune(current))-1]
			if !isCJK(first) && !isCJK(last) {
				sep = " "
			}
		}

		candidate := current + sep + tok
		if int(drawer.MeasureString(candidate)>>6) <= maxWidth {
			current = candidate
			continue
		}

		// if overflow
		if current != "" {
			lines = append(lines, current)
			current = ""
		}

		// tok alone too wide?
		if int(drawer.MeasureString(tok)>>6) <= maxWidth {
			current = tok
		} else {
			// hyphenate non-CJK words, else break CJK one rune at a time
			runes := []rune(tok)
			if !isCJK(runes[0]) && len(runes) > 1 {
				// hyphenate
				for i := 1; i < len(runes); i++ {
					part := string(runes[:i]) + "-"
					if int(drawer.MeasureString(part)>>6) > maxWidth {
						lines = append(lines, string(runes[:i-1])+"-")
						current = string(runes[i-1:])
						break
					}
				}
			} else {
				// CJK or single rune: char-by-char
				for _, r := range runes {
					s := string(r)
					if current == "" {
						current = s
					} else if int(drawer.MeasureString(current+s)>>6) <= maxWidth {
						current += s
					} else {
						lines = append(lines, current)
						current = s
					}
				}
			}
		}
	}

	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

func getSmsPages() {
	firstFetchComplete := false
	startupRetryInterval := 3 * time.Second

	getSmsInterval := func() time.Duration {
		if !firstFetchComplete {
			return startupRetryInterval
		}
		// Same 1-minute cadence as other dynamic data (idle and active).
		return baseSmsInterval
	}

	// Try to collect SMS immediately on startup
	if cfg.ShowSms {
		log.Println("Startup: attempting immediate SMS fetch...")
		lenSmsPagesImages = collectAndDrawSms(&cfg)
		if lenSmsPagesImages == 0 {
			lenSmsPagesImages = 1
		}
		if lenSmsPagesImages > 0 && lastSuccessfulSmsJsonContent != "" {
			firstFetchComplete = true
			log.Println("Startup: SMS fetch successful on first try, lenSmsPagesImages:", lenSmsPagesImages)
		} else {
			log.Println("Startup: SMS fetch failed, will retry every 3 seconds...")
		}
		totalNumPages = cfgNumPages + lenSmsPagesImages
	}

	ticker := time.NewTicker(getSmsInterval())
	defer ticker.Stop()

	for {
		// Either the poll interval elapsed or a config remerge asked for an
		// immediate repaint (theme change).
		select {
		case <-ticker.C:
		case <-smsRerender:
		}
		if cfg.ShowSms {
			if !firstFetchComplete {
				log.Println("Startup retry: attempting SMS fetch...")
			}
			lenSmsPagesImages = collectAndDrawSms(&cfg)
			if lenSmsPagesImages == 0 {
				lenSmsPagesImages = 1
			}

			// Check if this is the first successful fetch
			if !firstFetchComplete && lastSuccessfulSmsJsonContent != "" {
				firstFetchComplete = true
				log.Println("First successful SMS fetch! Switching to normal interval. lenSmsPagesImages:", lenSmsPagesImages)
				ticker.Stop()
				ticker = time.NewTicker(getSmsInterval())
			} else {
				log.Println("collect lenSmsPagesImages:", lenSmsPagesImages)
			}

			totalNumPages = cfgNumPages + lenSmsPagesImages
		} else {
			// SMS disabled - only JSON config pages
			lenSmsPagesImages = 0
			totalNumPages = cfgNumPages
		}
	}
}

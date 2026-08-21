package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"bytes"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// ---------------------------------------------------------------------------
// CaptchaService (P0-4 §5, docs/prd/auth-dual-channel-verification.md)
//
// Human-verification challenges rendered entirely in-process with the Go
// standard library — no external assets, no font rasteriser dependency.
//   * slider: geometric background + puzzle piece; the client drags the
//     piece to the notch and submits its x offset (tolerance ±6px).
//   * text:   4 distorted digits drawn in seven-segment style; the client
//     types them back.
//
// Storage is process-local (sync.Map) with a background sweeper: challenges
// live 3 minutes, issued tokens 10 minutes and are single-use. Multi-replica
// deployments need sticky sessions or a shared store — the interface
// abstraction leaves that door open (PRD §13).
// ---------------------------------------------------------------------------

const (
	captchaChallengeTTL    = 3 * time.Minute
	captchaTokenTTL        = 10 * time.Minute
	captchaMaxAttempts     = 5
	captchaSliderTolerance = 6

	sliderWidth  = 300
	sliderHeight = 180
	pieceSize    = 48

	textCaptchaWidth  = 160
	textCaptchaHeight = 60
)

type captchaChallenge struct {
	id        string
	kind      string // slider | text
	targetX   int    // slider: piece target x; text: unused
	answer    string // text: expected digits
	expiresAt time.Time
	attempts  int
}

type captchaService struct {
	challenges sync.Map // captchaID -> *captchaChallenge
	tokens     sync.Map // captchaToken -> expiry
	challengeType string
}

// NewCaptchaService builds the captcha service. A background sweeper evicts
// expired challenges/tokens so an idle attacker cannot balloon memory.
// Returns the interface so the DI container can satisfy consumers that
// depend on interfaces.CaptchaService.
func NewCaptchaService(cfg *config.Config) interfaces.CaptchaService {
	challengeType := types.CaptchaTypeSlider
	if cfg != nil && cfg.Auth != nil && cfg.Auth.Captcha != nil &&
		strings.TrimSpace(cfg.Auth.Captcha.Type) == types.CaptchaTypeText {
		challengeType = types.CaptchaTypeText
	}
	svc := &captchaService{challengeType: challengeType}
	go svc.sweepLoop()
	return svc
}

func (s *captchaService) sweepLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		s.challenges.Range(func(key, value any) bool {
			if ch, ok := value.(*captchaChallenge); ok && now.After(ch.expiresAt) {
				s.challenges.Delete(key)
			}
			return true
		})
		s.tokens.Range(func(key, value any) bool {
			if exp, ok := value.(time.Time); ok && now.After(exp) {
				s.tokens.Delete(key)
			}
			return true
		})
	}
}

// ChallengeType reports the configured flavour.
func (s *captchaService) ChallengeType() string { return s.challengeType }

// CreateChallenge renders a fresh challenge of the configured type.
func (s *captchaService) CreateChallenge(ctx context.Context) (*types.CaptchaChallengeResponse, error) {
	id := uuid.New().String()
	resp := &types.CaptchaChallengeResponse{
		Success:   true,
		CaptchaID: id,
		Type:      s.challengeType,
	}
	ch := &captchaChallenge{id: id, kind: s.challengeType, expiresAt: time.Now().Add(captchaChallengeTTL)}

	switch s.challengeType {
	case types.CaptchaTypeSlider:
		bg, piece, targetX, pieceY, err := renderSliderChallenge()
		if err != nil {
			return nil, fmt.Errorf("render slider: %w", err)
		}
		ch.targetX = targetX
		resp.BackgroundImage = bg
		resp.PieceImage = piece
		resp.PieceY = pieceY
		resp.PieceSize = pieceSize
	case types.CaptchaTypeText:
		answer, img, err := renderTextChallenge()
		if err != nil {
			return nil, fmt.Errorf("render text captcha: %w", err)
		}
		ch.answer = answer
		resp.TextImage = img
	default:
		// Defensive: unknown configured type falls back to slider.
		s.challengeType = types.CaptchaTypeSlider
		return s.CreateChallenge(ctx)
	}

	s.challenges.Store(id, ch)
	logger.Debugf(ctx, "[captcha] challenge %s issued (type=%s)", id, s.challengeType)
	return resp, nil
}

// VerifyChallenge checks the client's answer and, on success, issues the
// one-time captcha token.
func (s *captchaService) VerifyChallenge(ctx context.Context, req *types.CaptchaVerifyRequest) (*types.CaptchaVerifyResponse, error) {
	raw, ok := s.challenges.Load(req.CaptchaID)
	if !ok {
		return &types.CaptchaVerifyResponse{Success: false, Message: "captcha expired or not found"}, nil
	}
	ch, ok := raw.(*captchaChallenge)
	if !ok || time.Now().After(ch.expiresAt) {
		s.challenges.Delete(req.CaptchaID)
		return &types.CaptchaVerifyResponse{Success: false, Message: "captcha expired"}, nil
	}
	if ch.attempts >= captchaMaxAttempts {
		s.challenges.Delete(req.CaptchaID)
		return &types.CaptchaVerifyResponse{Success: false, Message: "too many attempts"}, nil
	}

	passed := false
	switch ch.kind {
	case types.CaptchaTypeSlider:
		if req.X != nil {
			delta := *req.X - ch.targetX
			if delta < 0 {
				delta = -delta
			}
			passed = delta <= captchaSliderTolerance
		}
	case types.CaptchaTypeText:
		passed = strings.TrimSpace(req.Answer) == ch.answer
	}

	if !passed {
		ch.attempts++
		if ch.attempts >= captchaMaxAttempts {
			s.challenges.Delete(req.CaptchaID)
		}
		return &types.CaptchaVerifyResponse{Success: false, Message: "verification failed"}, nil
	}

	// Passed: burn the challenge and mint the one-time ticket.
	s.challenges.Delete(req.CaptchaID)
	tokenBuf := make([]byte, 32)
	if _, err := rand.Read(tokenBuf); err != nil {
		return nil, fmt.Errorf("mint captcha token: %w", err)
	}
	token := hex.EncodeToString(tokenBuf)
	s.tokens.Store(token, time.Now().Add(captchaTokenTTL))
	logger.Debugf(ctx, "[captcha] challenge %s verified", req.CaptchaID)
	return &types.CaptchaVerifyResponse{Success: true, CaptchaToken: token}, nil
}

// ConsumeToken atomically validates and burns a captcha token.
func (s *captchaService) ConsumeToken(_ context.Context, token string) bool {
	if token == "" {
		return false
	}
	raw, ok := s.tokens.LoadAndDelete(token)
	if !ok {
		return false
	}
	expiry, ok := raw.(time.Time)
	return ok && time.Now().Before(expiry)
}

// ---------------------------------------------------------------------------
// Rendering helpers (pure Go, no external assets)
// ---------------------------------------------------------------------------

// renderSliderChallenge draws the geometric background, punches the notch
// and extracts the puzzle piece. Returns the background data URI, the piece
// data URI, the target x, and the piece's y offset.
func renderSliderChallenge() (bgURI, pieceURI string, targetX, pieceY int, err error) {
	bg := image.NewRGBA(image.Rect(0, 0, sliderWidth, sliderHeight))

	// Soft light base.
	base := color.RGBA{R: uint8(232 + randInt(16)), G: uint8(234 + randInt(16)), B: uint8(238 + randInt(16)), A: 255}
	fillRect(bg, image.Rect(0, 0, sliderWidth, sliderHeight), base)

	// Random soft-coloured shapes — gives the piece edges something to
	// align against, defeating flat-colour screenshot attacks.
	for i := 0; i < 10; i++ {
		c := softRandomColor()
		if randInt(2) == 0 {
			// circle
			r := 14 + randInt(26)
			cx := randInt(sliderWidth)
			cy := randInt(sliderHeight)
			fillCircle(bg, cx, cy, r, c)
		} else {
			// rectangle
			w := 20 + randInt(60)
			h := 14 + randInt(50)
			x := randInt(sliderWidth - w)
			y := randInt(sliderHeight - h)
			fillRect(bg, image.Rect(x, y, x+w, y+h), c)
		}
	}

	// Notch position: keep clear of the left rail (where the piece starts)
	// and the right edge.
	targetX = 70 + randInt(sliderWidth-pieceSize-90)
	pieceY = 30 + randInt(sliderHeight-pieceSize-60)

	// Extract the piece from the pre-notch background first, then punch
	// the notch into the background that ships to the client.
	piece := image.NewRGBA(image.Rect(0, 0, pieceSize, pieceSize))
	for y := 0; y < pieceSize; y++ {
		for x := 0; x < pieceSize; x++ {
			src := bg.RGBAAt(targetX+x, pieceY+y)
			piece.SetRGBA(x, y, color.RGBA{R: src.R, G: src.G, B: src.B, A: 230})
		}
	}
	// White outline so the piece reads against any background region.
	for x := 0; x < pieceSize; x++ {
		piece.SetRGBA(x, 0, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		piece.SetRGBA(x, pieceSize-1, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	}
	for y := 0; y < pieceSize; y++ {
		piece.SetRGBA(0, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		piece.SetRGBA(pieceSize-1, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
	}

	// Punch the notch: translucent dark plate + light inner border.
	notch := image.Rect(targetX, pieceY, targetX+pieceSize, pieceY+pieceSize)
	fillRect(bg, notch, color.RGBA{R: 30, G: 30, B: 34, A: 96})
	strokeRect(bg, notch, color.RGBA{R: 255, G: 255, B: 255, A: 200})

	// Sparse speckle.
	for i := 0; i < 220; i++ {
		x := randInt(sliderWidth)
		y := randInt(sliderHeight)
		bg.SetRGBA(x, y, color.RGBA{R: uint8(randInt(255)), G: uint8(randInt(255)), B: uint8(randInt(255)), A: 40})
	}

	if bgURI, err = pngDataURI(bg); err != nil {
		return "", "", 0, 0, err
	}
	if pieceURI, err = pngDataURI(piece); err != nil {
		return "", "", 0, 0, err
	}
	return bgURI, pieceURI, targetX, pieceY, nil
}

// renderTextChallenge draws 4 digits in seven-segment style with jitter,
// interference lines and speckle. Returns the answer and the data URI.
func renderTextChallenge() (answer, imgURI string, err error) {
	img := image.NewRGBA(image.Rect(0, 0, textCaptchaWidth, textCaptchaHeight))
	fillRect(img, image.Rect(0, 0, textCaptchaWidth, textCaptchaHeight), color.RGBA{R: 246, G: 247, B: 249, A: 255})

	digits := make([]byte, 4)
	if _, err = rand.Read(digits); err != nil {
		return "", "", err
	}
	answer = ""
	for i := 0; i < 4; i++ {
		d := int(digits[i]) % 10
		answer += fmt.Sprintf("%d", d)

		// Seven-segment cell with per-digit jitter.
		w, h := 18, 34
		x0 := 14 + i*36
		y0 := 12 + randInt(8) - 4
		c := darkRandomColor()
		drawSevenSegment(img, d, x0, y0, w, h, c)
	}

	// Interference lines.
	for i := 0; i < 4; i++ {
		drawLine(img, randInt(textCaptchaWidth), randInt(textCaptchaHeight),
			randInt(textCaptchaWidth), randInt(textCaptchaHeight), darkRandomColor())
	}
	// Speckle.
	for i := 0; i < 160; i++ {
		img.SetRGBA(randInt(textCaptchaWidth), randInt(textCaptchaHeight),
			color.RGBA{R: uint8(randInt(255)), G: uint8(randInt(255)), B: uint8(randInt(255)), A: 60})
	}

	imgURI, err = pngDataURI(img)
	return answer, imgURI, err
}

// sevenSegmentSegments maps digit -> lit segments {a,b,c,d,e,f,g}.
var sevenSegmentSegments = map[int][]bool{
	0: {true, true, true, true, true, true, false},
	1: {false, true, true, false, false, false, false},
	2: {true, true, false, true, true, false, true},
	3: {true, true, true, true, false, false, true},
	4: {false, true, true, false, false, true, true},
	5: {true, false, true, true, false, true, true},
	6: {true, false, true, true, true, true, true},
	7: {true, true, true, false, false, false, false},
	8: {true, true, true, true, true, true, true},
	9: {true, true, true, true, false, true, true},
}

// drawSevenSegment renders one digit into a w×h cell at (x0,y0).
func drawSevenSegment(img *image.RGBA, digit, x0, y0, w, h int, c color.RGBA) {
	t := 4 // segment thickness
	lit := sevenSegmentSegments[digit]
	mid := y0 + h/2
	a, b, cc, d, e, f, g := lit[0], lit[1], lit[2], lit[3], lit[4], lit[5], lit[6]
	if a {
		fillRect(img, image.Rect(x0+2, y0, x0+w-2, y0+t), c)
	}
	if b {
		fillRect(img, image.Rect(x0+w-t, y0+2, x0+w, mid-1), c)
	}
	if cc {
		fillRect(img, image.Rect(x0+w-t, mid+1, x0+w, y0+h-2), c)
	}
	if d {
		fillRect(img, image.Rect(x0+2, y0+h-t, x0+w-2, y0+h), c)
	}
	if e {
		fillRect(img, image.Rect(x0, mid+1, x0+t, y0+h-2), c)
	}
	if f {
		fillRect(img, image.Rect(x0, y0+2, x0+t, mid-1), c)
	}
	if g {
		fillRect(img, image.Rect(x0+2, mid-t/2, x0+w-2, mid-t/2+t), c)
	}
}

// ---------------------------------------------------------------------------
// Drawing primitives
// ---------------------------------------------------------------------------

func fillRect(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if x >= 0 && y >= 0 && x < img.Bounds().Dx() && y < img.Bounds().Dy() {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

func strokeRect(img *image.RGBA, r image.Rectangle, c color.RGBA) {
	for x := r.Min.X; x < r.Max.X; x++ {
		img.SetRGBA(x, r.Min.Y, c)
		img.SetRGBA(x, r.Max.Y-1, c)
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		img.SetRGBA(r.Min.X, y, c)
		img.SetRGBA(r.Max.X-1, y, c)
	}
}

func fillCircle(img *image.RGBA, cx, cy, r int, c color.RGBA) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r*r {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

func drawLine(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	dx := x1 - x0
	dy := y1 - y0
	steps := dx
	if steps < 0 {
		steps = -steps
	}
	if abs := dy; dy < 0 {
		steps += -abs
	} else {
		steps += abs
	}
	if steps == 0 {
		img.SetRGBA(x0, y0, c)
		return
	}
	for i := 0; i <= steps; i++ {
		x := x0 + dx*i/steps
		y := y0 + dy*i/steps
		img.SetRGBA(x, y, c)
	}
}

func pngDataURI(img image.Image) (string, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// randInt returns a crypto-random non-negative int in [0, n).
func randInt(n int) int {
	if n <= 0 {
		return 0
	}
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		// Crypto RNG failure is effectively fatal; fall back to a
		// time-derived value rather than panicking in a handler.
		return int(time.Now().UnixNano() % int64(n))
	}
	v := int(buf[0]) | int(buf[1])<<8 | int(buf[2])<<16 | int(buf[3])<<24
	if v < 0 {
		v = -v
	}
	return v % n
}

func softRandomColor() color.RGBA {
	return color.RGBA{
		R: uint8(150 + randInt(105)),
		G: uint8(150 + randInt(105)),
		B: uint8(150 + randInt(105)),
		A: uint8(90 + randInt(60)),
	}
}

func darkRandomColor() color.RGBA {
	return color.RGBA{
		R: uint8(30 + randInt(90)),
		G: uint8(30 + randInt(90)),
		B: uint8(30 + randInt(90)),
		A: 255,
	}
}

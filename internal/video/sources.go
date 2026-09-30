package video

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Sample is one H.264 access unit in Annex B form.
type Sample struct {
	Data     []byte
	Key      bool // contains an IDR slice
	Duration time.Duration
}

// Source produces samples until ctx ends or it fails. Sink never blocks for long; a source must not wait on it.
type Source interface {
	Name() string
	Run(ctx context.Context, sink func(Sample)) error
	// RequestKeyframe asks for an IDR as soon as possible (a new viewer session or a PLI).
	RequestKeyframe()
}

// ---- test pattern ----

// TestPattern draws colour bars with a moving box and encodes them with the PCM encoder: after the first frame only
// the macroblocks the box touched are sent.
type TestPattern struct {
	W, H, FPS int
	GOP       time.Duration // keyframe interval (default 4 s)
	keyReq    atomic.Bool
}

func (t *TestPattern) Name() string     { return "test-pattern" }
func (t *TestPattern) RequestKeyframe() { t.keyReq.Store(true) }

var bars = [8][3]byte{ // BT.601 limited range Y, Cb, Cr: white, yellow, cyan, green, magenta, red, blue, black
	{180, 128, 128}, {162, 44, 142}, {131, 156, 44}, {112, 72, 58}, {84, 184, 198}, {65, 100, 212}, {35, 212, 114}, {16, 128, 128},
}

// DrawPattern fills f with frame n of the pattern.
func DrawPattern(f *Frame, n int) {
	cw, ch := f.W/2, f.H/2
	for y := 0; y < f.H; y++ {
		for x := 0; x < f.W; x++ {
			f.Y[y*f.W+x] = bars[x*8/f.W][0]
		}
	}
	for y := 0; y < ch; y++ {
		for x := 0; x < cw; x++ {
			b := bars[x*16/f.W]
			f.Cb[y*cw+x], f.Cr[y*cw+x] = b[1], b[2]
		}
	}
	// A 32×32 box sweeping left to right across the middle, one macroblock per frame.
	span := f.W/16 - 1
	pos := n % (2 * span)
	if pos >= span {
		pos = 2*span - pos
	}
	bx, by := pos*16, (f.H/2/16)*16-16
	for y := by; y < by+32 && y < f.H; y++ {
		for x := bx; x < bx+32 && x < f.W; x++ {
			f.Y[y*f.W+x] = 235
		}
	}
	for y := by / 2; y < (by+32)/2 && y < ch; y++ {
		for x := bx / 2; x < (bx+32)/2 && x < cw; x++ {
			f.Cb[y*cw+x], f.Cr[y*cw+x] = 128, 128
		}
	}
}

func (t *TestPattern) Run(ctx context.Context, sink func(Sample)) error {
	if t.W == 0 {
		t.W, t.H = 320, 240
	}
	if t.FPS <= 0 {
		t.FPS = 10
	}
	if t.GOP <= 0 {
		t.GOP = 4 * time.Second
	}
	enc, err := NewPCMEncoder(t.W, t.H)
	if err != nil {
		return err
	}
	period := time.Second / time.Duration(t.FPS)
	tick := time.NewTicker(period)
	defer tick.Stop()
	var prev *Frame
	lastKey := time.Time{}
	for n := 0; ; n++ {
		f := NewFrame(t.W, t.H)
		DrawPattern(f, n)
		key := prev == nil || time.Since(lastKey) >= t.GOP || t.keyReq.Swap(false)
		var coded []int
		if !key {
			coded = ChangedMBs(prev, f, 0)
		} else {
			lastKey = time.Now()
		}
		sink(Sample{Data: enc.Encode(f, key, coded), Key: key, Duration: period})
		prev = f
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// ---- Annex B parsing ----

// AccessUnits splits an Annex B byte stream into access units, calling emit for each. A new access unit starts at an
// access unit delimiter, SPS, PPS or SEI after a slice, or at a slice whose first_mb_in_slice is 0.
func AccessUnits(r io.Reader, emit func(au []byte, key bool)) error {
	br := bufio.NewReaderSize(r, 1<<20)
	var au []byte
	hasVCL, key := false, false
	flush := func() {
		if hasVCL {
			emit(au, key)
		}
		au, hasVCL, key = nil, false, false
	}
	return splitNALs(br, func(n []byte) {
		if len(n) == 0 {
			return
		}
		typ := n[0] & 0x1f
		vcl := typ == 1 || typ == 5
		if hasVCL && (typ == 9 || typ == 7 || typ == 8 || typ == 6 || (vcl && firstMBZero(n))) {
			flush()
		}
		au = append(au, 0, 0, 0, 1)
		au = append(au, n...)
		if vcl {
			hasVCL = true
		}
		if typ == 5 {
			key = true
		}
	}, flush)
}

// firstMBZero reports whether a slice NAL's first_mb_in_slice (the first ue(v) after the header) is 0, i.e. its
// first bit is 1.
func firstMBZero(n []byte) bool { return len(n) > 1 && n[1]&0x80 != 0 }

// splitNALs finds start codes (00 00 01 or 00 00 00 01) and calls nalFn with each NAL (header onwards).
func splitNALs(br *bufio.Reader, nalFn func([]byte), end func()) error {
	var cur []byte
	started := false
	zeros := 0
	buf := make([]byte, 64*1024)
	for {
		n, err := br.Read(buf)
		for _, b := range buf[:n] {
			if b == 0 {
				zeros++
				continue
			}
			if b == 1 && zeros >= 2 {
				if started {
					nalFn(cur)
				}
				cur, started, zeros = cur[:0:0], true, 0
				continue
			}
			for ; zeros > 0; zeros-- {
				cur = append(cur, 0)
			}
			cur = append(cur, b)
		}
		if err != nil {
			if started {
				nalFn(cur)
			}
			end()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// ---- a child process writing H.264 ----

// CommandSource runs a program that writes an H.264 Annex B stream to stdout (rpicam-vid, ffmpeg) and restarts it
// with backoff when it exits.
type CommandSource struct {
	Argv []string
	Log  *slog.Logger
}

func (c *CommandSource) Name() string { return "command:" + c.Argv[0] }

// RequestKeyframe cannot reach the child; rpicam-vid and ffmpeg are started with a short keyframe interval instead.
func (c *CommandSource) RequestKeyframe() {}

func (c *CommandSource) Run(ctx context.Context, sink func(Sample)) error {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.runOnce(ctx, sink)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		if c.Log != nil {
			c.Log.Warn("video source exited; restarting", "argv0", c.Argv[0], "err", err, "in", backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return nil
}

func (c *CommandSource) runOnce(ctx context.Context, sink func(Sample)) error {
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	last := time.Now()
	perr := AccessUnits(out, func(au []byte, key bool) {
		now := time.Now()
		d := now.Sub(last)
		last = now
		sink(Sample{Data: au, Key: key, Duration: clampDur(d)})
	})
	werr := cmd.Wait()
	if perr != nil {
		return perr
	}
	if werr != nil {
		return fmt.Errorf("%w: %s", werr, bytes.TrimSpace(stderr.Bytes()))
	}
	return errors.New("exited")
}

func clampDur(d time.Duration) time.Duration {
	if d < time.Millisecond {
		return time.Millisecond
	}
	if d > time.Second {
		return time.Second
	}
	return d
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	_, _ = l.w.Write(q)
	return len(p), nil
}

// ---- JPEG frames from a plugin ----

// JPEGSource turns JPEG files announced by a plugin into H.264: through ffmpeg when it is installed, otherwise with
// the built-in PCM encoder at a low frame rate, sending only the macroblocks that changed.
type JPEGSource struct {
	FFmpeg string // path to ffmpeg; empty = look on PATH; "none" = always use the fallback
	FPS    int
	W, H   int // output size for the fallback (default 320×240)
	Log    *slog.Logger

	mu     sync.Mutex
	latest chan []byte
	keyReq atomic.Bool
}

func (j *JPEGSource) Name() string     { return "plugin-jpeg" }
func (j *JPEGSource) RequestKeyframe() { j.keyReq.Store(true) }

func (j *JPEGSource) ch() chan []byte {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.latest == nil {
		j.latest = make(chan []byte, 1)
	}
	return j.latest
}

// Push hands over a frame file. It reads the file at once (plugins rotate their files) and keeps only the newest
// frame if the encoder is busy.
func (j *JPEGSource) Push(path string) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 4 || b[0] != 0xff || b[1] != 0xd8 {
		return
	}
	c := j.ch()
	select {
	case c <- b:
	default:
		select {
		case <-c:
		default:
		}
		select {
		case c <- b:
		default:
		}
	}
}

func (j *JPEGSource) Run(ctx context.Context, sink func(Sample)) error {
	if j.FPS <= 0 {
		j.FPS = 10
	}
	ff := j.FFmpeg
	if ff == "" {
		ff, _ = exec.LookPath("ffmpeg")
	} else if ff == "none" {
		ff = ""
	}
	if ff != "" {
		return j.runFFmpeg(ctx, ff, sink)
	}
	if j.Log != nil {
		j.Log.Warn("ffmpeg not found; publishing camera frames with the built-in low-rate encoder")
	}
	return j.runPCM(ctx, sink)
}

func (j *JPEGSource) runFFmpeg(ctx context.Context, ff string, sink func(Sample)) error {
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "image2pipe", "-framerate", fmt.Sprint(j.FPS),
		"-c:v", "mjpeg", "-i", "pipe:0", "-an", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency",
		"-profile:v", "baseline", "-pix_fmt", "yuv420p", "-g", fmt.Sprint(j.FPS * 2), "-bf", "0",
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2", "-f", "h264", "pipe:1"}
	cmd := exec.CommandContext(ctx, ff, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		defer stdin.Close()
		c := j.ch()
		for {
			select {
			case <-ctx.Done():
				return
			case b := <-c:
				if _, err := stdin.Write(b); err != nil {
					return
				}
			}
		}
	}()
	last := time.Now()
	err = AccessUnits(stdout, func(au []byte, key bool) {
		now := time.Now()
		sink(Sample{Data: au, Key: key, Duration: clampDur(now.Sub(last))})
		last = now
	})
	_ = cmd.Wait()
	return err
}

func (j *JPEGSource) runPCM(ctx context.Context, sink func(Sample)) error {
	w, h := j.W, j.H
	if w == 0 {
		w, h = 320, 240
	}
	enc, err := NewPCMEncoder(w, h)
	if err != nil {
		return err
	}
	fps := j.FPS
	if fps > 5 {
		fps = 5
	}
	period := time.Second / time.Duration(fps)
	var prev *Frame
	lastKey, lastSent := time.Time{}, time.Now()
	c := j.ch()
	for {
		var b []byte
		select {
		case <-ctx.Done():
			return nil
		case b = <-c:
		}
		img, err := jpeg.Decode(bytes.NewReader(b))
		if err != nil {
			continue
		}
		f := FrameFromImage(img, w, h)
		key := prev == nil || time.Since(lastKey) > 10*time.Second || j.keyReq.Swap(false)
		var coded []int
		if key {
			lastKey = time.Now()
		} else {
			coded = ChangedMBs(prev, f, 6)
		}
		now := time.Now()
		sink(Sample{Data: enc.Encode(f, key, coded), Key: key, Duration: clampDur(now.Sub(lastSent))})
		lastSent, prev = now, f
		// Rate limit.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(period):
		}
	}
}

// FrameFromImage scales an image to w×h (nearest neighbour) in YUV 4:2:0.
func FrameFromImage(img image.Image, w, h int) *Frame {
	f := NewFrame(w, h)
	b := img.Bounds()
	cw := w / 2
	for y := 0; y < h; y++ {
		sy := b.Min.Y + y*b.Dy()/h
		for x := 0; x < w; x++ {
			sx := b.Min.X + x*b.Dx()/w
			yy, cb, cr := toYCbCr(img, sx, sy)
			f.Y[y*w+x] = yy
			if x%2 == 0 && y%2 == 0 {
				f.Cb[(y/2)*cw+x/2], f.Cr[(y/2)*cw+x/2] = cb, cr
			}
		}
	}
	return f
}

func toYCbCr(img image.Image, x, y int) (byte, byte, byte) {
	if yc, ok := img.(*image.YCbCr); ok {
		c := yc.YCbCrAt(x, y)
		return c.Y, c.Cb, c.Cr
	}
	if g, ok := img.(*image.Gray); ok {
		return g.GrayAt(x, y).Y, 128, 128
	}
	r, g, bl, _ := img.At(x, y).RGBA()
	R, G, B := float64(r>>8), float64(g>>8), float64(bl>>8)
	Y := 0.299*R + 0.587*G + 0.114*B
	return clampByte(Y), clampByte(128 - 0.168736*R - 0.331264*G + 0.5*B), clampByte(128 + 0.5*R - 0.418688*G - 0.081312*B)
}

func clampByte(v float64) byte {
	if v < 1 {
		return 1
	}
	if v > 254 {
		return 254
	}
	return byte(v + 0.5)
}

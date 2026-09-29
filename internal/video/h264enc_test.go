package video

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestExpGolomb(t *testing.T) {
	w := &bitWriter{}
	for _, v := range []uint32{0, 1, 2, 3, 7} {
		w.ue(v)
	}
	// 1 010 011 00100 0001000 → 1010 0110 0100 0001 000 + pad
	got := w.trailing()
	want := []byte{0xa6, 0x41, 0x01} // ...0001000 then stop bit 1 → 0000 0001 (with trailing)
	if !bytes.Equal(got[:2], want[:2]) {
		t.Fatalf("got %x", got)
	}
	w = &bitWriter{}
	w.se(1)
	w.se(-1)
	w.se(2) // 010 011 00100, then the stop bit
	if b := w.trailing(); b[0] != 0x4c || b[1] != 0x90 {
		t.Fatalf("se %x", b)
	}
}

func TestEmulationPrevention(t *testing.T) {
	n := nal(3, 5, []byte{0, 0, 1, 0, 0, 0, 0, 0, 3})
	want := []byte{0, 0, 0, 1, 0x65, 0, 0, 3, 1, 0, 0, 3, 0, 0, 3, 0, 3}
	if !bytes.Equal(n, want) {
		t.Fatalf("got % x", n)
	}
}

func TestAccessUnitsRoundTrip(t *testing.T) {
	enc, _ := NewPCMEncoder(64, 48)
	var stream []byte
	var prev *Frame
	for i := 0; i < 5; i++ {
		f := NewFrame(64, 48)
		DrawPattern(f, i)
		var coded []int
		if prev != nil {
			coded = ChangedMBs(prev, f, 0)
		}
		stream = append(stream, enc.Encode(f, false, coded)...)
		prev = f
	}
	var keys []bool
	err := AccessUnits(bytes.NewReader(stream), func(au []byte, key bool) { keys = append(keys, key) })
	if err != nil || len(keys) != 5 || !keys[0] || keys[1] {
		t.Fatalf("%v %v", err, keys)
	}
}

func TestChangedMBs(t *testing.T) {
	a, b := NewFrame(32, 32), NewFrame(32, 32)
	b.Y[16*32+20] = 200 // macroblock (1,1) → address 3
	if got := ChangedMBs(a, b, 0); len(got) != 1 || got[0] != 3 {
		t.Fatalf("%v", got)
	}
	if got := ChangedMBs(nil, b, 0); len(got) != 4 {
		t.Fatalf("%v", got)
	}
}

func TestTestPatternRuns(t *testing.T) {
	tp := &TestPattern{W: 64, H: 48, FPS: 50}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var n, keys int
	tp.Run(ctx, func(s Sample) {
		n++
		if s.Key {
			keys++
		}
	})
	if n < 3 || keys != 1 {
		t.Fatalf("frames %d keys %d", n, keys)
	}
}

// TestDecodesWithFFmpegLibraries decodes the encoder's output with PyAV (libavcodec) when it is installed and checks
// the pixels: the strongest evidence the bitstream is valid H.264.
func TestDecodesWithFFmpegLibraries(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil || exec.Command(py, "-c", "import av, numpy").Run() != nil {
		t.Skip("python3 with PyAV and numpy (pip install av numpy) not available")
	}
	enc, _ := NewPCMEncoder(320, 240)
	var stream []byte
	var prev *Frame
	var last *Frame
	for i := 0; i < 30; i++ {
		f := NewFrame(320, 240)
		DrawPattern(f, i)
		var coded []int
		if prev != nil {
			coded = ChangedMBs(prev, f, 0)
		}
		stream = append(stream, enc.Encode(f, i == 20, coded)...)
		prev, last = f, f
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "p.h264")
	os.WriteFile(path, stream, 0o600)
	os.WriteFile(filepath.Join(dir, "y.raw"), last.Y, 0o600)
	script := `
import av, sys
c = av.open(sys.argv[1], format="h264")
frames = [f for f in c.decode(video=0)]
last = frames[-1].to_ndarray(format="yuv420p")
want = open(sys.argv[2], "rb").read()
y = last[:240].tobytes()
diff = sum(1 for a, b in zip(y, want) if a != b)
print(len(frames), frames[-1].width, frames[-1].height, diff)
`
	out, err := exec.Command(py, "-c", script, path, filepath.Join(dir, "y.raw")).CombinedOutput()
	if err != nil {
		t.Fatalf("decode failed: %v\n%s", err, out)
	}
	if got := string(bytes.TrimSpace(out)); got != "30 320 240 0" {
		t.Fatalf("decoder said %q, want 30 frames 320x240 with the last frame identical", got)
	}
}

func TestCommandSource(t *testing.T) {
	enc, _ := NewPCMEncoder(32, 32)
	var stream []byte
	for i := 0; i < 4; i++ {
		f := NewFrame(32, 32)
		DrawPattern(f, i)
		stream = append(stream, enc.Encode(f, i == 0, []int{0, 3})...)
	}
	path := filepath.Join(t.TempDir(), "s.h264")
	os.WriteFile(path, stream, 0o600)
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("no cat")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	got := make(chan Sample, 100)
	go (&CommandSource{Argv: []string{cat, path}}).Run(ctx, func(s Sample) { got <- s })
	for i := 0; i < 4; i++ {
		select {
		case s := <-got:
			if s.Key != (i == 0) {
				t.Fatalf("sample %d key %v", i, s.Key)
			}
		case <-ctx.Done():
			t.Fatalf("only %d samples", i)
		}
	}
}

func TestJPEGFallback(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for i := range img.Pix {
		img.Pix[i] = byte(i)
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, nil)
	path := filepath.Join(t.TempDir(), "f.jpg")
	os.WriteFile(path, buf.Bytes(), 0o600)
	src := &JPEGSource{FFmpeg: "none", W: 64, H: 48}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got := make(chan Sample, 10)
	go src.Run(ctx, func(s Sample) { got <- s })
	src.Push(path)
	select {
	case s := <-got:
		if !s.Key || len(s.Data) < 64*48 {
			t.Fatalf("key %v len %d", s.Key, len(s.Data))
		}
	case <-ctx.Done():
		t.Fatal("no sample")
	}
	src.Push(filepath.Join(t.TempDir(), "missing.jpg")) // ignored
}

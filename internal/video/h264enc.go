package video

// A minimal H.264 encoder that needs no codec library: Constrained Baseline, CAVLC, every coded macroblock is I_PCM
// (raw samples) and every other macroblock of a P frame is P_Skip (copied from the previous frame). It is not
// efficient, but it is small, exact and decodable everywhere, which is what the dry-run test pattern and the
// no-ffmpeg fallback for JPEG frames need: only the macroblocks that changed are sent after the first frame.
//
// The SPS advertises profile-level-id 42e01f, the value WebRTC endpoints expect.

import "errors"

// bitWriter writes big-endian bit strings with Exp-Golomb helpers (H.264 §7.2, §9.1).
type bitWriter struct {
	buf []byte
	cur byte
	n   uint8
}

func (w *bitWriter) bit(b uint) {
	w.cur = w.cur<<1 | byte(b&1)
	w.n++
	if w.n == 8 {
		w.buf = append(w.buf, w.cur)
		w.cur, w.n = 0, 0
	}
}

func (w *bitWriter) bits(v uint64, n int) {
	for i := n - 1; i >= 0; i-- {
		w.bit(uint(v>>uint(i)) & 1)
	}
}

func (w *bitWriter) ue(v uint32) {
	x := uint64(v) + 1
	lz := 0
	for t := x; t > 1; t >>= 1 {
		lz++
	}
	w.bits(0, lz)
	w.bits(x, lz+1)
}

func (w *bitWriter) se(v int32) {
	if v > 0 {
		w.ue(uint32(2*v - 1))
	} else {
		w.ue(uint32(-2 * v))
	}
}

func (w *bitWriter) aligned() bool { return w.n == 0 }

func (w *bitWriter) alignZero() {
	for !w.aligned() {
		w.bit(0)
	}
}

// byteAligned appends whole bytes; the writer must be aligned.
func (w *bitWriter) raw(b []byte) { w.buf = append(w.buf, b...) }

// trailing writes rbsp_trailing_bits: a one, then zeros to the byte boundary.
func (w *bitWriter) trailing() []byte {
	w.bit(1)
	w.alignZero()
	return w.buf
}

// nal wraps an RBSP in an Annex B NAL unit with emulation prevention.
func nal(refIdc, typ byte, rbsp []byte) []byte {
	out := make([]byte, 0, len(rbsp)+len(rbsp)/64+5)
	out = append(out, 0, 0, 0, 1, refIdc<<5|typ)
	zeros := 0
	for _, b := range rbsp {
		if zeros >= 2 && b <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// Frame is a YUV 4:2:0 picture whose width and height are multiples of 16.
type Frame struct {
	W, H      int
	Y, Cb, Cr []byte
}

func NewFrame(w, h int) *Frame {
	return &Frame{W: w, H: h, Y: make([]byte, w*h), Cb: make([]byte, w*h/4), Cr: make([]byte, w*h/4)}
}

// PCMEncoder encodes Frames.
type PCMEncoder struct {
	W, H     int
	mbw, mbh int
	frameNum uint32
	idrID    uint32
	started  bool
}

func NewPCMEncoder(w, h int) (*PCMEncoder, error) {
	if w <= 0 || h <= 0 || w%16 != 0 || h%16 != 0 {
		return nil, errors.New("video: width and height must be positive multiples of 16")
	}
	return &PCMEncoder{W: w, H: h, mbw: w / 16, mbh: h / 16}, nil
}

// MBs is the number of macroblocks per frame.
func (e *PCMEncoder) MBs() int { return e.mbw * e.mbh }

func (e *PCMEncoder) sps() []byte {
	w := &bitWriter{}
	w.bits(66, 8)   // profile_idc: Baseline
	w.bits(0xe0, 8) // constraint_set0..2 = 1 (Constrained Baseline), reserved zeros
	w.bits(31, 8)   // level_idc 3.1
	w.ue(0)         // seq_parameter_set_id
	w.ue(0)         // log2_max_frame_num_minus4 → frame_num has 4 bits
	w.ue(2)         // pic_order_cnt_type 2: output order = decode order
	w.ue(1)         // max_num_ref_frames
	w.bit(0)        // gaps_in_frame_num_value_allowed_flag
	w.ue(uint32(e.mbw - 1))
	w.ue(uint32(e.mbh - 1))
	w.bit(1) // frame_mbs_only_flag
	w.bit(1) // direct_8x8_inference_flag
	w.bit(0) // frame_cropping_flag
	w.bit(0) // vui_parameters_present_flag
	return nal(3, 7, w.trailing())
}

func (e *PCMEncoder) pps() []byte {
	w := &bitWriter{}
	w.ue(0)  // pic_parameter_set_id
	w.ue(0)  // seq_parameter_set_id
	w.bit(0) // entropy_coding_mode_flag: CAVLC
	w.bit(0) // bottom_field_pic_order_in_frame_present_flag
	w.ue(0)  // num_slice_groups_minus1
	w.ue(0)  // num_ref_idx_l0_default_active_minus1
	w.ue(0)  // num_ref_idx_l1_default_active_minus1
	w.bit(0) // weighted_pred_flag
	w.bits(0, 2)
	w.se(0)  // pic_init_qp_minus26
	w.se(0)  // pic_init_qs_minus26
	w.se(0)  // chroma_qp_index_offset
	w.bit(1) // deblocking_filter_control_present_flag (so slices can turn it off)
	w.bit(0) // constrained_intra_pred_flag
	w.bit(0) // redundant_pic_cnt_present_flag
	return nal(3, 8, w.trailing())
}

func (e *PCMEncoder) pcm(w *bitWriter, f *Frame, mb int) {
	mx, my := mb%e.mbw, mb/e.mbw
	w.alignZero() // pcm_alignment_zero_bit
	for y := 0; y < 16; y++ {
		off := (my*16+y)*f.W + mx*16
		w.raw(f.Y[off : off+16])
	}
	cw := f.W / 2
	for _, plane := range [][]byte{f.Cb, f.Cr} {
		for y := 0; y < 8; y++ {
			off := (my*8+y)*cw + mx*8
			w.raw(plane[off : off+8])
		}
	}
}

// Encode returns one access unit in Annex B. idr (or the first frame) codes every macroblock and repeats SPS/PPS;
// otherwise only the macroblocks listed in coded (ascending addresses) are sent and the rest are skipped.
func (e *PCMEncoder) Encode(f *Frame, idr bool, coded []int) []byte {
	if f.W != e.W || f.H != e.H {
		panic("video: frame size does not match the encoder")
	}
	if !e.started {
		idr = true
	}
	e.started = true
	w := &bitWriter{}
	w.ue(0) // first_mb_in_slice
	if idr {
		e.frameNum = 0
		w.ue(7) // slice_type I (all slices of the picture)
	} else {
		w.ue(5) // slice_type P
	}
	w.ue(0)                           // pic_parameter_set_id
	w.bits(uint64(e.frameNum&0xf), 4) // frame_num
	if idr {
		w.ue(e.idrID & 0xffff) // idr_pic_id
		e.idrID++
	} else {
		w.bit(0) // num_ref_idx_active_override_flag
		w.bit(0) // ref_pic_list_modification_flag_l0
	}
	// dec_ref_pic_marking (nal_ref_idc != 0)
	if idr {
		w.bit(0) // no_output_of_prior_pics_flag
		w.bit(0) // long_term_reference_flag
	} else {
		w.bit(0) // adaptive_ref_pic_marking_mode_flag
	}
	w.se(0) // slice_qp_delta
	w.ue(1) // disable_deblocking_filter_idc: off (PCM needs none, and skips copy exactly)

	if idr {
		for mb := 0; mb < e.MBs(); mb++ {
			w.ue(25) // mb_type I_PCM
			e.pcm(w, f, mb)
		}
	} else {
		next := 0
		for _, mb := range coded {
			if mb < next || mb >= e.MBs() {
				continue
			}
			w.ue(uint32(mb - next)) // mb_skip_run
			w.ue(30)                // mb_type: intra in a P slice, 5 + I_PCM(25)
			e.pcm(w, f, mb)
			next = mb + 1
		}
		if rest := e.MBs() - next; rest > 0 {
			w.ue(uint32(rest))
		}
	}
	slice := w.trailing()
	e.frameNum = (e.frameNum + 1) & 0xf
	var au []byte
	if idr {
		au = append(au, e.sps()...)
		au = append(au, e.pps()...)
		return append(au, nal(3, 5, slice)...)
	}
	return append(au, nal(2, 1, slice)...)
}

// ChangedMBs lists the macroblocks whose samples differ between a and b by more than threshold (mean absolute luma
// difference per sample). A nil a means everything changed.
func ChangedMBs(a, b *Frame, threshold int) []int {
	mbw, mbh := b.W/16, b.H/16
	var out []int
	for my := 0; my < mbh; my++ {
		for mx := 0; mx < mbw; mx++ {
			if a == nil {
				out = append(out, my*mbw+mx)
				continue
			}
			sum := 0
			for y := 0; y < 16; y++ {
				off := (my*16+y)*b.W + mx*16
				for x := 0; x < 16; x++ {
					d := int(a.Y[off+x]) - int(b.Y[off+x])
					if d < 0 {
						d = -d
					}
					sum += d
				}
			}
			if sum > threshold*256 || chromaDiff(a, b, mx, my) > threshold*64 {
				out = append(out, my*mbw+mx)
			}
		}
	}
	return out
}

func chromaDiff(a, b *Frame, mx, my int) int {
	cw := b.W / 2
	sum := 0
	for _, p := range [][2][]byte{{a.Cb, b.Cb}, {a.Cr, b.Cr}} {
		for y := 0; y < 8; y++ {
			off := (my*8+y)*cw + mx*8
			for x := 0; x < 8; x++ {
				d := int(p[0][off+x]) - int(p[1][off+x])
				if d < 0 {
					d = -d
				}
				sum += d
			}
		}
	}
	return sum
}

package video

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
)

// WHIPReceiver is a minimal WHIP endpoint for tests (and the fake Bot server): it answers offers, receives the H.264
// track and counts RTP packets. It checks the bearer publish key.
type WHIPReceiver struct {
	PublishKey string

	Packets atomic.Int64
	Deletes atomic.Int64
	BadAuth atomic.Int64

	mu  sync.Mutex
	pcs map[string]*webrtc.PeerConnection
	n   int
}

// SetPublishKey changes the bearer key the receiver demands. It is safe to call while the receiver is serving; set
// PublishKey directly only before then.
func (w *WHIPReceiver) SetPublishKey(k string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.PublishKey = k
}

func (w *WHIPReceiver) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	key := w.PublishKey
	w.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+key {
		w.BadAuth.Add(1)
		http.Error(rw, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPost:
		w.post(rw, r)
	case http.MethodDelete:
		w.Deletes.Add(1)
		w.mu.Lock()
		if pc := w.pcs[r.URL.Path]; pc != nil {
			pc.Close()
			delete(w.pcs, r.URL.Path)
		}
		w.mu.Unlock()
		rw.WriteHeader(http.StatusOK)
	default:
		http.Error(rw, "method", http.StatusMethodNotAllowed)
	}
}

func (w *WHIPReceiver) post(rw http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/sdp") {
		http.Error(rw, "content type", http.StatusUnsupportedMediaType)
		return
	}
	offer, _ := io.ReadAll(r.Body)
	m := &webrtc.MediaEngine{}
	_ = m.RegisterDefaultCodecs()
	se := webrtc.SettingEngine{}
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(se))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			if _, _, err := t.ReadRTP(); err != nil {
				return
			}
			w.Packets.Add(1)
		}
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(offer)}); err != nil {
		pc.Close()
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	ans, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	g := webrtc.GatheringCompletePromise(pc)
	_ = pc.SetLocalDescription(ans)
	select {
	case <-g:
	case <-time.After(5 * time.Second):
	}
	w.mu.Lock()
	if w.pcs == nil {
		w.pcs = map[string]*webrtc.PeerConnection{}
	}
	w.n++
	loc := r.URL.Path + "/session" + string(rune('a'+w.n%26))
	w.pcs[loc] = pc
	w.mu.Unlock()
	rw.Header().Set("Content-Type", "application/sdp")
	rw.Header().Set("Location", loc)
	rw.WriteHeader(http.StatusCreated)
	_, _ = rw.Write([]byte(pc.LocalDescription().SDP))
}

// Close ends every session.
func (w *WHIPReceiver) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k, pc := range w.pcs {
		pc.Close()
		delete(w.pcs, k)
	}
}

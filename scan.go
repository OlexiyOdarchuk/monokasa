package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ScanServer is the entrance-side QR scanner: an HTML page with camera
// access on /scan and a POST /scan/check that validates and marks the
// ticket used.
type ScanServer struct {
	store *Store
	coder *Coder
	token string // empty disables auth (handy for local testing)
}

func NewScanServer(store *Store, coder *Coder, token string) *ScanServer {
	return &ScanServer{store: store, coder: coder, token: token}
}

func (s *ScanServer) Register(mux *http.ServeMux) {
	mux.HandleFunc("/scan", s.handlePage)
	mux.HandleFunc("/scan/check", s.handleCheck)
}

func (s *ScanServer) authOK(r *http.Request) bool {
	if s.token == "" {
		return true
	}
	got := r.URL.Query().Get("token")
	if got == "" {
		got = r.Header.Get("X-Scanner-Token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func (s *ScanServer) handlePage(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(scanHTML))
}

type checkRequest struct {
	Payload string `json:"payload"`
}

type checkResponse struct {
	Status string `json:"status"` // ok | used | invalid
	Detail string `json:"detail,omitempty"`
	UsedAt string `json:"usedAt,omitempty"`
	Seat   string `json:"seat,omitempty"`
}

func (s *ScanServer) handleCheck(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req checkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, checkResponse{Status: "invalid", Detail: "malformed body"})
		return
	}
	payload := strings.TrimSpace(req.Payload)
	if payload == "" {
		writeJSON(w, http.StatusBadRequest, checkResponse{Status: "invalid", Detail: "empty payload"})
		return
	}

	if _, _, err := s.coder.VerifyQRPayload(payload); err != nil {
		writeJSON(w, http.StatusOK, checkResponse{Status: "invalid", Detail: err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	t, err := s.store.UseTicket(ctx, payload)
	switch {
	case errors.Is(err, ErrTicketUsed):
		writeJSON(w, http.StatusOK, checkResponse{
			Status: "used",
			UsedAt: t.UsedAt.Format(time.RFC3339),
		})
		return
	case errors.Is(err, ErrTicketNotFound):
		writeJSON(w, http.StatusOK, checkResponse{Status: "invalid", Detail: "ticket not found"})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, checkResponse{Status: "invalid", Detail: err.Error()})
		return
	}

	// Pull seat info for a nicer UI.
	res, seat, _ := s.store.findReservationByTicket(ctx, t.ID)
	seatStr := ""
	if res.ID != 0 {
		seatStr = formatSeat(seat)
	}
	writeJSON(w, http.StatusOK, checkResponse{Status: "ok", Seat: seatStr})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func formatSeat(s Seat) string {
	return fmt.Sprintf("Ряд %d · місце %d", s.Row, s.Col)
}

const scanHTML = `<!doctype html>
<html lang="uk">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>Ticket scanner</title>
<style>
  :root { color-scheme: dark; }
  html,body { margin:0; height:100%; background:#111; color:#eee; font-family:system-ui,sans-serif; }
  #wrap { display:flex; flex-direction:column; height:100%; }
  video { width:100%; flex:1; object-fit:cover; background:#000; }
  canvas { display:none; }
  #status {
    flex: 0 0 30vh; display:flex; align-items:center; justify-content:center;
    text-align:center; padding:1rem; font-size:1.5rem; line-height:1.3;
    transition: background-color .15s ease;
  }
  #status .big { font-size:2.5rem; font-weight:700; letter-spacing:.04em; }
  #status .sub { font-size:1rem; opacity:.85; margin-top:.5rem; }
  .idle { background:#222; }
  .ok   { background:#1f7a1f; }
  .used { background:#a26a00; }
  .err  { background:#a32020; }
</style>
</head>
<body>
<div id="wrap">
  <video id="cam" playsinline autoplay muted></video>
  <canvas id="cnv"></canvas>
  <div id="status" class="idle">
    <div>
      <div class="big" id="big">наведи QR</div>
      <div class="sub" id="sub">камера активна</div>
    </div>
  </div>
</div>
<script src="https://cdn.jsdelivr.net/npm/jsqr@1.4.0/dist/jsQR.min.js"></script>
<script>
(async () => {
  const video = document.getElementById('cam');
  const canvas = document.getElementById('cnv');
  const ctx = canvas.getContext('2d', { willReadFrequently: true });
  const statusEl = document.getElementById('status');
  const big = document.getElementById('big');
  const sub = document.getElementById('sub');

  function setStatus(cls, b, s) {
    statusEl.className = cls;
    big.textContent = b;
    sub.textContent = s || '';
  }
  function vibrate(ms) { if (navigator.vibrate) navigator.vibrate(ms); }

  try {
    const stream = await navigator.mediaDevices.getUserMedia({
      video: { facingMode: { ideal: 'environment' } },
      audio: false,
    });
    video.srcObject = stream;
    await video.play();
  } catch (e) {
    setStatus('err', 'нема камери', e.message);
    return;
  }

  let lastPayload = '';
  let cooldownUntil = 0;
  const TOKEN = new URLSearchParams(location.search).get('token') || '';

  async function check(payload) {
    try {
      const res = await fetch('/scan/check' + (TOKEN ? '?token=' + encodeURIComponent(TOKEN) : ''), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ payload }),
      });
      const data = await res.json().catch(() => ({ status:'invalid', detail:'bad server response' }));
      if (data.status === 'ok') {
        setStatus('ok', '✓ OK', data.seat || '');
        vibrate(80);
      } else if (data.status === 'used') {
        setStatus('used', '⏱ Уже використано', data.usedAt || '');
        vibrate([60, 80, 60]);
      } else {
        setStatus('err', '✗ Недійсний', data.detail || '');
        vibrate([120, 60, 120]);
      }
    } catch (e) {
      setStatus('err', '✗ Помилка мережі', e.message);
    }
  }

  function tick() {
    if (video.readyState === video.HAVE_ENOUGH_DATA) {
      canvas.width = video.videoWidth;
      canvas.height = video.videoHeight;
      ctx.drawImage(video, 0, 0, canvas.width, canvas.height);
      const img = ctx.getImageData(0, 0, canvas.width, canvas.height);
      const code = jsQR(img.data, img.width, img.height, { inversionAttempts: 'attemptBoth' });
      const now = Date.now();
      if (code && code.data && now >= cooldownUntil && code.data !== lastPayload) {
        lastPayload = code.data;
        cooldownUntil = now + 2000;
        sub.textContent = 'перевіряю…';
        check(code.data).finally(() => {
          setTimeout(() => { lastPayload = ''; setStatus('idle', 'наведи QR', 'готов'); }, 1500);
        });
      }
    }
    requestAnimationFrame(tick);
  }
  requestAnimationFrame(tick);
})();
</script>
</body>
</html>
`

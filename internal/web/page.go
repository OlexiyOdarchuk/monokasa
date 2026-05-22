package web

// loginHTML is the password gate shown at GET /scan when SCANNER_TOKEN is
// set and the request has no valid cookie. The {{ERR}} placeholder is
// replaced server-side with either "" or a short error div — content is
// fixed text from us, so no XSS escaping is needed.
const loginHTML = `<!doctype html>
<html lang="uk">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Сканер — вхід</title>
<style>
  :root { color-scheme: dark; }
  html,body { margin:0; height:100%; background:#111; color:#eee;
    font-family:system-ui,sans-serif; display:flex; align-items:center; justify-content:center; }
  form { background:#1a1a1a; padding:1.5rem 1.5rem 1.2rem; border-radius:12px;
    min-width:280px; box-shadow:0 4px 24px rgba(0,0,0,.4); }
  h1 { margin:0 0 1rem; font-size:1.15rem; font-weight:600; letter-spacing:.02em; }
  input[type=password] { width:100%; box-sizing:border-box; padding:.7rem;
    font-size:1rem; background:#222; color:#eee; border:1px solid #333;
    border-radius:6px; outline:none; }
  input[type=password]:focus { border-color:#2563eb; }
  button { width:100%; margin-top:.7rem; padding:.7rem; font-size:1rem;
    background:#2563eb; color:#fff; border:0; border-radius:6px; font-weight:600; cursor:pointer; }
  button:active { background:#1d4ed8; }
  .err { color:#f87171; margin-top:.6rem; font-size:.9rem; text-align:center; }
</style>
</head>
<body>
<form method="POST" action="/scan" autocomplete="off">
  <h1>🎟  Сканер квитків</h1>
  <input type="password" name="password" autofocus required placeholder="пароль">
  <button type="submit">Увійти</button>
  {{ERR}}
</form>
</body>
</html>
`

const pageHTML = `<!doctype html>
<html lang="uk">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>Сканер квитків</title>
<style>
  :root { color-scheme: dark; }
  html,body { margin:0; height:100%; background:#111; color:#eee; font-family:system-ui,sans-serif; }
  #wrap { display:flex; flex-direction:column; height:100%; }
  video { width:100%; flex:1; object-fit:cover; background:#000; }
  canvas { display:none; }
  #status {
    flex: 0 0 32vh; display:flex; align-items:center; justify-content:center;
    text-align:center; padding:1rem; transition: background-color .15s ease;
  }
  #status .big { font-size:2.2rem; font-weight:700; letter-spacing:.04em; line-height:1.1; }
  #status .who { font-size:1.4rem; margin-top:.4rem; }
  #status .sub { font-size:.95rem; opacity:.85; margin-top:.35rem; }
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
      <div class="who" id="who"></div>
      <div class="sub" id="sub">камера активна</div>
    </div>
  </div>
</div>
<script src="https://cdn.jsdelivr.net/npm/jsqr@1.4.0/dist/jsQR.min.js"></script>
<!-- Telegram WebApp SDK: harmless no-op outside Telegram context.
     Used so window.Telegram.WebApp.initData can authenticate the
     scanner without the shared password when admin opens it via the
     bot's "🔍 Сканер" inline button. -->
<script src="https://telegram.org/js/telegram-web-app.js"></script>
<script>
(async () => {
  const video = document.getElementById('cam');
  const canvas = document.getElementById('cnv');
  const ctx = canvas.getContext('2d', { willReadFrequently: true });
  const statusEl = document.getElementById('status');
  const big = document.getElementById('big');
  const who = document.getElementById('who');
  const sub = document.getElementById('sub');

  // Web Audio for short beeps — different timbre for ok / used / invalid.
  const AudioCtx = window.AudioContext || window.webkitAudioContext;
  const actx = AudioCtx ? new AudioCtx() : null;
  function beep(freq, durMs, type) {
    if (!actx) return;
    if (actx.state === 'suspended') actx.resume();
    const o = actx.createOscillator();
    const g = actx.createGain();
    o.type = type || 'sine';
    o.frequency.value = freq;
    g.gain.setValueAtTime(0.001, actx.currentTime);
    g.gain.exponentialRampToValueAtTime(0.25, actx.currentTime + 0.01);
    g.gain.exponentialRampToValueAtTime(0.001, actx.currentTime + durMs/1000);
    o.connect(g).connect(actx.destination);
    o.start();
    o.stop(actx.currentTime + durMs/1000);
  }
  function chord(freqs, durMs, type) { freqs.forEach(f => beep(f, durMs, type)); }
  function vibrate(p) { if (navigator.vibrate) navigator.vibrate(p); }

  function setStatus(cls, b, person, s) {
    statusEl.className = cls;
    big.textContent = b;
    who.textContent = person || '';
    sub.textContent = s || '';
  }

  try {
    const stream = await navigator.mediaDevices.getUserMedia({
      video: { facingMode: { ideal: 'environment' } },
      audio: false,
    });
    video.srcObject = stream;
    await video.play();
  } catch (e) {
    setStatus('err', 'нема камери', '', e.message);
    return;
  }

  let lastPayload = '';
  let cooldownUntil = 0;

  // Telegram WebApp integration: when this page is opened as a Mini
  // App from the bot, window.Telegram.WebApp.initData carries the
  // signed payload our server verifies as a second auth path. The
  // SDK script is added by Telegram automatically when launched as a
  // WebApp; outside that context window.Telegram is undefined and
  // we just fall through to the cookie/password flow.
  const tgInitData = (window.Telegram && window.Telegram.WebApp && window.Telegram.WebApp.initData) || '';
  if (window.Telegram && window.Telegram.WebApp && window.Telegram.WebApp.ready) {
    window.Telegram.WebApp.ready();
    window.Telegram.WebApp.expand && window.Telegram.WebApp.expand();
  }

  async function check(payload) {
    try {
      const headers = { 'Content-Type': 'application/json' };
      if (tgInitData) headers['X-Telegram-Init-Data'] = tgInitData;
      const res = await fetch('/scan/check', {
        method: 'POST',
        headers,
        body: JSON.stringify({ payload }),
        credentials: 'same-origin',
      });
      const data = await res.json().catch(() => ({ status:'invalid', detail:'bad server response' }));
      const seat = data.seat || '';
      const person = data.buyer ? (data.buyer + (seat ? ' · ' + seat : '')) : seat;
      if (data.status === 'ok') {
        setStatus('ok', '✓ ПРОХОДЬ', person, data.bookedAt ? ('куплено: ' + data.bookedAt) : '');
        chord([880, 1320], 140, 'sine');
        vibrate(80);
      } else if (data.status === 'used') {
        const sub = [
          data.bookedAt ? 'куплено: ' + data.bookedAt : '',
          data.usedAt ? 'пройшов: ' + data.usedAt : '',
        ].filter(Boolean).join(' · ');
        setStatus('used', '⏱ Вже використано', person, sub);
        beep(440, 220, 'square');
        vibrate([60, 80, 60]);
      } else {
        setStatus('err', '✗ Недійсний', '', data.detail || '');
        chord([220, 196], 260, 'sawtooth');
        vibrate([120, 60, 120]);
      }
    } catch (e) {
      setStatus('err', '✗ Помилка мережі', '', e.message);
      beep(180, 300, 'sawtooth');
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
        cooldownUntil = now + 2200;
        sub.textContent = 'перевіряю…';
        check(code.data).finally(() => {
          setTimeout(() => { lastPayload = ''; setStatus('idle', 'наведи QR', '', 'готов'); }, 1800);
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

package server

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"log/slog"
)

// ─── Middleware ───────────────────────────────────────────────────────────────

// adminAuth enforces HTTP Basic Auth using WEB_ADMIN as the password.
// Username is ignored — any value is accepted.
func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if !ok || pass != s.cfg.WebAdminPassword {
			w.Header().Set("WWW-Authenticate", `Basic realm="klistr admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ─── Handlers ─────────────────────────────────────────────────────────────────

func (s *Server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, adminHTML)
}

func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	type statusResponse struct {
		Domain        string   `json:"domain"`
		Username      string   `json:"username"`
		Npub          string   `json:"npub"`
		PubkeyShort   string   `json:"pubkey_short"`
		BskyEnabled   bool     `json:"bsky_enabled"`
		BskyIdentifier string  `json:"bsky_identifier,omitempty"`
		Relays        []string `json:"relays"`
		Version       string   `json:"version"`
	}

	resp := statusResponse{
		Domain:      s.cfg.LocalDomain,
		Username:    s.cfg.NostrUsername,
		Npub:        s.cfg.NostrNpub,
		PubkeyShort: s.cfg.NostrPublicKey[:8] + "…",
		BskyEnabled: s.cfg.BskyEnabled(),
		Relays:      s.cfg.NostrRelays,
		Version:     version,
	}
	if s.cfg.BskyEnabled() {
		resp.BskyIdentifier = s.cfg.BskyIdentifier
	}
	jsonResponse(w, resp, http.StatusOK)
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)
	stats, err := s.store.Stats(localActorURL)
	if err != nil {
		slog.Error("admin stats query failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]int{
		"followers":  stats.FollowerCount,
		"objects":    stats.ObjectCount,
		"actor_keys": stats.ActorKeyCount,
	}, http.StatusOK)
}

func (s *Server) handleAdminFollowers(w http.ResponseWriter, r *http.Request) {
	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)
	followers, err := s.store.GetFollowers(localActorURL)
	if err != nil {
		slog.Error("admin followers query failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if followers == nil {
		followers = []string{}
	}
	jsonResponse(w, map[string]interface{}{
		"items": followers,
		"total": len(followers),
	}, http.StatusOK)
}

func (s *Server) handleAdminSyncBsky(w http.ResponseWriter, r *http.Request) {
	if s.bskyTrigger == nil {
		jsonResponse(w, map[string]string{"message": "Bluesky bridge is not configured."}, http.StatusOK)
		return
	}
	select {
	case s.bskyTrigger <- struct{}{}:
		jsonResponse(w, map[string]string{"message": "Bluesky sync triggered."}, http.StatusOK)
	default:
		// Channel full — a sync is already queued.
		jsonResponse(w, map[string]string{"message": "Sync already queued."}, http.StatusOK)
	}
}

func (s *Server) handleAdminLogStream(w http.ResponseWriter, r *http.Request) {
	if s.logBroadcaster == nil {
		http.Error(w, "log streaming not available", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Disable the server write deadline for this long-lived SSE connection.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		slog.Debug("admin log stream: could not clear write deadline", "error", err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

	history, ch, cancel := s.logBroadcaster.Subscribe()
	defer cancel()

	// Send ring-buffer history so the client sees recent context immediately.
	for _, line := range history {
		fmt.Fprintf(w, "data: %s\n\n", jsonEscapeSSE(line))
	}
	flusher.Flush()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case line, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", jsonEscapeSSE(line))
			flusher.Flush()
		}
	}
}

// jsonEscapeSSE ensures newlines inside JSON don't break the SSE framing.
func jsonEscapeSSE(s string) string {
	// SSE data fields cannot contain raw newlines — replace with space.
	return strings.ReplaceAll(s, "\n", " ")
}

// ─── HTML template ────────────────────────────────────────────────────────────

const adminHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>klistr admin</title>
<style>
:root {
  --bg: #0d1117; --surface: #161b22; --surface2: #1c2128;
  --border: #30363d; --text: #e6edf3; --muted: #8b949e;
  --green: #3fb950; --blue: #58a6ff; --yellow: #d29922; --red: #f85149;
  --purple: #a371f7;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
body { background: var(--bg); color: var(--text); font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; font-size: 14px; line-height: 1.5; }
.layout { max-width: 960px; margin: 0 auto; padding: 24px 16px; }
.header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 28px; padding-bottom: 16px; border-bottom: 1px solid var(--border); }
.header h1 { font-size: 18px; font-weight: 600; display: flex; align-items: center; gap: 10px; }
.header h1 span { font-size: 11px; font-weight: 400; color: var(--muted); background: var(--surface2); border: 1px solid var(--border); border-radius: 4px; padding: 2px 8px; }
.header a { color: var(--muted); text-decoration: none; font-size: 13px; }
.header a:hover { color: var(--text); }
.grid { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; margin-bottom: 16px; }
@media (max-width: 640px) { .grid { grid-template-columns: 1fr; } }
.card { background: var(--surface); border: 1px solid var(--border); border-radius: 10px; padding: 20px; }
.card-full { background: var(--surface); border: 1px solid var(--border); border-radius: 10px; padding: 20px; margin-bottom: 16px; }
.card h2 { font-size: 11px; font-weight: 600; color: var(--muted); text-transform: uppercase; letter-spacing: 0.06em; margin-bottom: 14px; }
.kv { display: grid; grid-template-columns: 130px 1fr; gap: 5px 12px; align-items: start; }
.kv .k { color: var(--muted); white-space: nowrap; padding-top: 1px; }
.kv .v { color: var(--text); font-family: 'SF Mono', Consolas, monospace; font-size: 12px; word-break: break-all; }
.badge { display: inline-flex; align-items: center; gap: 5px; padding: 2px 9px; border-radius: 20px; font-size: 11px; font-weight: 500; font-family: inherit; }
.badge::before { content: ''; display: inline-block; width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
.badge-green { background: rgba(63,185,80,.15); color: var(--green); }
.badge-muted  { background: rgba(139,148,158,.12); color: var(--muted); }
.stats-row { display: flex; gap: 0; }
.stat { flex: 1; text-align: center; padding: 12px 8px; border-right: 1px solid var(--border); }
.stat:last-child { border-right: none; }
.stat .num { font-size: 32px; font-weight: 700; color: var(--blue); line-height: 1; }
.stat .lbl { color: var(--muted); font-size: 11px; margin-top: 6px; }
.actions { display: flex; flex-wrap: wrap; gap: 10px; align-items: center; }
.btn { display: inline-flex; align-items: center; gap: 6px; padding: 8px 16px; border-radius: 6px; border: none; cursor: pointer; font-size: 13px; font-weight: 500; transition: opacity .15s, transform .1s; }
.btn:active { transform: scale(.97); }
.btn:hover:not(:disabled) { opacity: .85; }
.btn-blue { background: var(--blue); color: #fff; }
.btn-surface { background: var(--surface2); color: var(--text); border: 1px solid var(--border); }
.btn:disabled { opacity: .4; cursor: not-allowed; }
.action-msg { font-size: 12px; color: var(--muted); margin-top: 10px; min-height: 18px; }
#log-wrap { position: relative; }
#log { background: #010409; border: 1px solid var(--border); border-radius: 6px; height: 420px; overflow-y: auto; padding: 10px 14px; font-family: 'JetBrains Mono', 'Fira Code', 'SF Mono', Consolas, monospace; font-size: 12px; line-height: 1.65; scroll-behavior: smooth; }
.ll { white-space: pre-wrap; word-break: break-all; }
.ll.INFO  { color: #c9d1d9; }
.ll.DEBUG { color: #484f58; }
.ll.WARN  { color: var(--yellow); }
.ll.ERROR { color: var(--red); }
.log-indicator { position: absolute; top: -8px; right: 8px; font-size: 11px; padding: 1px 8px; border-radius: 10px; background: var(--surface); border: 1px solid var(--border); }
.dot { display: inline-block; width: 7px; height: 7px; border-radius: 50%; margin-right: 4px; background: var(--muted); }
.dot.live { background: var(--green); animation: pulse 2s infinite; }
@keyframes pulse { 0%,100%{opacity:1} 50%{opacity:.4} }
.relays-list { display: flex; flex-wrap: wrap; gap: 6px; margin-top: 2px; }
.relay-tag { background: var(--surface2); border: 1px solid var(--border); border-radius: 4px; padding: 2px 8px; font-size: 11px; font-family: monospace; color: var(--muted); }
.followers-list { display: flex; flex-direction: column; gap: 4px; max-height: 220px; overflow-y: auto; margin-top: 4px; }
.follower { font-size: 12px; font-family: monospace; color: var(--text); padding: 4px 8px; background: var(--surface2); border-radius: 4px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.follower a { color: var(--blue); text-decoration: none; }
.follower a:hover { text-decoration: underline; }
.empty { color: var(--muted); font-size: 12px; font-style: italic; }
.toast { position: fixed; bottom: 20px; right: 20px; background: var(--surface); border: 1px solid var(--border); border-radius: 8px; padding: 10px 16px; font-size: 13px; opacity: 0; pointer-events: none; transition: opacity .3s; z-index: 999; }
.toast.show { opacity: 1; pointer-events: auto; }
</style>
</head>
<body>
<div class="layout">

<div class="header">
  <h1>klistr admin <span id="hdr-version">—</span></h1>
  <a href="/">← public root</a>
</div>

<div class="grid">
  <!-- Status card -->
  <div class="card">
    <h2>Node</h2>
    <div class="kv" id="status-kv">
      <span class="k">loading…</span><span class="v"></span>
    </div>
  </div>

  <!-- Stats card -->
  <div class="card">
    <h2>Stats</h2>
    <div class="stats-row">
      <div class="stat"><div class="num" id="s-followers">—</div><div class="lbl">Fediverse Followers</div></div>
      <div class="stat"><div class="num" id="s-objects">—</div><div class="lbl">Bridged Objects</div></div>
      <div class="stat"><div class="num" id="s-actors">—</div><div class="lbl">Known AP Actors</div></div>
    </div>
  </div>
</div>

<!-- Followers -->
<div class="card-full">
  <h2>Fediverse Followers</h2>
  <div class="followers-list" id="followers-list"><span class="empty">loading…</span></div>
</div>

<!-- Actions -->
<div class="card-full">
  <h2>Actions</h2>
  <div class="actions">
    <button class="btn btn-blue" id="btn-bsky-sync" onclick="syncBsky()">
      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>
      Force Bluesky Sync
    </button>
    <button class="btn btn-surface" onclick="refreshAll()">
      <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>
      Refresh
    </button>
  </div>
  <div class="action-msg" id="action-msg"></div>
</div>

<!-- Relays -->
<div class="card-full">
  <h2>Configured Relays</h2>
  <div class="relays-list" id="relays-list"><span class="empty">loading…</span></div>
</div>

<!-- Live Log -->
<div class="card-full">
  <h2>Live Log</h2>
  <div id="log-wrap">
    <div class="log-indicator"><span class="dot" id="dot"></span><span id="log-status">connecting</span></div>
    <div id="log"></div>
  </div>
</div>

</div><!-- /layout -->
<div class="toast" id="toast"></div>

<script>
const MAX_LINES = 600;
let lineCount = 0;
let autoScroll = true;
let bskyEnabled = false;

// ── Toast ──────────────────────────────────────────────────────────────────
function toast(msg) {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.classList.add('show');
  setTimeout(() => el.classList.remove('show'), 3000);
}

// ── Log ───────────────────────────────────────────────────────────────────
const logEl = document.getElementById('log');
logEl.addEventListener('scroll', () => {
  autoScroll = logEl.scrollTop + logEl.clientHeight >= logEl.scrollHeight - 30;
});

function appendLog(raw) {
  const div = document.createElement('div');
  div.className = 'll';
  try {
    const j = JSON.parse(raw);
    const lvl = (j.level || 'INFO').toUpperCase();
    div.classList.add(lvl);
    const ts  = j.time ? new Date(j.time).toLocaleTimeString([], {hour:'2-digit',minute:'2-digit',second:'2-digit'}) : '';
    const msg = j.msg || '';
    const extras = Object.entries(j)
      .filter(([k]) => !['time','level','msg'].includes(k))
      .map(([k,v]) => k + '=' + (typeof v === 'string' ? v : JSON.stringify(v)))
      .join('  ');
    div.textContent = [ts, lvl.padEnd(5), msg, extras].filter(Boolean).join('  ');
  } catch {
    div.textContent = raw;
  }
  logEl.appendChild(div);
  lineCount++;
  if (lineCount > MAX_LINES) { logEl.removeChild(logEl.firstChild); lineCount--; }
  if (autoScroll) logEl.scrollTop = logEl.scrollHeight;
}

function connectLog() {
  const dot = document.getElementById('dot');
  const status = document.getElementById('log-status');
  const src = new EventSource('/web/log/stream');
  src.onopen = () => { dot.className = 'dot live'; status.textContent = 'live'; };
  src.onmessage = e => appendLog(e.data);
  src.onerror = () => {
    dot.className = 'dot'; status.textContent = 'reconnecting…';
    src.close(); setTimeout(connectLog, 5000);
  };
}

// ── Status ────────────────────────────────────────────────────────────────
async function loadStatus() {
  const r = await fetch('/web/api/status');
  const d = await r.json();
  bskyEnabled = d.bsky_enabled;

  document.getElementById('hdr-version').textContent = 'v' + d.version;

  const kv = document.getElementById('status-kv');
  kv.innerHTML = '';
  function row(k, v, html) {
    const ke = document.createElement('span'); ke.className = 'k'; ke.textContent = k;
    const ve = document.createElement('span'); ve.className = 'v';
    if (html) ve.innerHTML = v; else ve.textContent = v;
    kv.appendChild(ke); kv.appendChild(ve);
  }
  row('Domain',   d.domain);
  row('Username', '@' + d.username);
  row('Npub',     d.npub);
  row('Pubkey',   d.pubkey_short);
  row('Bluesky',  d.bsky_enabled
    ? '<span class="badge badge-green">enabled — ' + d.bsky_identifier + '</span>'
    : '<span class="badge badge-muted">disabled</span>', true);

  // Relay tags
  const rl = document.getElementById('relays-list');
  rl.innerHTML = '';
  (d.relays || []).forEach(r => {
    const t = document.createElement('span'); t.className = 'relay-tag'; t.textContent = r;
    rl.appendChild(t);
  });

  if (!d.bsky_enabled) {
    const btn = document.getElementById('btn-bsky-sync');
    btn.disabled = true;
    btn.title = 'Bluesky bridge not configured';
  }
}

// ── Stats ─────────────────────────────────────────────────────────────────
async function loadStats() {
  const r = await fetch('/web/api/stats');
  const d = await r.json();
  document.getElementById('s-followers').textContent = d.followers;
  document.getElementById('s-objects').textContent   = d.objects;
  document.getElementById('s-actors').textContent    = d.actor_keys;
}

// ── Followers ─────────────────────────────────────────────────────────────
async function loadFollowers() {
  const r = await fetch('/web/api/followers');
  const d = await r.json();
  const el = document.getElementById('followers-list');
  el.innerHTML = '';
  if (!d.items || d.items.length === 0) {
    el.innerHTML = '<span class="empty">No Fediverse followers yet.</span>';
    return;
  }
  d.items.forEach(url => {
    const div = document.createElement('div');
    div.className = 'follower';
    div.innerHTML = '<a href="' + url + '" target="_blank" rel="noopener">' + url + '</a>';
    el.appendChild(div);
  });
}

// ── Actions ───────────────────────────────────────────────────────────────
async function syncBsky() {
  const btn = document.getElementById('btn-bsky-sync');
  btn.disabled = true;
  btn.textContent = 'Triggering…';
  try {
    const r = await fetch('/web/api/sync-bsky', { method: 'POST' });
    const d = await r.json();
    document.getElementById('action-msg').textContent = d.message;
    toast(d.message);
  } catch (e) {
    document.getElementById('action-msg').textContent = 'Error: ' + e.message;
  } finally {
    btn.disabled = !bskyEnabled;
    btn.innerHTML = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg> Force Bluesky Sync';
  }
}

function refreshAll() {
  loadStats();
  loadFollowers();
  toast('Refreshed');
}

// ── Init ──────────────────────────────────────────────────────────────────
Promise.all([loadStatus(), loadStats(), loadFollowers()])
  .catch(e => console.error('init failed', e));

setInterval(loadStats, 30000);

connectLog();
</script>
</body>
</html>`

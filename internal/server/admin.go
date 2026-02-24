package server

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// ─── Middleware ───────────────────────────────────────────────────────────────

// adminAuth enforces HTTP Basic Auth using WEB_ADMIN as the password.
// Username is ignored — any value is accepted.
func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.WebAdminPassword)) != 1 {
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
		Domain         string   `json:"domain"`
		Username       string   `json:"username"`
		Npub           string   `json:"npub"`
		PubkeyShort    string   `json:"pubkey_short"`
		BskyEnabled    bool     `json:"bsky_enabled"`
		BskyIdentifier string   `json:"bsky_identifier,omitempty"`
		Relays         []string `json:"relays"`
		Version        string   `json:"version"`
		StartedAt      int64    `json:"started_at"` // unix timestamp
	}

	resp := statusResponse{
		Domain:      s.cfg.LocalDomain,
		Username:    s.cfg.NostrUsername,
		Npub:        s.cfg.NostrNpub,
		PubkeyShort: s.cfg.NostrPublicKey[:8] + "…",
		BskyEnabled: s.cfg.BskyEnabled(),
		Relays:      s.cfg.NostrRelays,
		Version:     version,
		StartedAt:   s.startedAt.Unix(),
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
	jsonResponse(w, map[string]interface{}{
		"bsky_enabled":        s.cfg.BskyEnabled(),
		"fediverse_followers": stats.FollowerCount,
		"bsky_followers":      stats.BskyFollowerCount,
		"fediverse_actors":    stats.ActorKeyCount,
		"fediverse_objects":   stats.FediverseObjects,
		"bsky_objects":        stats.BskyObjects,
		"bsky_last_seen":      stats.BskyLastSeen,
		"bsky_last_poll":      stats.BskyLastPoll,
		"total_objects":       stats.TotalObjects,
		"last_resync_at":      stats.LastResyncAt,
		"last_resync_count":   stats.LastResyncCount,
	}, http.StatusOK)
}

func (s *Server) handleAdminResyncAccounts(w http.ResponseWriter, r *http.Request) {
	if s.resyncTrigger == nil {
		jsonResponse(w, map[string]string{"message": "Account resync is not available."}, http.StatusOK)
		return
	}
	select {
	case s.resyncTrigger <- struct{}{}:
		jsonResponse(w, map[string]string{"message": "Account resync triggered — profiles will refresh in the background."}, http.StatusOK)
	default:
		jsonResponse(w, map[string]string{"message": "Resync already queued."}, http.StatusOK)
	}
}

type followerEntry struct {
	URL    string `json:"url"`
	Handle string `json:"handle"`
}

func (s *Server) handleAdminFollowers(w http.ResponseWriter, r *http.Request) {
	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)

	apFollowers, err := s.store.GetAPFollowers(localActorURL)
	if err != nil {
		slog.Error("admin ap followers query failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	bskyFollowerIDs, err := s.store.GetBskyFollowers(localActorURL)
	if err != nil {
		slog.Error("admin bsky followers query failed", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Build response slices.
	fedItems := make([]followerEntry, 0, len(apFollowers))
	for _, url := range apFollowers {
		fedItems = append(fedItems, followerEntry{URL: url})
	}

	bskyItems := make([]followerEntry, 0, len(bskyFollowerIDs))
	for _, id := range bskyFollowerIDs {
		did := strings.TrimPrefix(id, "bsky:")
		handle, _ := s.store.GetKV("bsky_follower_handle_" + did)
		if handle == "" {
			handle = did
		}
		bskyItems = append(bskyItems, followerEntry{
			URL:    "https://bsky.app/profile/" + handle,
			Handle: handle,
		})
	}

	jsonResponse(w, map[string]interface{}{
		"fediverse":       fedItems,
		"bluesky":         bskyItems,
		"total_fediverse": len(fedItems),
		"total_bluesky":   len(bskyItems),
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

// handleAdminLogSnapshot returns the current ring-buffer contents as a JSON
// array of raw log lines. The client refreshes on demand instead of streaming.
func (s *Server) handleAdminLogSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.logBroadcaster == nil {
		jsonResponse(w, []string{}, http.StatusOK)
		return
	}
	lines := s.logBroadcaster.Lines()
	if lines == nil {
		lines = []string{}
	}
	jsonResponse(w, lines, http.StatusOK)
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
  --bg:#0d1117; --surface:#161b22; --surface2:#1c2128;
  --border:#30363d; --text:#e6edf3; --muted:#8b949e;
  --green:#3fb950; --blue:#58a6ff; --yellow:#d29922; --red:#f85149;
  --purple:#a371f7; --sky:#38bdf8;
}
*{box-sizing:border-box;margin:0;padding:0}
body{background:var(--bg);color:var(--text);font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;font-size:14px;line-height:1.5}
.layout{max-width:1060px;margin:0 auto;padding:24px 16px}

/* Header */
.header{display:flex;justify-content:space-between;align-items:center;margin-bottom:28px;padding-bottom:16px;border-bottom:1px solid var(--border)}
.header h1{font-size:18px;font-weight:600;display:flex;align-items:center;gap:10px}
.vtag{font-size:11px;font-weight:400;color:var(--muted);background:var(--surface2);border:1px solid var(--border);border-radius:4px;padding:2px 8px}
.header-right{display:flex;align-items:center;gap:16px}
.uptime-badge{font-size:11px;color:var(--muted)}
.header a{color:var(--muted);text-decoration:none;font-size:13px}
.header a:hover{color:var(--text)}

/* Grids */
.grid2{display:grid;grid-template-columns:1fr 1fr;gap:16px;margin-bottom:16px}
@media(max-width:640px){.grid2{grid-template-columns:1fr}}

/* Cards */
.card{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:20px}
.card-full{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:20px;margin-bottom:16px}
.card h2,.card-full h2{font-size:11px;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:.06em;margin-bottom:14px}

/* Key-value */
.kv{display:grid;grid-template-columns:110px 1fr;gap:5px 12px;align-items:start}
.kv .k{color:var(--muted);white-space:nowrap;padding-top:2px;font-size:13px}
.kv .v{color:var(--text);font-family:'SF Mono',Consolas,monospace;font-size:12px;word-break:break-all;display:flex;align-items:center;gap:6px}

/* Badges */
.badge{display:inline-flex;align-items:center;gap:5px;padding:2px 9px;border-radius:20px;font-size:11px;font-weight:500}
.badge::before{content:'';display:inline-block;width:6px;height:6px;border-radius:50%;background:currentColor}
.badge-green{background:rgba(63,185,80,.15);color:var(--green)}
.badge-muted{background:rgba(139,148,158,.12);color:var(--muted)}

/* Copy button */
.copy-btn{background:none;border:none;cursor:pointer;color:var(--muted);padding:0 2px;display:inline-flex;align-items:center;opacity:.6;transition:opacity .15s}
.copy-btn:hover{opacity:1;color:var(--blue)}

/* Quick links */
.quick-links{display:flex;gap:12px;flex-wrap:wrap;margin-top:12px;padding-top:12px;border-top:1px solid var(--border)}
.qlink{font-size:11px;color:var(--blue);text-decoration:none;display:flex;align-items:center;gap:4px}
.qlink:hover{text-decoration:underline}

/* Relay management */
.relay-dot{width:7px;height:7px;border-radius:50%;flex-shrink:0}
.relay-row{display:flex;align-items:center;gap:7px;padding:5px 8px;background:var(--surface2);border-radius:5px;margin-bottom:3px}
.relay-url{font-family:monospace;font-size:11px;flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.relay-cb{font-size:10px;white-space:nowrap;padding:1px 6px;border-radius:3px;font-weight:500}
.relay-cb-ok{background:rgba(63,185,80,.12);color:var(--green)}
.relay-cb-warn{background:rgba(210,153,34,.12);color:var(--yellow)}
.relay-cb-open{background:rgba(248,81,73,.12);color:var(--red)}
.relay-acts{display:flex;gap:4px;flex-shrink:0}
.rbtn{background:none;border:1px solid var(--border);border-radius:3px;padding:2px 7px;font-size:10px;cursor:pointer;color:var(--muted);font-family:inherit;transition:color .12s,border-color .12s}
.rbtn:hover{color:var(--text);border-color:var(--muted)}
.rbtn-red:hover{color:var(--red);border-color:var(--red)}
.rbtn-blue:hover{color:var(--blue);border-color:var(--blue)}

/* Bridge panels */
.bridge-grid{display:grid;grid-template-columns:repeat(4,1fr);gap:12px}
@media(max-width:720px){.bridge-grid{grid-template-columns:repeat(2,1fr)}}
@media(max-width:380px){.bridge-grid{grid-template-columns:1fr}}
.bp{border:1px solid var(--border);border-radius:8px;overflow:hidden;border-left:3px solid transparent}
.bp-nostr{border-left-color:var(--purple)}
.bp-fediverse{border-left-color:var(--blue)}
.bp-bsky{border-left-color:var(--sky)}
.bp-total{border-left-color:var(--green)}
.bp-header{padding:10px 14px 8px;font-size:11px;font-weight:600;letter-spacing:.05em;text-transform:uppercase;border-bottom:1px solid var(--border);display:flex;align-items:center;gap:7px}
.bp-nostr .bp-header{color:var(--purple)}
.bp-fediverse .bp-header{color:var(--blue)}
.bp-bsky .bp-header{color:var(--sky)}
.bp-total .bp-header{color:var(--green)}
.bp-body{padding:12px 14px;display:flex;flex-direction:column;gap:8px}
.bp-row{display:flex;justify-content:space-between;align-items:baseline;gap:8px}
.bpl{color:var(--muted);font-size:12px;white-space:nowrap}
.bpv{font-weight:600;font-size:14px;color:var(--text);font-family:'SF Mono',Consolas,monospace;text-align:right}
.bpv.big{font-size:24px;font-family:inherit}
.bpv.sm{font-size:11px}

/* Followers */
.followers-list{display:flex;flex-direction:column;gap:4px;max-height:260px;overflow-y:auto}
.follower{font-size:12px;padding:5px 10px;background:var(--surface2);border-radius:5px;display:flex;align-items:center;justify-content:space-between;gap:8px}
.f-handle{font-family:monospace;color:var(--text);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.follower a{color:var(--blue);text-decoration:none;font-size:11px;flex-shrink:0;opacity:.7}
.follower a:hover{opacity:1;text-decoration:underline}
.empty{color:var(--muted);font-size:12px;font-style:italic}

/* Actions */
.actions{display:flex;flex-wrap:wrap;gap:10px;align-items:center}
.btn{display:inline-flex;align-items:center;gap:6px;padding:8px 16px;border-radius:6px;border:none;cursor:pointer;font-size:13px;font-weight:500;transition:opacity .15s,transform .1s;font-family:inherit}
.btn:active{transform:scale(.97)}
.btn:hover:not(:disabled){opacity:.85}
.btn-blue{background:var(--blue);color:#000}
.btn-surface{background:var(--surface2);color:var(--text);border:1px solid var(--border)}
.btn-red{background:var(--surface2);color:var(--red);border:1px solid var(--border)}
.btn:disabled{opacity:.4;cursor:not-allowed}
.action-msg{font-size:12px;color:var(--muted);margin-top:10px;min-height:18px}

/* Log */
.log-toolbar{display:flex;align-items:center;gap:6px;margin-bottom:10px;flex-wrap:wrap}
.lfb{padding:4px 11px;border-radius:4px;border:1px solid var(--border);background:var(--surface2);color:var(--muted);cursor:pointer;font-size:11px;font-weight:500;font-family:inherit;transition:all .15s}
.lfb.active{color:var(--text);border-color:var(--muted)}
.lfb[data-level=INFO].active{border-color:var(--blue);color:var(--blue)}
.lfb[data-level=WARN].active{border-color:var(--yellow);color:var(--yellow)}
.lfb[data-level=ERROR].active{border-color:var(--red);color:var(--red)}
.lfb[data-level=DEBUG].active{border-color:var(--purple);color:var(--purple)}
.log-sep{width:1px;height:18px;background:var(--border);margin:0 2px}
#log{background:#010409;border:1px solid var(--border);border-radius:6px;height:440px;overflow-y:auto;padding:10px 14px;font-family:'JetBrains Mono','Fira Code','SF Mono',Consolas,monospace;font-size:12px;line-height:1.65;scroll-behavior:smooth}
.ll{white-space:pre-wrap;word-break:break-all}
.ll.INFO{color:#c9d1d9}
.ll.DEBUG{color:#484f58}
.ll.WARN{color:var(--yellow)}
.ll.ERROR{color:var(--red)}

/* Toast */
.toast{position:fixed;bottom:20px;right:20px;background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:10px 16px;font-size:13px;opacity:0;pointer-events:none;transition:opacity .3s;z-index:999}
.toast.show{opacity:1;pointer-events:auto}
</style>
</head>
<body>
<div class="layout">

<!-- Header -->
<div class="header">
  <h1>klistr <span class="vtag" id="hdr-version">…</span></h1>
  <div class="header-right">
    <span class="uptime-badge">up <span id="hdr-uptime">—</span></span>
    <a href="/">← public root</a>
  </div>
</div>

<!-- Row 1: Node + Relays -->
<div class="grid2">
  <div class="card">
    <h2>Node</h2>
    <div class="kv" id="status-kv">
      <span class="k">loading…</span><span class="v"></span>
    </div>
    <div class="quick-links" id="quick-links" style="display:none">
      <a class="qlink" id="ql-actor" href="#" target="_blank" rel="noopener">
        <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/></svg>
        AP Profile
      </a>
      <a class="qlink" id="ql-njump" href="#" target="_blank" rel="noopener">
        <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M13 2L3 14h9l-1 8 10-12h-9l1-8z"/></svg>
        Nostr Profile
      </a>
      <a class="qlink" id="ql-wf" href="#" target="_blank" rel="noopener">
        <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><path d="M2 12h20M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z"/></svg>
        WebFinger
      </a>
    </div>
  </div>

  <div class="card">
    <h2>Relays</h2>
    <div id="relays-list"><span class="empty">loading…</span></div>
    <div style="display:flex;gap:7px;margin-top:10px">
      <input type="text" id="relay-add-input" placeholder="wss://relay.example.com"
        style="flex:1;background:var(--surface2);border:1px solid var(--border);border-radius:5px;padding:5px 9px;color:var(--text);font-size:11px;font-family:monospace"
        onkeydown="if(event.key==='Enter')addRelay()">
      <button class="btn btn-surface" style="padding:5px 12px;font-size:11px" onclick="addRelay()">+ Add</button>
    </div>
    <div id="relay-msg" style="font-size:11px;color:var(--muted);margin-top:5px;min-height:15px"></div>
  </div>
</div>

<!-- Row 2: Bridge Activity -->
<div class="card-full">
  <h2>Bridge Activity</h2>
  <div class="bridge-grid">

    <!-- Nostr panel -->
    <div class="bp bp-nostr">
      <div class="bp-header">⚡ Nostr</div>
      <div class="bp-body">
        <div class="bp-row"><span class="bpl">Relays</span><span class="bpv" id="bp-relays">—</span></div>
        <div class="bp-row"><span class="bpl">Npub</span><span class="bpv sm" id="bp-npub">—</span></div>
      </div>
    </div>

    <!-- Fediverse panel -->
    <div class="bp bp-fediverse">
      <div class="bp-header">🌐 Fediverse</div>
      <div class="bp-body">
        <div class="bp-row"><span class="bpl">Followers</span><span class="bpv big" id="bp-ap-followers">—</span></div>
        <div class="bp-row"><span class="bpl">Known actors</span><span class="bpv" id="bp-ap-actors">—</span></div>
        <div class="bp-row"><span class="bpl">Objects</span><span class="bpv" id="bp-ap-objects">—</span></div>
        <div class="bp-row"><span class="bpl">Last resync</span><span class="bpv sm" id="bp-last-resync">—</span></div>
      </div>
    </div>

    <!-- Bluesky panel -->
    <div class="bp bp-bsky">
      <div class="bp-header">☁ Bluesky</div>
      <div class="bp-body" id="bp-bsky-body">
        <div class="bp-row"><span class="bpl">Status</span><span class="bpv"><span class="badge badge-muted">disabled</span></span></div>
      </div>
    </div>

    <!-- Total panel -->
    <div class="bp bp-total">
      <div class="bp-header">∑ Total</div>
      <div class="bp-body">
        <div class="bp-row"><span class="bpl">Objects</span><span class="bpv big" id="bp-total-obj">—</span></div>
        <div class="bp-row"><span class="bpl">Followers</span><span class="bpv" id="bp-total-fol">—</span></div>
        <div class="bp-row"><span class="bpl">Actors</span><span class="bpv" id="bp-total-act">—</span></div>
      </div>
    </div>

  </div>
</div>

<!-- Row 3: Followers -->
<div class="card-full">
  <h2>Followers</h2>
  <div class="grid2">
    <div>
      <div style="font-size:12px;color:var(--blue);font-weight:600;margin-bottom:8px">🌐 Fediverse</div>
      <div id="fed-followers-container"><span class="empty">loading…</span></div>
    </div>
    <div>
      <div style="font-size:12px;color:var(--sky);font-weight:600;margin-bottom:8px">☁ Bluesky</div>
      <div id="bsky-followers-container"><span class="empty">loading…</span></div>
    </div>
  </div>
</div>

<!-- Row 4: Following -->
<div class="card-full">
  <h2>Following</h2>
  <div class="grid2">

    <!-- Fediverse column -->
    <div>
      <div style="font-size:12px;color:var(--blue);font-weight:600;margin-bottom:8px">🌐 Fediverse</div>
      <div class="followers-list" id="fediverse-following-list" style="max-height:200px;margin-bottom:10px">
        <span class="empty">loading…</span>
      </div>
      <div style="display:flex;gap:8px">
        <input type="text" id="fediverse-follow-input"
          placeholder="alice@mastodon.social"
          style="flex:1;background:var(--surface2);border:1px solid var(--border);border-radius:5px;padding:6px 10px;color:var(--text);font-size:12px;font-family:monospace"
          onkeydown="if(event.key==='Enter')addFollow('fediverse')">
        <button class="btn btn-surface" style="padding:6px 14px;font-size:12px" onclick="addFollow('fediverse')">Follow</button>
      </div>
      <div class="action-msg" id="fediverse-follow-msg"></div>
    </div>

    <!-- Bluesky column -->
    <div>
      <div style="font-size:12px;color:var(--sky);font-weight:600;margin-bottom:8px">☁ Bluesky</div>
      <div class="followers-list" id="bsky-following-list" style="max-height:200px;margin-bottom:10px">
        <span class="empty">loading…</span>
      </div>
      <div style="display:flex;gap:8px">
        <input type="text" id="bsky-follow-input"
          placeholder="user.bsky.social"
          style="flex:1;background:var(--surface2);border:1px solid var(--border);border-radius:5px;padding:6px 10px;color:var(--text);font-size:12px;font-family:monospace"
          onkeydown="if(event.key==='Enter')addFollow('bsky')">
        <button class="btn btn-surface" id="btn-bsky-follow-add" style="padding:6px 14px;font-size:12px" onclick="addFollow('bsky')">Follow</button>
      </div>
      <div class="action-msg" id="bsky-follow-msg"></div>
    </div>

  </div>
</div>

<!-- Row 5a: Import Fediverse Following -->
<div class="card-full">
  <h2>Import Fediverse Following</h2>
  <p style="color:var(--muted);font-size:12px;margin-bottom:12px">
    Paste Fediverse handles (one per line). klistr will resolve them via WebFinger, derive their Nostr pubkeys, and publish an updated kind-3 contact list — merged with your existing follows.
  </p>
  <textarea id="import-textarea"
    placeholder="alice@mastodon.social&#10;bob@hachyderm.io&#10;carol@fosstodon.org"
    style="width:100%;height:110px;background:var(--surface2);border:1px solid var(--border);border-radius:6px;padding:10px 12px;color:var(--text);font-family:'SF Mono',Consolas,monospace;font-size:12px;resize:vertical;line-height:1.6"
  ></textarea>
  <div style="display:flex;align-items:center;gap:10px;margin-top:10px">
    <button class="btn btn-blue" id="btn-import" onclick="importFollowing()">
      <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><line x1="19" y1="8" x2="19" y2="14"/><line x1="22" y1="11" x2="16" y2="11"/></svg>
      Import &amp; Publish Kind-3
    </button>
    <span style="font-size:12px;color:var(--muted)" id="import-status"></span>
  </div>
  <div id="import-results" style="margin-top:14px"></div>
</div>

<!-- Row 5b: Import Bluesky Following (hidden when Bluesky is not configured) -->
<div class="card-full" id="import-bsky-card" style="display:none">
  <h2>Import Bluesky Following</h2>
  <p style="color:var(--muted);font-size:12px;margin-bottom:12px">
    Paste Bluesky handles or DIDs (one per line). klistr will follow each account on Bluesky, derive their Nostr pubkey, and publish an updated kind-3 contact list — merged with your existing follows.
  </p>
  <textarea id="import-bsky-textarea"
    placeholder="alice.bsky.social&#10;bob.bsky.social&#10;did:plc:xxxx"
    style="width:100%;height:110px;background:var(--surface2);border:1px solid var(--border);border-radius:6px;padding:10px 12px;color:var(--text);font-family:'SF Mono',Consolas,monospace;font-size:12px;resize:vertical;line-height:1.6"
  ></textarea>
  <div style="display:flex;align-items:center;gap:10px;margin-top:10px">
    <button class="btn btn-blue" id="btn-import-bsky" onclick="importBskyFollowing()">
      <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><line x1="19" y1="8" x2="19" y2="14"/><line x1="22" y1="11" x2="16" y2="11"/></svg>
      Import &amp; Publish Kind-3
    </button>
    <span style="font-size:12px;color:var(--muted)" id="import-bsky-status"></span>
  </div>
  <div id="import-bsky-results" style="margin-top:14px"></div>
</div>

<!-- Row 5c: Actions -->
<div class="card-full">
  <h2>Actions</h2>
  <div style="display:flex;flex-direction:column;gap:10px">

    <div style="display:flex;align-items:center;gap:14px;flex-wrap:wrap">
      <button class="btn btn-surface" id="btn-republish-kind0" onclick="republishKind0()" style="min-width:178px">
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/><line x1="12" y1="14" x2="12" y2="20"/><polyline points="9 17 12 14 15 17"/></svg>
        Re-broadcast Profile
      </button>
      <span style="font-size:12px;color:var(--muted)">Re-publishes your Nostr profile (kind-0) to all configured relays. Useful after adding a new relay.</span>
    </div>

    <div style="display:flex;align-items:center;gap:14px;flex-wrap:wrap">
      <button class="btn btn-surface" id="btn-republish-kind3" onclick="republishKind3()" style="min-width:178px">
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><line x1="8" y1="6" x2="21" y2="6"/><line x1="8" y1="12" x2="21" y2="12"/><line x1="8" y1="18" x2="21" y2="18"/><line x1="3" y1="6" x2="3.01" y2="6"/><line x1="3" y1="12" x2="3.01" y2="12"/><line x1="3" y1="18" x2="3.01" y2="18"/><line x1="17" y1="3" x2="17" y2="9"/><polyline points="14 6 17 3 20 6"/></svg>
        Re-broadcast Following
      </button>
      <span style="font-size:12px;color:var(--muted)">Re-publishes your contact list (kind-3) to all configured relays. Useful after adding a new relay.</span>
    </div>

    <div style="display:flex;align-items:center;gap:14px;flex-wrap:wrap">
      <button class="btn btn-surface" id="btn-refresh-profiles" onclick="refreshProfiles()" style="min-width:178px">
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><path d="M20 11A8.1 8.1 0 0 0 4.5 9M4 5v4h4M4 13a8.1 8.1 0 0 0 15.5 2M20 19v-4h-4"/></svg>
        Refresh Profiles
      </button>
      <span style="font-size:12px;color:var(--muted)">Re-fetches profiles for all bridged accounts and republishes their Nostr kind-0 metadata. Also runs automatically every 24 hours.</span>
    </div>

    <div id="bsky-sync-row" style="display:flex;align-items:center;gap:14px;flex-wrap:wrap">
      <button class="btn btn-surface" id="btn-bsky-sync" onclick="syncBsky()" style="min-width:178px">
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>
        Sync Bluesky Now
      </button>
      <span style="font-size:12px;color:var(--muted)">Polls Bluesky for new notifications immediately. Useful for testing or when you want to check for new activity without waiting.</span>
    </div>

    <div style="display:flex;align-items:center;gap:14px;flex-wrap:wrap">
      <button class="btn btn-surface" onclick="refreshAll()" style="min-width:178px">
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>
        Refresh Dashboard
      </button>
      <span style="font-size:12px;color:var(--muted)">Reloads the stats and follow lists shown on this page.</span>
    </div>

  </div>
  <div class="action-msg" id="action-msg"></div>
</div>

<!-- Row 6: Settings -->
<div class="card-full">
  <h2>Settings</h2>
  <div style="display:flex;flex-direction:column;gap:16px">

    <!-- Show source link -->
    <label style="display:flex;align-items:center;gap:10px;cursor:pointer;font-size:13px;user-select:none">
      <input type="checkbox" id="set-show-source-link" style="width:15px;height:15px;accent-color:var(--blue);cursor:pointer">
      Append source link (🔗) at the bottom of bridged notes
    </label>

    <!-- Profile fields -->
    <div>
      <div style="font-size:11px;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:.06em;margin-bottom:10px">Profile (saves kind-0 to relays)</div>
      <div style="display:grid;grid-template-columns:120px 1fr;gap:8px 12px;align-items:center;max-width:560px">
        <span style="font-size:12px;color:var(--muted);text-align:right">Display name</span>
        <input type="text" id="set-display-name" placeholder="Alice" style="background:var(--surface2);border:1px solid var(--border);border-radius:5px;padding:6px 9px;color:var(--text);font-size:12px;font-family:inherit">
        <span style="font-size:12px;color:var(--muted);text-align:right">Bio</span>
        <input type="text" id="set-summary" placeholder="My bio" style="background:var(--surface2);border:1px solid var(--border);border-radius:5px;padding:6px 9px;color:var(--text);font-size:12px;font-family:inherit">
        <span style="font-size:12px;color:var(--muted);text-align:right">Picture URL</span>
        <input type="text" id="set-picture" placeholder="https://example.com/avatar.jpg" style="background:var(--surface2);border:1px solid var(--border);border-radius:5px;padding:6px 9px;color:var(--text);font-size:12px;font-family:monospace">
        <span style="font-size:12px;color:var(--muted);text-align:right">Banner URL</span>
        <input type="text" id="set-banner" placeholder="https://example.com/banner.jpg" style="background:var(--surface2);border:1px solid var(--border);border-radius:5px;padding:6px 9px;color:var(--text);font-size:12px;font-family:monospace">
      </div>
    </div>

    <!-- Links & Zap -->
    <div>
      <div style="font-size:11px;font-weight:600;color:var(--muted);text-transform:uppercase;letter-spacing:.06em;margin-bottom:10px">Links &amp; Zap</div>
      <div style="display:grid;grid-template-columns:120px 1fr;gap:8px 12px;align-items:center;max-width:560px">
        <span style="font-size:12px;color:var(--muted);text-align:right">External base URL</span>
        <input type="text" id="set-external-base-url" placeholder="https://njump.me" style="background:var(--surface2);border:1px solid var(--border);border-radius:5px;padding:6px 9px;color:var(--text);font-size:12px;font-family:monospace">
        <span style="font-size:12px;color:var(--muted);text-align:right">Zap pubkey</span>
        <input type="text" id="set-zap-pubkey" placeholder="hex pubkey (optional)" style="background:var(--surface2);border:1px solid var(--border);border-radius:5px;padding:6px 9px;color:var(--text);font-size:12px;font-family:monospace">
        <span style="font-size:12px;color:var(--muted);text-align:right">Zap split</span>
        <input type="number" id="set-zap-split" placeholder="0.1" step="0.01" min="0" max="1" style="background:var(--surface2);border:1px solid var(--border);border-radius:5px;padding:6px 9px;color:var(--text);font-size:12px;font-family:inherit;width:120px">
      </div>
    </div>

  </div>
  <div style="display:flex;align-items:center;gap:12px;margin-top:16px">
    <button class="btn btn-blue" id="btn-save-settings" onclick="saveSettings()">Save Settings</button>
    <span id="settings-msg" class="action-msg"></span>
  </div>
</div>

<!-- Row 7: Log -->
<div class="card-full">
  <h2>Log</h2>
  <div class="log-toolbar">
    <button class="lfb active" data-level="ALL"   onclick="setLogFilter('ALL')">All</button>
    <button class="lfb"        data-level="DEBUG"  onclick="setLogFilter('DEBUG')">Debug</button>
    <button class="lfb"        data-level="INFO"   onclick="setLogFilter('INFO')">Info</button>
    <button class="lfb"        data-level="WARN"   onclick="setLogFilter('WARN')">Warn</button>
    <button class="lfb"        data-level="ERROR"  onclick="setLogFilter('ERROR')">Error</button>
    <div class="log-sep"></div>
    <button class="btn btn-surface" style="padding:4px 12px;font-size:11px" id="btn-refresh-log" onclick="refreshLog()">
      <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5"><polyline points="23 4 23 10 17 10"/><polyline points="1 20 1 14 7 14"/><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15"/></svg>
      Refresh
    </button>
    <button class="btn btn-red" style="padding:4px 12px;font-size:11px" onclick="clearLog()">
      <svg width="11" height="11" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2"/></svg>
      Clear
    </button>
    <span style="font-size:11px;color:var(--muted);margin-left:4px" id="log-ts"></span>
  </div>
  <div id="log"></div>
</div>

</div><!-- /layout -->
<div class="toast" id="toast"></div>

<script>
let autoScroll = true;
let bskyEnabled = false;
let startedAt = 0;
let activeFilter = 'ALL';
let _npub = '';

const levelOrder = {DEBUG:0, INFO:1, WARN:2, ERROR:3};

// ── Helpers ──────────────────────────────────────────────────────────────────
function esc(s) {
  return String(s||'').replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}
function toast(msg) {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.classList.add('show');
  setTimeout(() => el.classList.remove('show'), 3000);
}
function copyText(text) {
  navigator.clipboard.writeText(text).then(() => toast('Copied!')).catch(()=>{});
}
function copyIcon() {
  return '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>';
}
function formatUptime(ts) {
  if (!ts) return '—';
  const sec = Math.floor(Date.now()/1000) - ts;
  const h = Math.floor(sec/3600), m = Math.floor((sec%3600)/60), s = sec%60;
  if (h > 0) return h+'h '+m+'m';
  if (m > 0) return m+'m '+s+'s';
  return s+'s';
}
function relativeTime(iso) {
  if (!iso) return '—';
  const diff = Math.floor((Date.now() - new Date(iso).getTime()) / 1000);
  if (diff < 10) return 'just now';
  if (diff < 60) return diff+'s ago';
  if (diff < 3600) return Math.floor(diff/60)+'m ago';
  if (diff < 86400) return Math.floor(diff/3600)+'h ago';
  return Math.floor(diff/86400)+'d ago';
}
// Parse AP actor URL → @user@domain display string.
function formatFollowerURL(url) {
  try {
    const u = new URL(url);
    const parts = u.pathname.replace(/\/$/,'').split('/').filter(Boolean);
    let username = parts[parts.length-1] || '';
    username = username.replace(/^@/,'');
    if (username) return '@'+username+'@'+u.host;
  } catch {}
  return url;
}
function updateUptime() {
  const el = document.getElementById('hdr-uptime');
  if (el && startedAt) el.textContent = formatUptime(startedAt);
}

// ── Log ──────────────────────────────────────────────────────────────────────
const logEl = document.getElementById('log');
logEl.addEventListener('scroll', () => {
  autoScroll = logEl.scrollTop + logEl.clientHeight >= logEl.scrollHeight - 30;
});

function renderLine(raw) {
  const div = document.createElement('div');
  div.className = 'll';
  let lvl = 'INFO';
  try {
    const j = JSON.parse(raw);
    lvl = (j.level||'INFO').toUpperCase();
    div.classList.add(lvl);
    const ts  = j.time ? new Date(j.time).toLocaleTimeString([],{hour:'2-digit',minute:'2-digit',second:'2-digit'}) : '';
    const msg = j.msg || '';
    const extras = Object.entries(j)
      .filter(([k]) => !['time','level','msg'].includes(k))
      .map(([k,v]) => k+'='+(typeof v==='string' ? v : JSON.stringify(v)))
      .join('  ');
    div.textContent = [ts, lvl.padEnd(5), msg, extras].filter(Boolean).join('  ');
  } catch { div.textContent = raw; }
  if (activeFilter !== 'ALL' && levelOrder[lvl] < levelOrder[activeFilter]) {
    div.style.display = 'none';
  }
  return div;
}

function setLogFilter(level) {
  activeFilter = level;
  document.querySelectorAll('.lfb').forEach(b => b.classList.toggle('active', b.dataset.level === level));
  logEl.querySelectorAll('.ll').forEach(el => {
    const elLvl = [...el.classList].find(c => levelOrder[c] !== undefined) || 'INFO';
    el.style.display = (level==='ALL' || levelOrder[elLvl] >= levelOrder[level]) ? '' : 'none';
  });
}

function clearLog() {
  logEl.innerHTML = '';
  document.getElementById('log-ts').textContent = '';
  toast('Log cleared');
}

async function refreshLog() {
  const btn = document.getElementById('btn-refresh-log');
  btn.disabled = true;
  try {
    const r = await fetch('/web/api/log');
    const lines = await r.json();
    logEl.innerHTML = '';
    (lines||[]).forEach(raw => logEl.appendChild(renderLine(raw)));
    document.getElementById('log-ts').textContent =
      'Last refreshed ' + new Date().toLocaleTimeString([],{hour:'2-digit',minute:'2-digit',second:'2-digit'});
    if (autoScroll) logEl.scrollTop = logEl.scrollHeight;
  } catch(e) {
    toast('Log fetch failed: ' + e.message);
  } finally {
    btn.disabled = false;
  }
}

// ── Status ───────────────────────────────────────────────────────────────────
async function loadStatus() {
  const r = await fetch('/web/api/status');
  const d = await r.json();
  bskyEnabled = d.bsky_enabled;
  startedAt   = d.started_at;
  _npub       = d.npub || '';
  document.getElementById('hdr-version').textContent = 'v'+d.version;
  updateUptime();

  const kv = document.getElementById('status-kv');
  kv.innerHTML = '';
  function row(k, html) {
    const ke = document.createElement('span'); ke.className='k'; ke.textContent=k;
    const ve = document.createElement('span'); ve.className='v'; ve.innerHTML=html;
    kv.appendChild(ke); kv.appendChild(ve);
  }
  row('Domain',   esc(d.domain));
  row('Username', '@'+esc(d.username));
  row('Npub',     esc(d.npub)+' <button class="copy-btn" title="Copy npub" onclick="copyText(_npub)">'+copyIcon()+'</button>');
  row('Bluesky',  d.bsky_enabled
    ? '<span class="badge badge-green">enabled — '+esc(d.bsky_identifier)+'</span>'
    : '<span class="badge badge-muted">disabled</span>');

  // Quick links
  const ql = document.getElementById('quick-links');
  ql.style.display = '';
  document.getElementById('ql-actor').href  = d.domain+'/users/'+d.username;
  document.getElementById('ql-njump').href  = 'https://njump.me/'+d.npub;
  try {
    const wfHost = new URL(d.domain).host;
    document.getElementById('ql-wf').href = d.domain+'/.well-known/webfinger?resource=acct:'+d.username+'@'+wfHost;
  } catch {}

  // Nostr bridge panel — relay count is filled by loadRelays()
  document.getElementById('bp-npub').textContent = d.npub ? d.npub.slice(0,20)+'…' : '—';

  document.getElementById('bsky-sync-row').style.display = d.bsky_enabled ? '' : 'none';
  document.getElementById('import-bsky-card').style.display = d.bsky_enabled ? '' : 'none';
}

// ── Stats ────────────────────────────────────────────────────────────────────
async function loadStats() {
  const r = await fetch('/web/api/stats');
  const d = await r.json();

  // Fediverse panel
  document.getElementById('bp-ap-followers').textContent = d.fediverse_followers ?? '—';
  document.getElementById('bp-ap-actors').textContent    = d.fediverse_actors    ?? '—';
  document.getElementById('bp-ap-objects').textContent   = d.fediverse_objects   ?? '—';
  const resyncEl = document.getElementById('bp-last-resync');
  if (d.last_resync_at) {
    const countSuffix = d.last_resync_count ? ' ('+d.last_resync_count+')' : '';
    resyncEl.textContent = relativeTime(d.last_resync_at) + countSuffix;
    resyncEl.title = d.last_resync_at;
  } else {
    resyncEl.textContent = 'never';
  }

  // Bluesky panel
  const bskyBody = document.getElementById('bp-bsky-body');
  if (d.bsky_enabled) {
    bskyBody.innerHTML =
      '<div class="bp-row"><span class="bpl">Status</span><span class="bpv"><span class="badge badge-green">active</span></span></div>'+
      '<div class="bp-row"><span class="bpl">Followers</span><span class="bpv big">'+esc(String(d.bsky_followers??'—'))+'</span></div>'+
      '<div class="bp-row"><span class="bpl">Objects</span><span class="bpv">'+esc(String(d.bsky_objects??'—'))+'</span></div>'+
      '<div class="bp-row"><span class="bpl">Last poll</span><span class="bpv sm">'+esc(relativeTime(d.bsky_last_poll))+'</span></div>'+
      '<div class="bp-row"><span class="bpl">Last notification</span><span class="bpv sm">'+esc(relativeTime(d.bsky_last_seen))+'</span></div>';
  }

  // Total panel
  const totalFollowers = (d.fediverse_followers ?? 0) + (d.bsky_followers ?? 0);
  document.getElementById('bp-total-obj').textContent = d.total_objects    ?? '—';
  document.getElementById('bp-total-fol').textContent = totalFollowers;
  document.getElementById('bp-total-act').textContent = d.fediverse_actors ?? '—';
}

// ── Followers ────────────────────────────────────────────────────────────────
// Renders items into container with a collapse toggle when count > limit.
function renderCollapsibleList(container, items, emptyMsg, renderItem, limit) {
  if (limit === undefined) limit = 5;
  container.innerHTML = '';
  if (!items || items.length === 0) {
    container.innerHTML = '<span class="empty">'+esc(emptyMsg)+'</span>';
    return;
  }
  const list = document.createElement('div');
  list.className = 'followers-list';
  list.style.maxHeight = 'none';
  list.style.overflow = 'visible';

  const visibleItems = items.slice(0, limit);
  const hiddenItems  = items.slice(limit);

  visibleItems.forEach(item => list.appendChild(renderItem(item)));
  const hiddenEls = hiddenItems.map(item => {
    const el = renderItem(item);
    el.style.display = 'none';
    list.appendChild(el);
    return el;
  });
  container.appendChild(list);

  if (hiddenItems.length > 0) {
    const toggle = document.createElement('button');
    toggle.style.cssText = 'margin-top:6px;width:100%;background:none;border:1px solid var(--border);border-radius:4px;padding:4px 8px;color:var(--muted);cursor:pointer;font-size:11px;font-family:inherit';
    toggle.textContent = 'Show '+hiddenItems.length+' more…';
    let expanded = false;
    toggle.addEventListener('click', () => {
      expanded = !expanded;
      hiddenEls.forEach(el => el.style.display = expanded ? '' : 'none');
      toggle.textContent = expanded ? 'Show less' : 'Show '+hiddenItems.length+' more…';
    });
    container.appendChild(toggle);
  }
}

async function loadFollowers() {
  const r = await fetch('/web/api/followers');
  const d = await r.json();

  // Fediverse followers
  const fedContainer = document.getElementById('fed-followers-container');
  renderCollapsibleList(fedContainer, d.fediverse || [], 'No Fediverse followers yet.', item => {
    const div = document.createElement('div'); div.className = 'follower';
    const handle = formatFollowerURL(item.url);
    div.innerHTML = '<span class="f-handle">'+esc(handle)+'</span>'+
      '<a href="'+esc(item.url)+'" target="_blank" rel="noopener">→ profile</a>';
    return div;
  });

  // Bluesky followers
  const bskyContainer = document.getElementById('bsky-followers-container');
  renderCollapsibleList(bskyContainer, d.bluesky || [], 'No Bluesky followers yet.', item => {
    const div = document.createElement('div'); div.className = 'follower';
    div.innerHTML = '<span class="f-handle">@'+esc(item.handle)+'</span>'+
      '<a href="'+esc(item.url)+'" target="_blank" rel="noopener">→ profile</a>';
    return div;
  });
}

// ── Actions ──────────────────────────────────────────────────────────────────
async function syncBsky() {
  const btn = document.getElementById('btn-bsky-sync');
  btn.disabled = true;
  const orig = btn.innerHTML;
  btn.textContent = 'Triggering…';
  try {
    const r = await fetch('/web/api/sync-bsky', {method:'POST'});
    const d = await r.json();
    document.getElementById('action-msg').textContent = d.message;
    toast(d.message);
    setTimeout(loadStats, 2000);
  } catch(e) {
    document.getElementById('action-msg').textContent = 'Error: '+e.message;
  } finally {
    btn.disabled = false;
    btn.innerHTML = orig;
  }
}

async function refreshProfiles() {
  const btn = document.getElementById('btn-refresh-profiles');
  const msg = document.getElementById('action-msg');
  btn.disabled = true;
  const orig = btn.innerHTML;
  btn.textContent = 'Refreshing…';
  msg.textContent = '';
  try {
    const r = await fetch('/web/api/resync-follows', {method:'POST'});
    const d = await r.json();
    msg.textContent = d.message;
    toast(d.message);
    setTimeout(loadStats, 3000);
  } catch(e) {
    msg.textContent = 'Error: '+e.message;
  } finally {
    btn.disabled = false;
    btn.innerHTML = orig;
  }
}

async function republishKind0() {
  const btn = document.getElementById('btn-republish-kind0');
  btn.disabled = true;
  const orig = btn.innerHTML;
  btn.textContent = 'Publishing…';
  try {
    const r = await fetch('/web/api/republish-kind0', {method:'POST'});
    const d = await r.json();
    document.getElementById('action-msg').textContent = d.message;
    toast(d.message);
  } catch(e) {
    document.getElementById('action-msg').textContent = 'Error: '+e.message;
  } finally {
    btn.disabled = false;
    btn.innerHTML = orig;
  }
}

async function republishKind3() {
  const btn = document.getElementById('btn-republish-kind3');
  btn.disabled = true;
  const orig = btn.innerHTML;
  btn.textContent = 'Publishing…';
  try {
    const r = await fetch('/web/api/republish-kind3', {method:'POST'});
    const d = await r.json();
    document.getElementById('action-msg').textContent = d.message;
    toast(d.message);
  } catch(e) {
    document.getElementById('action-msg').textContent = 'Error: '+e.message;
  } finally {
    btn.disabled = false;
    btn.innerHTML = orig;
  }
}

function refreshAll() {
  loadStats(); loadFollowers(); loadFollowing(); loadRelays();
  toast('Dashboard refreshed');
}

// ── Following management ─────────────────────────────────────────────────────
async function loadFollowing() {
  try {
    const r = await fetch('/web/api/following');
    const d = await r.json();

    // Fediverse list
    const fedEl = document.getElementById('fediverse-following-list');
    fedEl.innerHTML = '';
    if (!d.fediverse || d.fediverse.length === 0) {
      fedEl.innerHTML = '<span class="empty">Not following anyone on Fediverse yet.</span>';
    } else {
      d.fediverse.forEach(item => {
        const div = document.createElement('div'); div.className = 'follower';
        div.innerHTML =
          '<span class="f-handle">'+esc(item.handle)+'</span>'+
          '<button style="background:none;border:none;cursor:pointer;color:var(--red);font-size:14px;opacity:.7;padding:0 4px" '+
            'title="Unfollow" onclick="removeFollow(\''+esc(item.actor)+'\',\'fediverse\')">✕</button>';
        fedEl.appendChild(div);
      });
    }

    // Bluesky list
    const bskyEl = document.getElementById('bsky-following-list');
    bskyEl.innerHTML = '';
    if (!bskyEnabled) {
      bskyEl.innerHTML = '<span class="empty">Bluesky not configured.</span>';
      document.getElementById('bsky-follow-input').disabled = true;
      document.getElementById('btn-bsky-follow-add').disabled = true;
    } else if (!d.bluesky || d.bluesky.length === 0) {
      bskyEl.innerHTML = '<span class="empty">Not following anyone on Bluesky yet.</span>';
    } else {
      d.bluesky.forEach(item => {
        const displayHandle = item.handle || item.did;
        const removeKey = item.handle || item.did;
        const div = document.createElement('div'); div.className = 'follower';
        div.innerHTML =
          '<span class="f-handle">'+esc(displayHandle)+'</span>'+
          '<button style="background:none;border:none;cursor:pointer;color:var(--red);font-size:14px;opacity:.7;padding:0 4px" '+
            'title="Unfollow" onclick="removeFollow(\''+esc(removeKey)+'\',\'bsky\')">✕</button>';
        bskyEl.appendChild(div);
      });
    }
  } catch(e) {
    console.warn('loadFollowing failed', e);
  }
}

async function addFollow(bridge) {
  const inputId = bridge === 'fediverse' ? 'fediverse-follow-input' : 'bsky-follow-input';
  const msgId   = bridge === 'fediverse' ? 'fediverse-follow-msg'   : 'bsky-follow-msg';
  const handle = document.getElementById(inputId).value.trim();
  if (!handle) { toast('Enter a handle first'); return; }

  const msgEl = document.getElementById(msgId);
  msgEl.textContent = 'Following…';
  try {
    const r = await fetch('/web/api/follow', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({handle, bridge}),
    });
    const d = await r.json();
    if (r.ok) {
      document.getElementById(inputId).value = '';
      msgEl.textContent = '';
      toast('Now following ' + handle);
      loadFollowing();
    } else {
      msgEl.textContent = 'Error: ' + (d.error || r.statusText);
    }
  } catch(e) {
    msgEl.textContent = 'Error: ' + e.message;
  }
}

async function removeFollow(handle, bridge) {
  if (!confirm('Unfollow ' + handle + '?')) return;

  try {
    const r = await fetch('/web/api/unfollow', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({handle, bridge}),
    });
    const d = await r.json();
    if (r.ok) {
      toast('Unfollowed ' + handle);
      loadFollowing();
    } else {
      toast('Error: ' + (d.error || r.statusText));
    }
  } catch(e) {
    toast('Error: ' + e.message);
  }
}


// ── Import Following ─────────────────────────────────────────────────────────
async function importFollowing() {
  const raw = document.getElementById('import-textarea').value;
  const handles = raw.split('\n').map(h => h.trim()).filter(Boolean);
  if (!handles.length) { toast('No handles entered'); return; }

  const btn    = document.getElementById('btn-import');
  const status = document.getElementById('import-status');
  btn.disabled = true;
  const origHTML = btn.innerHTML;
  btn.textContent = 'Resolving…';
  status.textContent = 'Fetching existing follows from relay, this may take up to 8s…';

  try {
    const r = await fetch('/web/api/import-following', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({handles}),
    });
    const d = await r.json();

    // Status line
    const ok  = (d.results||[]).filter(r => r.status==='ok').length;
    const err = (d.results||[]).filter(r => r.status==='error').length;
    let msg = ok + ' resolved';
    if (err) msg += ', ' + err + ' failed';
    if (d.published) msg += ' — kind-3 published (' + d.total_follows + ' total follows)';
    else if (d.error) msg += ' — ' + d.error;
    if (!d.fetched_existing) msg += ' ⚠ no existing kind-3 found on relay';
    status.textContent = msg;

    // Result table
    const el = document.getElementById('import-results');
    if (!d.results || d.results.length === 0) { el.innerHTML = ''; return; }

    let html = '<table style="width:100%;border-collapse:collapse;font-size:12px;margin-top:4px">'
      + '<thead><tr style="color:var(--muted);text-align:left">'
      + '<th style="padding:5px 8px;border-bottom:1px solid var(--border)">Handle</th>'
      + '<th style="padding:5px 8px;border-bottom:1px solid var(--border)">Status</th>'
      + '<th style="padding:5px 8px;border-bottom:1px solid var(--border)">Npub / Error</th>'
      + '</tr></thead><tbody>';

    (d.results||[]).forEach(r => {
      const isOk = r.status === 'ok';
      const statusCell = isOk
        ? '<span style="color:var(--green);font-weight:600">✓ ok</span>'
        : '<span style="color:var(--red);font-weight:600">✗ error</span>';
      const detail = isOk
        ? '<span style="font-family:monospace;color:var(--muted)">' + esc(r.npub||'') + '</span>'
        : '<span style="color:var(--red)">' + esc(r.error||'') + '</span>';
      html += '<tr style="border-bottom:1px solid var(--border)">'
        + '<td style="padding:5px 8px;font-family:monospace">' + esc(r.handle) + '</td>'
        + '<td style="padding:5px 8px">' + statusCell + '</td>'
        + '<td style="padding:5px 8px">' + detail + '</td>'
        + '</tr>';
    });
    html += '</tbody></table>';
    el.innerHTML = html;

    if (d.published) { toast('Kind-3 published — ' + ok + ' new follows added'); loadFollowers(); }
  } catch(e) {
    status.textContent = 'Error: ' + e.message;
  } finally {
    btn.disabled = false;
    btn.innerHTML = origHTML;
  }
}

// ── Import Bluesky Following ──────────────────────────────────────────────────
async function importBskyFollowing() {
  const raw = document.getElementById('import-bsky-textarea').value;
  const handles = raw.split('\n').map(h => h.trim()).filter(Boolean);
  if (!handles.length) { toast('No handles entered'); return; }

  const btn    = document.getElementById('btn-import-bsky');
  const status = document.getElementById('import-bsky-status');
  btn.disabled = true;
  const origHTML = btn.innerHTML;
  btn.textContent = 'Following…';
  status.textContent = 'Resolving handles and following on Bluesky…';

  try {
    const r = await fetch('/web/api/import-bsky-following', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({handles}),
    });
    const d = await r.json();

    const ok  = (d.results||[]).filter(r => r.status==='ok').length;
    const err = (d.results||[]).filter(r => r.status==='error').length;
    let msg = ok + ' followed';
    if (err) msg += ', ' + err + ' failed';
    if (d.published) msg += ' — kind-3 published (' + d.total_follows + ' total follows)';
    else if (d.error) msg += ' — ' + d.error;
    if (!d.fetched_existing) msg += ' ⚠ no existing kind-3 found on relay';
    status.textContent = msg;

    const el = document.getElementById('import-bsky-results');
    if (!d.results || d.results.length === 0) { el.innerHTML = ''; return; }

    let html = '<table style="width:100%;border-collapse:collapse;font-size:12px;margin-top:4px">'
      + '<thead><tr style="color:var(--muted);text-align:left">'
      + '<th style="padding:5px 8px;border-bottom:1px solid var(--border)">Handle / DID</th>'
      + '<th style="padding:5px 8px;border-bottom:1px solid var(--border)">Status</th>'
      + '<th style="padding:5px 8px;border-bottom:1px solid var(--border)">Npub / Error</th>'
      + '</tr></thead><tbody>';

    (d.results||[]).forEach(r => {
      const isOk = r.status === 'ok';
      const statusCell = isOk
        ? '<span style="color:var(--green);font-weight:600">✓ ok</span>'
        : '<span style="color:var(--red);font-weight:600">✗ error</span>';
      const detail = isOk
        ? '<span style="font-family:monospace;color:var(--muted)">' + esc(r.npub||'') + '</span>'
        : '<span style="color:var(--red)">' + esc(r.error||'') + '</span>';
      html += '<tr style="border-bottom:1px solid var(--border)">'
        + '<td style="padding:5px 8px;font-family:monospace">' + esc(r.handle) + '</td>'
        + '<td style="padding:5px 8px">' + statusCell + '</td>'
        + '<td style="padding:5px 8px">' + detail + '</td>'
        + '</tr>';
    });
    html += '</tbody></table>';
    el.innerHTML = html;

    if (d.published) { toast('Kind-3 published — ' + ok + ' new Bluesky follows added'); loadFollowing(); }
  } catch(e) {
    status.textContent = 'Error: ' + e.message;
  } finally {
    btn.disabled = false;
    btn.innerHTML = origHTML;
  }
}

// ── Relay management ─────────────────────────────────────────────────────────
async function loadRelays() {
  try {
    const r = await fetch('/web/api/relays');
    const relays = await r.json();
    const rl = document.getElementById('relays-list');
    rl.innerHTML = '';
    if (!relays || relays.length === 0) {
      rl.innerHTML = '<span class="empty">No relays configured.</span>';
      document.getElementById('bp-relays').textContent = '0';
      return;
    }
    document.getElementById('bp-relays').textContent = relays.length;
    relays.forEach(relay => {
      const row = document.createElement('div');
      row.className = 'relay-row';
      row.dataset.url = relay.url;
      let dotColor = 'var(--green)';
      let badge = '<span class="relay-cb relay-cb-ok">ok</span>';
      if (relay.circuit_open) {
        dotColor = 'var(--red)';
        const secs = relay.cooldown_remaining_secs||0;
        const cd = secs > 60 ? Math.floor(secs/60)+'m '+String(secs%60).padStart(2,'0')+'s' : secs+'s';
        badge = '<span class="relay-cb relay-cb-open">circuit open · '+esc(cd)+'</span>';
      } else if (relay.fail_count > 0) {
        dotColor = 'var(--yellow)';
        badge = '<span class="relay-cb relay-cb-warn">'+relay.fail_count+' fail(s)</span>';
      }
      const resetBtn = (relay.circuit_open || relay.fail_count > 0)
        ? '<button class="rbtn rbtn-blue" onclick="resetCircuit(\''+esc(relay.url)+'\')">Reset</button>'
        : '';
      row.innerHTML =
        '<span class="relay-dot" style="background:'+dotColor+'"></span>'+
        '<span class="relay-url">'+esc(relay.url)+'</span>'+
        badge+
        '<div class="relay-acts">'+
          resetBtn+
          '<button class="rbtn" onclick="pingRelay(\''+esc(relay.url)+'\',this)">Test</button>'+
          '<button class="rbtn rbtn-red" onclick="removeRelay(\''+esc(relay.url)+'\')">×</button>'+
        '</div>';
      rl.appendChild(row);
    });
  } catch(e) {
    console.warn('loadRelays failed', e);
  }
}

async function pingRelay(url, btn) {
  const orig = btn ? btn.textContent : '';
  if (btn) { btn.disabled = true; btn.textContent = '…'; }
  try {
    const r = await fetch('/web/api/relays/test', {
      method: 'POST',
      headers: {'Content-Type':'application/json'},
      body: JSON.stringify({url})
    });
    const d = await r.json();
    toast(d.ok ? '✓ '+url+' · '+d.latency+'ms' : '✗ '+url+': '+(d.error||'failed'));
    // Update the badge in-place so the test result is immediately visible.
    // Do NOT call loadRelays() here — the circuit-breaker state on the server
    // is unaffected by a manual test, so reloading would reset the badge to
    // "ok" even when the test just reported a failure.
    const row = document.querySelector('.relay-row[data-url="'+url.replace(/"/g,'&quot;')+'"]');
    if (row) {
      const dot = row.querySelector('.relay-dot');
      const existingBadge = row.querySelector('.relay-cb');
      let badge, dotColor;
      if (d.ok) {
        dotColor = 'var(--green)';
        badge = '<span class="relay-cb relay-cb-ok">✓ '+d.latency+'ms</span>';
      } else {
        dotColor = 'var(--red)';
        // Truncate long errors to keep the row tidy.
        const short = (d.error||'failed').replace(/^error opening websocket[^:]*:\s*/i,'').slice(0,60);
        badge = '<span class="relay-cb relay-cb-open">✗ '+esc(short)+'</span>';
      }
      if (dot) dot.style.background = dotColor;
      if (existingBadge) existingBadge.outerHTML = badge;
    }
  } catch(e) {
    toast('Test failed: '+e.message);
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = orig; }
  }
}

async function addRelay() {
  const input = document.getElementById('relay-add-input');
  const msg = document.getElementById('relay-msg');
  const url = input.value.trim();
  if (!url) return;
  msg.textContent = 'Adding…';
  try {
    const r = await fetch('/web/api/relays', {
      method: 'POST',
      headers: {'Content-Type':'application/json'},
      body: JSON.stringify({url})
    });
    const d = await r.json();
    if (r.ok) {
      input.value = '';
      msg.textContent = '';
      toast(d.message || 'Done');
      loadRelays();
    } else {
      msg.textContent = 'Error: '+(d||r.statusText);
    }
  } catch(e) {
    msg.textContent = 'Error: '+e.message;
  }
}

async function removeRelay(url) {
  if (!confirm('Remove relay '+url+'?')) return;
  try {
    const r = await fetch('/web/api/relays', {
      method: 'DELETE',
      headers: {'Content-Type':'application/json'},
      body: JSON.stringify({url})
    });
    const d = await r.json();
    toast(d.message || (d.removed ? 'Removed' : 'Not found'));
    loadRelays();
  } catch(e) {
    toast('Error: '+e.message);
  }
}

async function resetCircuit(url) {
  try {
    await fetch('/web/api/relays/reset-circuit', {
      method: 'POST',
      headers: {'Content-Type':'application/json'},
      body: JSON.stringify({url})
    });
    toast('Circuit reset: '+url);
    setTimeout(loadRelays, 300);
  } catch(e) {
    toast('Error: '+e.message);
  }
}

// ── Settings ─────────────────────────────────────────────────────────────────
async function loadSettings() {
  try {
    const r = await fetch('/web/api/settings');
    const d = await r.json();
    document.getElementById('set-show-source-link').checked = !!d.show_source_link;
    document.getElementById('set-display-name').value = d.display_name || '';
    document.getElementById('set-summary').value = d.summary || '';
    document.getElementById('set-picture').value = d.picture || '';
    document.getElementById('set-banner').value = d.banner || '';
    document.getElementById('set-external-base-url').value = d.external_base_url || '';
    document.getElementById('set-zap-pubkey').value = d.zap_pubkey || '';
    document.getElementById('set-zap-split').value = d.zap_split != null ? d.zap_split : '';
  } catch(e) {
    console.warn('loadSettings failed', e);
  }
}

async function saveSettings() {
  const btn = document.getElementById('btn-save-settings');
  const msg = document.getElementById('settings-msg');
  btn.disabled = true;
  const origHTML = btn.innerHTML;
  btn.textContent = 'Saving…';
  msg.textContent = '';
  msg.style.color = '';
  try {
    const zapVal = document.getElementById('set-zap-split').value;
    const body = {
      show_source_link: document.getElementById('set-show-source-link').checked,
      display_name:     document.getElementById('set-display-name').value,
      summary:          document.getElementById('set-summary').value,
      picture:          document.getElementById('set-picture').value,
      banner:           document.getElementById('set-banner').value,
      external_base_url: document.getElementById('set-external-base-url').value,
      zap_pubkey:       document.getElementById('set-zap-pubkey').value,
      zap_split:        zapVal !== '' ? parseFloat(zapVal) : 0.1,
    };
    const r = await fetch('/web/api/settings', {
      method: 'PATCH',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify(body),
    });
    if (r.ok) {
      msg.textContent = 'Saved.';
      msg.style.color = 'var(--green)';
      toast('Settings saved.');
      setTimeout(() => { msg.textContent = ''; msg.style.color = ''; }, 3000);
    } else {
      const text = await r.text();
      msg.textContent = 'Error: ' + text;
      msg.style.color = 'var(--red)';
    }
  } catch(e) {
    msg.textContent = 'Error: ' + e.message;
    msg.style.color = 'var(--red)';
  } finally {
    btn.disabled = false;
    btn.innerHTML = origHTML;
  }
}

// ── Init ─────────────────────────────────────────────────────────────────────
// loadFollowing depends on bskyEnabled (set by loadStatus), so chain it.
loadStatus().then(() => loadFollowing()).catch(e => console.error('loadFollowing failed', e));
Promise.all([loadStats(), loadFollowers(), loadRelays(), loadSettings()]).catch(e => console.error('init failed', e));

setInterval(loadStats,    30000);
setInterval(loadRelays,   15000);
setInterval(updateUptime, 10000);

// Load log on first visit.
refreshLog();
</script>
</body>
</html>`

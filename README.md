# klistr

A self-hosted personal bridge that connects your Nostr identity to the Fediverse.

You run one instance, for yourself. Your Nostr posts appear on the Fediverse and Bluesky. Fediverse users can follow you. It's your identity across all three networks, simultaneously.

---

## What it does

klistr is an **ActivityPub server** and a **Nostr client** running in a single process. The Bluesky bridge is an optional add-on that mirrors your activity to Bluesky via their API.

```
Your Nostr key → relay subscription (author filter)
                       ↓
              nostr.Handler
                  ↓         ↓
       ap.Federator      bsky.Poster
           ↓                 ↓
  POST to AP inboxes    Bluesky XRPC API
  (Fediverse followers) (your Bluesky account)

Fediverse user → POST /inbox        Bluesky notification poll (30s)
                       ↓                         ↓
              ap.APHandler               bsky.Poller
                       ↓                         ↓
         Nostr Publisher → relay ←───────────────┘
```

### Protocol mapping

| ActivityPub → Nostr | Kind | Notes |
|---|---|---|
| `Create(Note)` | `1` | Text posts; threaded replies (10-level ancestry walk); images as NIP-94 `imeta` tags; content warnings preserved; `r`-tag with original URL |
| `Create(Article)` | `30023` | Long-form articles |
| `Create(Question)` | `1068` | Polls (NIP-69) |
| `Announce` | `6` | Reposts |
| `Update(Actor)` | `0` | Profile updates |
| `Like` | `7` (`+`) | Reactions |
| `Like` + zap extension | `9735` | Zap receipts (from Zap-aware AP servers) |
| `EmojiReact` | `7` (emoji) | Emoji reactions |
| `Delete` | `5` | Deletions |
| `Block` | — | Cross-protocol mute (AP Block → tracked for unmute) |

### Reliable delivery

All outbound deliveries (AP federation, Nostr relay publish, Bluesky XRPC) flow through a persistent outbox queue backed by the database. If a relay or server is down, the bridge retries with exponential backoff (up to 1 hour) before dead-lettering. Priority lanes ensure real-time interactions (replies, likes) are never starved by background work (profile resyncs). Dead-lettered items are visible in the admin UI and can be manually retried.

### Signing

- **Your posts** (Nostr → AP) are signed with your real Nostr private key.
- **Remote AP actors** (AP → Nostr) get deterministic derived keys: `SHA-256(yourPrivKey + ":" + apActorID)`. No extra keys are stored.

### Identity

- Your AP handle: `@<NOSTR_USERNAME>@your-domain.com`
- Your AP actor URL: `https://your-domain.com/users/<NOSTR_USERNAME>`
- Your Nostr events become AP objects at: `https://your-domain.com/objects/<event-id>`
- Fediverse users you follow get a NIP-05 identifier: `username_at_domain@your-domain.com`

When you follow a Fediverse user via klistr, their bridged Nostr profile includes a `nip05` field pointing back to your bridge. Nostr clients that support NIP-05 will show `doktorzjivago_at_mastodonsweden.se@your-domain.com` with a ✓ verified badge instead of a raw npub. The identifier also resolves via `GET /.well-known/nostr.json?name=username_at_domain` for any client to verify.

---

## Non-technical: how to get started

You need three things before running klistr:

1. **A domain name** pointing to a server you control (e.g. `klistr.alice.com`). It must be reachable over HTTPS in production.
2. **Your Nostr private key** in hex format. Most Nostr apps can export this. It starts with `nsec...` — you need the raw hex version. Tools like [nostr.band](https://nostr.band/) can convert `nsec` → hex.
3. **A server** (a VPS, a Raspberry Pi, anything that can run a Linux binary and accept HTTPS traffic).

### Step 1 — Download or build the binary

If you have Go installed:

```bash
git clone https://github.com/klppl/klistr
cd klistr
go build ./cmd/klistr
```

Or grab a prebuilt binary from the releases page.

### Step 2 — Create your config file

Copy the example and fill it in:

```bash
cp .env.example .env
```

Open `.env` in any text editor. The only required fields are:

```bash
LOCAL_DOMAIN=https://klistr.alice.com   # your domain, with https://
NOSTR_PRIVATE_KEY=your_hex_private_key  # your Nostr key in hex (64 characters)
NOSTR_USERNAME=alice                    # your handle (alice → alice@klistr.alice.com)
```

Everything else has sensible defaults.

### Step 3 — Run it

```bash
# Load your config
source .env   # or export the variables manually

# Start the bridge
./klistr
```

On first run it will:
- Create a SQLite database (`klistr.db`) automatically
- Generate RSA keys for HTTP Signatures (`private.pem`, `public.pem`) automatically
- Start listening on port `8000`

### Step 4 — Set up HTTPS (production)

Put klistr behind a reverse proxy. Example with **Caddy** (handles HTTPS automatically):

```
klistr.alice.com {
    reverse_proxy localhost:8000
}
```

Example with **nginx**:

```nginx
server {
    listen 443 ssl;
    server_name klistr.alice.com;

    location / {
        proxy_pass http://localhost:8000;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

### Step 5 — Verify it works

```bash
# WebFinger — Fediverse discovery
curl https://klistr.alice.com/.well-known/webfinger?resource=acct:alice@klistr.alice.com

# Actor profile
curl https://klistr.alice.com/users/alice

# Health check
curl https://klistr.alice.com/api/healthcheck
```

Then search for `@alice@klistr.alice.com` from any Mastodon (or other Fediverse) account. Once you follow yourself, your new Nostr posts will appear on the Fediverse.

### Docker (alternative)

```bash
cp .env.example .env
# Edit .env with your settings
docker compose up -d
```

---

## Bluesky bridge (optional)

The Bluesky bridge works differently from the Fediverse bridge. For ActivityPub, klistr *is* the server — Mastodon talks directly to it. For Bluesky, klistr acts as a **client** to the AT Protocol network, which means **you need a Bluesky account**.

> **Why the difference?** ActivityPub is simple HTTP + JSON, easy to implement server-side from scratch. AT Protocol (Bluesky's protocol) requires a Personal Data Server (PDS) with Merkle tree storage, DID identity management, and a firehose — far too complex to embed in a lightweight bridge.

### Step 1 — Create a Bluesky account

Register at [bsky.app](https://bsky.app) (free). This account will be the Bluesky face of your Nostr identity — think of it as your Bluesky "mirror".

### Step 2 — Use your domain as your Bluesky handle (recommended)

Bluesky supports verified domain handles. Instead of `@alice.bsky.social`, you can be `@yourdomain.com` — the same domain klistr runs on. This has two benefits:

- It immediately signals to Bluesky users that this is a bridge account tied to your real domain/identity
- It looks like a natural extension of your Nostr/Fediverse identity rather than a separate account

**To set your domain as your Bluesky handle:**

1. In Bluesky: **Settings → Handle → "I have my own domain"**
2. Bluesky will show you your DID (a string like `did:plc:abc123...`)
3. Add a DNS TXT record to verify ownership:
   ```
   Record type: TXT
   Host:        _atproto.yourdomain.com
   Value:       did=did:plc:abc123...
   ```
4. Click **Verify** in Bluesky — your handle is now `@yourdomain.com`

If you can't add DNS records, you can instead serve the DID as a file. Create a file at `https://yourdomain.com/.well-known/atproto-did` containing only your DID string (no quotes, just the text).

### Step 3 — Create an app password

Go to **Settings → App Passwords → Add App Password** and create one named `klistr`. Copy the password (it looks like `xxxx-xxxx-xxxx-xxxx`).

> Use an app password, not your main password. App passwords have limited permissions and can be revoked independently.

### Step 4 — Add to your klistr config

```bash
BSKY_IDENTIFIER=yourdomain.com   # or alice.bsky.social if you skipped domain setup
BSKY_APP_PASSWORD=xxxx-xxxx-xxxx-xxxx
```

Restart klistr. You should see this log line on startup:

```
{"level":"INFO","msg":"bsky bridge enabled","identifier":"yourdomain.com"}
```

### What gets bridged

| Nostr event | Bluesky action |
|---|---|
| Kind 1 (note) | Creates a post (with images via blob upload, quote posts via `embed.record`) |
| Kind 1 + `content-warning` tag | Creates a post with Bluesky self-label content warning |
| Kind 5 (deletion) | Deletes the bridged post |
| Kind 6 (repost) | Reposts (if the original was bridged) |
| Kind 7 `+` (like) | Likes (if the original was bridged) |
| Kind 9735 (zap receipt) | Likes (Lightning zaps become Bluesky likes) |
| Kind 10000 (mute list) | Mutes/unmutes actors on Bluesky (cross-protocol sync) |

| Bluesky → Nostr | How | Requires |
|---|---|---|
| Posts from accounts you follow | Kind 1 (signed with a derived key per Bluesky author); threaded if parent is known | default (disable with `BSKY_BRIDGE_TIMELINE=false`) |
| Reply to your post | Threaded Kind 1 reply (signed with a derived key for the Bluesky author), or NIP-04 DM if the parent post isn't bridged | default |
| Like on your post | Kind 7 reaction | default |
| Repost of your post | Kind 6 repost | default |
| Mention / quote | NIP-04 DM notification to yourself | default |
| New follower | NIP-04 DM notification to yourself | default |
| Images (`app.bsky.embed.images`) | NIP-94 `imeta` tags + CDN image URL appended to content | default |
| Facet links (anchor text) | URL appended to content if not already visible in text | default |
| Link cards (`embed.external`) | URL appended to content if not already visible in text | default |

> **Timeline bridging is on by default.** Set `BSKY_BRIDGE_TIMELINE=false` to disable it and receive only interactions *targeting you* (replies, likes, reposts of your posts).

**Notes:**
- Long Nostr posts (> 300 characters) are truncated and a link to the full post on njump.me is appended.
- **Images** from Nostr posts (NIP-94 `imeta` tags) are downloaded, uploaded as Bluesky blobs, and attached as native image embeds (max 4 images, 1MB each).
- **Quote posts** (Nostr `q`-tag) become native Bluesky quote posts (`embed.record`). When a post has both images and a quote, `embed.recordWithMedia` is used.
- **Content warnings** (NIP-36 `content-warning` tag) become Bluesky self-labels, and vice versa.
- **Mentions** (`nostr:npub` references) are converted to Bluesky mention facets when the DID is known, and Bluesky mention facets become Nostr `p`-tags.
- **Zap receipts** (kind 9735) are bridged as Bluesky likes. On ActivityPub, zaps use a dual-type `["Zap", "Like"]` so servers that don't understand zaps see a like instead.
- **Mute list sync** (NIP-51 kind 10000) propagates mutes to Bluesky and ActivityPub. When you mute someone on Nostr, they're muted on Bluesky and blocked on the Fediverse.
- Bluesky is polled every 30 seconds for new notifications and timeline posts. The poller paginates automatically on catch-up after a restart, so no posts are missed.
- The bridge stores AT URIs in the same database table as ActivityPub IDs, so likes/reposts/deletes and reply threading can be correctly linked.
- Replying from Nostr to a bridged Bluesky reply will thread correctly back into the Bluesky conversation.
- All bridged posts include an `r`-tag with the original post URL for "View Original" links in Nostr clients.

---

## Web admin UI (optional)

Set `WEB_ADMIN=<password>` to enable a dashboard at `https://your-domain.com/web`. It's protected by HTTP Basic Auth (any username, the password you set).

### What's on the dashboard

| Section | What it shows |
|---|---|
| **Node** | Domain, username, npub (with copy button), Bluesky status, uptime. Quick links to AP profile, Nostr profile (njump), WebFinger. |
| **Configured Relays** | Live relay list with circuit-breaker status (OK / degraded / open). Per-relay Test (ping), Reset circuit, and Remove buttons. Add new relays at runtime. Changes persist across restarts. |
| **Bridge Activity** | Per-bridge stat panels — Nostr (relay count), Fediverse (followers, known actors, bridged objects), Bluesky (status, bridged objects, last sync time), Total. |
| **Fediverse Followers** | List of everyone following you on the Fediverse, shown as `@user@domain`. |
| **Following** | Two-column panel (Fediverse \| Bluesky) showing who you follow on each bridge, with per-row unfollow buttons and an add-handle input. Fediverse: WebFinger-resolves the handle and publishes a kind-3; ActivityPub Follow is sent automatically. Bluesky: creates the follow record on Bluesky and updates the kind-3. Bluesky panel is disabled when the bridge is not configured. |
| **Import Fediverse Following** | Paste Fediverse handles (`user@domain.tld`, one per line). klistr resolves them via WebFinger, derives their Nostr pubkeys, fetches your current kind-3 from the relay to preserve existing follows, and publishes a merged kind-3 contact-list event. The bridge then sends ActivityPub Follow activities automatically. |
| **Settings** | Edit display name, bio, picture URL, banner URL, external base URL, zap config, and the source-link toggle — all without restarting. Changes are saved to the database and survive container recreation. Profile changes (name/bio/picture/banner) immediately re-publish your kind-0 to relays. |
| **Actions** | Force an immediate Bluesky notification poll; re-sync all bridged account profiles; refresh dashboard. |
| **Log** | Last 500 log lines from the ring buffer. Click **Refresh** to update. Filter by level (All / Debug / Info / Warn / Error). |

---

## Configuration reference

Variables marked **admin UI** can also be changed at runtime from the `/web` dashboard without restarting — they are saved to the database and override the env var on every subsequent start.

| Variable | Default | Required | Description |
|---|---|---|---|
| `LOCAL_DOMAIN` | `http://localhost:8000` | **Yes** | Your public domain (HTTPS in production) |
| `NOSTR_PRIVATE_KEY` | — | **Yes** | Your Nostr private key in hex |
| `NOSTR_USERNAME` | first 8 chars of pubkey | No | Your handle on this bridge (e.g. `alice`) |
| `NOSTR_DISPLAY_NAME` | value of `NOSTR_USERNAME` | No | Display name. **Admin UI** — changes re-publish kind-0 immediately. |
| `NOSTR_SUMMARY` | — | No | Bio / profile description. **Admin UI** — changes re-publish kind-0 immediately. |
| `NOSTR_PICTURE` | — | No | Avatar image URL. **Admin UI** — changes re-publish kind-0 immediately. |
| `NOSTR_BANNER` | — | No | Banner/header image URL. **Admin UI** — changes re-publish kind-0 immediately. |
| `NOSTR_RELAY` | `wss://relay.mostr.pub` | No | Nostr relays, comma-separated. **Fully managed via admin UI** — you can omit this env var entirely once you've configured relays in `/web`. |
| `DATABASE_URL` | `klistr.db` | No | SQLite file path or `postgres://...` URL |
| `PORT` | `8000` | No | HTTP server port |
| `SIGN_FETCH` | `true` | No | Sign outbound HTTP requests (recommended) |
| `LOG_LEVEL` | `info` | No | `info` or `debug` |
| `BSKY_IDENTIFIER` | — | No | Bluesky handle or DID (enables Bluesky bridge) |
| `BSKY_APP_PASSWORD` | — | No | Bluesky app password (Settings → App Passwords) |
| `BSKY_BRIDGE_TIMELINE` | `true` | No | Bridge posts from Bluesky accounts you follow into your Nostr feed. Set to `false` to receive only interactions targeting you (replies, likes, reposts). |
| `BSKY_PDS_URL` | `https://bsky.social` | No | Custom PDS endpoint. Only needed for third-party PDS accounts or did:web identities. |
| `EXTERNAL_BASE_URL` | `https://njump.me` | No | Base URL for Nostr links (used in truncated Bluesky posts). **Admin UI.** |
| `ZAP_PUBKEY` | — | No | Hex pubkey for Lightning zap split recipient. **Admin UI.** |
| `ZAP_SPLIT` | `0.1` | No | Zap split percentage (0–1). **Admin UI.** |
| `WEB_ADMIN` | — | No | Password for the web admin UI at `/web` (HTTP Basic Auth). Omit to disable entirely. |
| `SHOW_SOURCE_LINK` | `false` | No | Append the original post URL (`🔗`) at the bottom of bridged notes. **Admin UI** — takes effect immediately for new posts. |
| `RESYNC_INTERVAL` | `24h` | No | How often bridged AP actor profiles are re-fetched and re-published as kind-0 events. |
| `AP_CACHE_TTL` | `1h` | No | TTL for the AP object and WebFinger in-memory caches. |
| `BSKY_POLL_INTERVAL` | `30s` | No | How often the Bluesky notification and timeline poller runs. |
| `AP_FEDERATION_CONCURRENCY` | `10` | No | Max concurrent outbound ActivityPub HTTP delivery requests. |
| `RELAY_CB_THRESHOLD` | `3` | No | Consecutive relay publish failures before the circuit breaker opens (opens for 5 min, then auto-retries). |

---

## Credits

Inspired by [Mostr](https://gitlab.com/soapbox-pub/mostr) by Soapbox.

---

## License

GNU Affero General Public License v3.0 (AGPL-3.0)


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

| ActivityPub | Nostr Kind | Notes |
|---|---|---|
| `Create(Note)` | `1` | Text posts |
| `Announce` | `6` | Reposts |
| `Update(Actor)` | `0` | Profile updates |
| `Like` | `7` (`+`) | Reactions |
| `EmojiReact` | `7` (emoji) | Emoji reactions |
| `Delete` | `5` | Deletions |

### Signing

- **Your posts** (Nostr → AP) are signed with your real Nostr private key.
- **Remote AP actors** (AP → Nostr) get deterministic derived keys: `SHA-256(yourPrivKey + ":" + apActorID)`. No extra keys are stored.

### Identity

- Your AP handle: `@<NOSTR_USERNAME>@your-domain.com`
- Your AP actor URL: `https://your-domain.com/users/<NOSTR_USERNAME>`
- Your Nostr events become AP objects at: `https://your-domain.com/objects/<event-id>`

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
| Kind 1 (note) | Creates a post |
| Kind 5 (deletion) | Deletes the bridged post |
| Kind 6 (repost) | Reposts (if the original was bridged) |
| Kind 7 `+` (like) | Likes (if the original was bridged) |

| Bluesky notification | Nostr event |
|---|---|
| Like on your post | Kind 7 reaction |
| Repost of your post | Kind 6 repost |
| Reply to your post | Threaded Kind 1 reply (signed with a derived key for the Bluesky author), or NIP-04 DM if the parent post isn't bridged |
| Mention / quote | NIP-04 DM notification to yourself |
| New follower | NIP-04 DM notification to yourself |

**Notes:**
- Long Nostr posts (> 300 characters) are truncated and a link to the full post on njump.me is appended.
- Bluesky is polled every 30 seconds for new notifications.
- The bridge stores AT URIs in the same database table as ActivityPub IDs, so likes/reposts/deletes and reply threading can be correctly linked.
- Replying from Nostr to a bridged Bluesky reply will thread correctly back into the Bluesky conversation.

---

## Web admin UI (optional)

Set `WEB_ADMIN=<password>` to enable a dashboard at `https://your-domain.com/web`. It's protected by HTTP Basic Auth (any username, the password you set).

### What's on the dashboard

| Section | What it shows |
|---|---|
| **Node** | Domain, username, npub (with copy button), Bluesky status, uptime. Quick links to AP profile, Nostr profile (njump), WebFinger. |
| **Configured Relays** | All relays from `NOSTR_RELAY`. |
| **Bridge Activity** | Per-bridge stat panels — Nostr (relay count), Fediverse (followers, known actors, bridged objects), Bluesky (status, bridged objects, last sync time), Total. |
| **Fediverse Followers** | List of everyone following you on the Fediverse, shown as `@user@domain`. |
| **Import Fediverse Following** | Paste Fediverse handles (`user@domain.tld`, one per line). klistr resolves them via WebFinger, derives their Nostr pubkeys, fetches your current kind-3 from the relay to preserve existing follows, and publishes a merged kind-3 contact-list event. The bridge then sends ActivityPub Follow activities automatically. |
| **Actions** | Force an immediate Bluesky notification poll; refresh stats. |
| **Log** | Last 500 log lines from the ring buffer. Click **Refresh** to update. Filter by level (All / Debug / Info / Warn / Error). |

---

## Configuration reference

| Variable | Default | Required | Description |
|---|---|---|---|
| `LOCAL_DOMAIN` | `http://localhost:8000` | **Yes** | Your public domain (HTTPS in production) |
| `NOSTR_PRIVATE_KEY` | — | **Yes** | Your Nostr private key in hex |
| `NOSTR_USERNAME` | first 8 chars of pubkey | No | Your handle on this bridge (e.g. `alice`) |
| `NOSTR_DISPLAY_NAME` | value of `NOSTR_USERNAME` | No | Display name shown on the Fediverse |
| `NOSTR_SUMMARY` | — | No | Bio / profile description |
| `NOSTR_PICTURE` | — | No | Avatar image URL |
| `NOSTR_BANNER` | — | No | Banner/header image URL |
| `NOSTR_RELAY` | `wss://relay.mostr.pub` | No | Nostr relays, comma-separated. First entry is used as the relay hint in event tags. |
| `DATABASE_URL` | `klistr.db` | No | SQLite file path or `postgres://...` URL |
| `PORT` | `8000` | No | HTTP server port |
| `SIGN_FETCH` | `true` | No | Sign outbound HTTP requests (recommended) |
| `LOG_LEVEL` | `info` | No | `info` or `debug` |
| `BSKY_IDENTIFIER` | — | No | Bluesky handle or DID (enables Bluesky bridge) |
| `BSKY_APP_PASSWORD` | — | No | Bluesky app password (Settings → App Passwords) |
| `EXTERNAL_BASE_URL` | `https://njump.me` | No | Base URL for Nostr links (used in truncated Bluesky posts) |
| `WEB_ADMIN` | — | No | Password for the web admin UI at `/web` (HTTP Basic Auth). Omit to disable entirely. |

---

## Credits

Inspired by [Mostr](https://gitlab.com/soapbox-pub/mostr) by Soapbox.

---

## License

GNU Affero General Public License v3.0 (AGPL-3.0)


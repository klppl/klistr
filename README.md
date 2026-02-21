# klistr

A **personal bridge** between [Nostr](https://nostr.com/) and the [Fediverse](https://en.wikipedia.org/wiki/Fediverse) (Mastodon, Misskey, Pixelfed, etc.).

You run one instance, for yourself. Your Nostr posts appear on the Fediverse. Fediverse users can follow you. It's your identity on both networks, simultaneously.

---

## What it does (technical)

klistr is an **ActivityPub server** and a **Nostr client** running in a single process.

```
Your Nostr key → relay subscription (author filter)
                       ↓
              nostr.Handler
                       ↓
         ap.Federator → POST to AP inboxes
                            (your Fediverse followers)

Fediverse user → POST /inbox
                       ↓
              ap.APHandler
                       ↓
         Nostr Publisher → relay
                 (signed with your real key or a derived key)
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

## Configuration reference

| Variable | Default | Required | Description |
|---|---|---|---|
| `LOCAL_DOMAIN` | `http://localhost:8000` | **Yes** | Your public domain (HTTPS in production) |
| `NOSTR_PRIVATE_KEY` | — | **Yes** | Your Nostr private key in hex |
| `NOSTR_USERNAME` | first 8 chars of pubkey | No | Your handle on this bridge (e.g. `alice`) |
| `NOSTR_RELAY` | `wss://relay.mostr.pub` | No | Primary Nostr relay |
| `POOL_READ_RELAYS` | — | No | Extra read relays, comma-separated |
| `POOL_WRITE_RELAYS` | — | No | Extra write relays, comma-separated |
| `DATABASE_URL` | `klistr.db` | No | SQLite file path or `postgres://...` URL |
| `PORT` | `8000` | No | HTTP server port |
| `SIGN_FETCH` | `true` | No | Sign outbound HTTP requests (recommended) |
| `LOG_LEVEL` | `info` | No | `info` or `debug` |

---

## License

GNU Affero General Public License v3.0 (AGPL-3.0)


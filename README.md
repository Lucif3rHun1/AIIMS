# AIIMS Appointment Bot

Telegram-notified bot that books hospital (AIIMS) appointments via ABDM. Runs as a long-lived service that books daily at a target time, persists live appointments to a SQLite-backed landing page, and deploys on EasyPanel.

## Prerequisites

- **Go 1.25+** — `go version`
- A Telegram bot token from [@BotFather](https://t.me/BotFather)
- ABDM credentials (auth token, refresh token, HIP ID, phone)

## Setup

`config.json` is **gitignored** — it holds your secrets and is never committed.

Create `config.json` in the project root (same dir as the `bot` binary):

```json
{
  "telegram_bot_token": "<BOT_TOKEN_FROM_BOTFATHER>",
  "owner_id": 123456789,
  "broadcast_chat_id": -1001234567890,
  "timezone": "Asia/Kolkata",
  "phone_number": "<YOUR_PHONE>",
  "auth_token": "<ABDM_AUTH_TOKEN>",
  "refresh_token": "<ABDM_REFRESH_TOKEN>",
  "hip_id": "<HIP_ID>",
  "active_account_id": "<PHONE_KEY>",
  "accounts": {
    "<PHONE_KEY>": {
      "auth_token": "<ABDM_AUTH_TOKEN>",
      "refresh_token": "<ABDM_REFRESH_TOKEN>"
    }
  }
}
```

`owner_id` and `broadcast_chat_id` are integers, not strings — replace the example
numbers above with your own. Group/channel IDs are negative.

| Field | Required | Notes |
|---|---|---|
| `telegram_bot_token` | ✅ | Bot token from BotFather |
| `owner_id` | ✅ | Your Telegram user ID (int) |
| `broadcast_chat_id` | — | Chat/group ID for run summaries (int, negative for groups) |
| `timezone` | — | Defaults to `Asia/Kolkata` |
| `phone_number` | — | Account phone |
| `auth_token` / `refresh_token` | — | ABDM session credentials |
| `hip_id` | — | Health Information Provider ID |
| `accounts` | — | Per-account credentials keyed by phone |

`config.json` is **not** static configuration: the bot rewrites it on every token
refresh, patient selection and login. Wherever you deploy it, it must be writable
and it must live on storage that survives a restart.

Set `AIIMS_MASTER_KEY` (base64, 32 bytes) so the tokens in `config.json` are stored
encrypted; without it they are written as plaintext:

```bash
openssl rand -base64 32
```

Keep that key — rotating it makes already-encrypted tokens unreadable.

## Build

```bash
go build -o bot ./cmd/bot
```

## Run locally (macOS, supervised by launchd)

`./bot` in a terminal is fine for a quick foreground test, but nothing restarts it
when it dies — and a crash at 05:59 silently loses the booking window. On macOS the
supervisor is launchd; `com.aiims.bot.plist` in this repo is the agent template.

It ships with placeholders, because an agent needs the absolute path of *your*
checkout. Fill them in and install it:

```bash
go build -o bot ./cmd/bot

export AIIMS_MASTER_KEY='<your existing key>'   # first time: openssl rand -base64 32
sed -e "s|__REPO__|$PWD|g" \
    -e "s|__AIIMS_MASTER_KEY__|$AIIMS_MASTER_KEY|" \
    com.aiims.bot.plist > ~/Library/LaunchAgents/com.aiims.bot.plist

launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.aiims.bot.plist
```

`KeepAlive` means launchd restarts the bot whenever it exits, and `RunAtLoad`
starts it at login. Day-to-day control:

```bash
launchctl kickstart -k gui/$(id -u)/com.aiims.bot   # restart now (after a rebuild)
launchctl print gui/$(id -u)/com.aiims.bot          # status, last exit code
launchctl bootout gui/$(id -u)/com.aiims.bot        # stop for real
```

`launchctl bootout` is the only way to stop it — killing the process just makes
launchd start it again.

Logs, all in the repo root:

- `bot.log` — the bot's own slog output
- `bot_stdout.log` — same, as launchd captures it
- `bot_stderr.log` — **Go runtime panic traces land here and nowhere else.** The bot
  redirects slog, not stderr, which is why past crashes left no stack trace.

The installed copy under `~/Library/LaunchAgents/` contains your master key. It is
outside the repo, so it is not committed — do not copy it back in.

The old `watchdog.sh` + cron model is gone: `KeepAlive` is the OS doing the same job.

## Deployment (EasyPanel)

EasyPanel hosts the bot as a Docker container with restart-on-crash and persistent
state. Build artifacts live in this repo: `Dockerfile`, `easypanel.yml`, `.dockerignore`.

The container's working directory **is** the `bot-data` volume (`/app/data`). Every
file the bot writes — `config.json`, `bot_state.json`, `appointments.db`, `bot.log` —
is a path relative to that directory, so all of it persists across redeploys.

### One-time setup

1. In EasyPanel, create a project and add a service from this repo (it auto-detects `easypanel.yml`).
2. Create the persistent volume `bot-data` (defined in `easypanel.yml`), mounted at `/app/data`.
3. Put `config.json` **on that volume** at `/app/data/config.json` — via EasyPanel's
   volume file browser, or `docker cp config.json <container>:/app/data/config.json`.
   Do not use a read-only file mount for it: the bot saves it by writing a temp file
   and renaming over it, which fails against a bind-mounted single file whether or
   not the mount is writable.
4. In the service's **Environment** tab, set `AIIMS_MASTER_KEY` to the same base64 key you use locally.
5. Optional: set `broadcast_chat_id` in `config.json` to receive daily run summaries.
6. Deploy. `restart: unless-stopped` brings the container back after a crash, and the
   healthcheck (`/app/bot -healthcheck`, which polls the bot's own `/healthz`) reports
   whether it is merely alive or actually still processing updates.

## Security

- **Never commit `config.json`.** It is gitignored for this reason.
- `bot_state.json` and `appointments.db*` are gitignored too: both contain account IDs, which are phone numbers.
- If a token is ever committed, **revoke it at [@BotFather](https://t.me/BotFather)** and rotate — removing it from git history does not undo exposure in clones or remote history.
- Keep `owner_id` / `broadcast_chat_id` private; they identify your Telegram accounts.
  Earlier revisions of this README published the real values. They are still in git
  history and, unlike a token, a Telegram ID cannot be revoked or rotated.

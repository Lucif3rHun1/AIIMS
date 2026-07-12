# AIIMS Appointment Bot

Telegram-notified bot that books hospital (AIIMS) appointments via ABDM. It runs a daily booking routine and a watchdog that alerts you on Telegram if the run fails.

## Prerequisites

- **Go 1.24+** — `go version`
- **python3** — used by `watchdog.sh` to read `config.json`
- A Telegram bot token from [@BotFather](https://t.me/BotFather)
- ABDM credentials (auth token, refresh token, HIP ID, phone)

## Setup

`config.json` is **gitignored** — it holds your secrets and is never committed.

Create `config.json` in the project root (same dir as `watchdog.sh`):

```json
{
  "telegram_bot_token": "<BOT_TOKEN_FROM_BOTFATHER>",
  "owner_id": 5016416878,
  "broadcast_chat_id": -5297606084,
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

| Field | Required | Notes |
|---|---|---|
| `telegram_bot_token` | ✅ | Bot token from BotFather |
| `owner_id` | ✅ | Your Telegram user ID (int) |
| `broadcast_chat_id` | — | Chat/group ID for watchdog success summaries (int, negative for groups) |
| `timezone` | — | Defaults to `Asia/Kolkata` |
| `phone_number` | — | Account phone |
| `auth_token` / `refresh_token` | — | ABDM session credentials |
| `hip_id` | — | Health Information Provider ID |
| `accounts` | — | Per-account credentials keyed by phone |

## Build

```bash
go build -o bot ./cmd/bot
```

## Run

```bash
./bot
```

The bot loads `config.json` from its working directory at startup.

## Watchdog (daily monitoring)

`./bot` is a **persistent service** — it stays running, handles Telegram updates, and self-schedules the daily booking at ~05:58 IST.

`watchdog.sh` keeps that service alive and verifies each day's run:
- if the bot process isn't running, it **starts** it (in the project dir, output → `bot.log`)
- waits for `bot.log` (the bot creates it on startup)
- sleeps until **05:58 IST**, then monitors `bot.log` for the run result
- on success → sends a summary to `broadcast_chat_id`
- on failure (no confirmation before 06:05) → sends a Telegram retry alert to `owner_id`

It reads `telegram_bot_token`, `owner_id`, and `broadcast_chat_id` from `config.json` (no secrets in the script). `python3` is required.

Schedule with cron (runs every day; the script self-aligns to 05:58):

```cron
58 5 * * * /bin/bash /path/to/AIIMS/watchdog.sh >> /path/to/AIIMS/cron.log 2>&1
```

Run once manually to test (skips the wait-to-05:58 so it runs immediately):

```bash
WATCHDOG_NOWAIT=1 bash watchdog.sh
```

## Security

- **Never commit `config.json`.** It is gitignored for this reason.
- If a token is ever committed, **revoke it at [@BotFather](https://t.me/BotFather)** and rotate — removing it from git history does not undo exposure in clones or remote history.
- Keep `owner_id` / `broadcast_chat_id` private; they identify your Telegram accounts.

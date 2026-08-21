# deadlines-api

Go backend I wrote for [Redloop](https://github.com/itbestyle/Deadline), my iOS deadline tracker on the App Store.

This is a small REST API in standard library Go: auth, per-user deadlines, PostgreSQL. No framework.

**iOS client:** https://github.com/itbestyle/Deadline

## What it does

- Register / login — bcrypt password hashes, JWT
- Firebase ID-token login (what the App Store build uses)
- Account deletion (App Store requirement)
- Deadline CRUD, user isolation, soft-delete
- Recurring due-date rollover
- `/health` for Cloud Run

Stack: Go, `net/http`, PostgreSQL (`lib/pq`), JWT, bcrypt, Docker.

## How to run

```bash
cp .env.example .env   # set DATABASE_URL and JWT_SECRET (32+ chars)
go run .
```

Listens on `PORT` or `8080`.

```bash
curl -s localhost:8080/health
```

## Environment

| Variable | Required | Purpose |
|---|---|---|
| `DATABASE_URL` | yes | Postgres |
| `JWT_SECRET` | yes | HMAC secret, min 32 chars |
| `FIREBASE_PROJECT_ID` | for Firebase login | Firebase project id |
| `EMAIL_VERIFY_BASE_URL` | only if you send your own verify emails | Public API origin |
| `RESEND_*` / `SMTP_*` | optional | Email (legacy path; the app uses Firebase) |

Secrets stay in the environment, not in this repo.

## Docker

```bash
docker build -t deadlines-api .
docker run --env-file .env -p 8080:8080 deadlines-api
```

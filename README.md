# deadlines-api

Go REST API for [Redloop](https://github.com/itbestyle/Deadline), an iOS deadline tracker.

Stack: Go `net/http`, PostgreSQL (`lib/pq`), JWT, bcrypt, Docker.

## Features

- Register / login with bcrypt password hashes and JWT (30 days)
- Email verification (Resend or SMTP)
- Firebase ID-token login as an alternative
- Account deletion (App Store requirement)
- Deadline CRUD with per-user isolation and soft-delete (`deleted_at`)
- Recurring due-date rollover
- Health check for Cloud Run / Render

## Run locally

```bash
cp .env.example .env
# fill DATABASE_URL and JWT_SECRET (32+ characters)

go run .
```

Server listens on `PORT` or `8080`.

```bash
curl -s localhost:8080/health
```

## Environment

| Variable | Required | Purpose |
|---|---|---|
| `DATABASE_URL` | yes | Postgres connection string |
| `JWT_SECRET` | yes | HMAC secret, min 32 chars |
| `EMAIL_VERIFY_BASE_URL` | for email links | Public origin of this API |
| `FIREBASE_PROJECT_ID` | for Firebase login | Firebase project id |
| `RESEND_API_KEY` / `RESEND_FROM` | optional | Verification email |
| `SMTP_*` | optional | SMTP fallback |

Secrets stay in the environment. They are not in this repository.

## Docker

```bash
docker build -t deadlines-api .
docker run --env-file .env -p 8080:8080 deadlines-api
```

## iOS client

https://github.com/itbestyle/Deadline

# Render + Neon: пост-миграционный чеклист

## 1) Бэкап Neon сейчас

```bash
cd /path/to/deadlines-api
bash scripts/backup_neon.sh 'postgresql://USER:PASS@HOST/neondb?sslmode=require&channel_binding=require'
```

## 2) Убрать лишние переменные в Render

Если используете **Resend**, оставьте:

- `RESEND_API_KEY`
- `RESEND_FROM`

И удалите SMTP-переменные (чтобы не путаться):

- `SMTP_HOST`
- `SMTP_PORT`
- `SMTP_USER`
- `SMTP_PASS`
- `SMTP_FROM`
- `SMTP_TLS_MODE`

Если используете **SMTP**, наоборот удалите Resend-переменные.

Обязательный минимум для API:

- `DATABASE_URL`
- `JWT_SECRET`
- `EMAIL_VERIFY_BASE_URL`

Опционально:

- `FIREBASE_PROJECT_ID`

## 3) Ротация секретов

Сразу после миграции:

1. Сгенерируйте новый пароль БД в Neon (Rotate credentials).
2. Обновите `DATABASE_URL` в Render.
3. Сгенерируйте новый `JWT_SECRET`:
   ```bash
   openssl rand -base64 48
   ```
4. Обновите `JWT_SECRET` в Render.
5. Redeploy сервиса.

## 4) Проверка, что секреты не в репозитории

Локально:

```bash
cd /path/to/deadlines-api
git grep -nE 'postgres://|postgresql://|npg_|RESEND_API_KEY=|SMTP_PASS=|JWT_SECRET=' -- . ':!*.md' ':!.env.example' || true
```

Ожидаемо: ничего с реальными значениями.

## 5) Операционная проверка после ротации

- `POST /auth/login`
- `GET /deadlines` с Bearer токеном
- `POST /auth/resend-verification` (если используете email)

## 6) Политика на будущее

- Не коммитить `.env`, `*.dump`, `*.sql`.
- Делать бэкап Neon перед крупными изменениями схемы.
- После смены БД делать logout/login в мобильном приложении.

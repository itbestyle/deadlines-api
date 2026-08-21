# Безопасная настройка приватного GitHub-репозитория и доступа с сервера

## Важно для Render

Если сервис размещён в **Render**, то `systemd`, `Deploy Key` на вашем VPS и ручной `git pull` обычно **не нужны**.

Для Render используйте такой путь:

1. Подключите приватный репозиторий GitHub в Render (Render запросит доступ к репозиториям через GitHub App).
2. В Render → Service → **Environment** задайте переменные:
   - `DATABASE_URL`
   - `JWT_SECRET` (минимум 32 символа)
   - при необходимости: `RESEND_API_KEY`, `RESEND_FROM`, SMTP-переменные.
3. В Render включите авто-деплой от нужной ветки (`main` или другой).
4. Никогда не храните реальные секреты в репозитории и в `.env.example`.

Ниже в этом файле шаги 2-4 и 8-9 относятся к **self-hosted Linux/VPS** сценарию.

## 1) Репозиторий можно держать публичным

Секреты живут только в переменных окружения (Render / Cloud Run / `.env` на сервере), не в git. Публичный репозиторий для портфолио безопасен, пока `.env`, дампы БД и ключи не попадают в коммиты.

## 2) Выдайте серверу доступ только на чтение (рекомендуется: Deploy Key)

На сервере:

```bash
cd /path/to/deadlines-api
bash scripts/generate_deploy_key.sh
```

Скопируйте выведенный публичный ключ и добавьте его в GitHub:
**Repository -> Settings -> Deploy keys -> Add deploy key**

- Title: `deadlines-api-server`
- Key: вставьте `.pub` ключ
- **Allow write access: OFF**

## 3) Настройте `git remote` на SSH

```bash
git remote set-url origin git@github.com:<OWNER>/<REPO>.git
```

## 4) Безопасно подтягивайте обновления на сервере

```bash
cd /path/to/deadlines-api
bash scripts/pull_private_repo.sh /path/to/deadlines-api ~/.ssh/deadlines-api/github_deploy_key main
```

## 5) Секреты окружения

Создайте `.env` на основе `.env.example` и заполните значения:

- `DATABASE_URL` (обязателен)
- `JWT_SECRET` (обязателен, минимум 32 символа)

Никогда не коммитьте `.env` и приватные ключи.

## 6) Доступ через API-токен (если нужен)

Если сервер читает файлы приватного репозитория через GitHub API, используйте отдельный read-only токен (`Contents: Read-only`) и храните его в переменных окружения (например, `GITHUB_TOKEN`).

## 7) Ротация ключей и действия при инциденте

- Ротируйте deploy key/токен каждые 60-90 дней.
- Немедленно отзывайте доступ при любом подозрении на утечку.
- Используйте отдельные учётные данные для каждого сервера.

## 8) Настройка `systemd`-сервиса (Linux)

> Пропустите этот раздел, если деплой в Render.

Ниже предполагается, что приложение развёрнуто в `/opt/deadlines-api` под пользователем `deadlines`.

```bash
sudo useradd --system --create-home --shell /usr/sbin/nologin deadlines || true
sudo mkdir -p /opt/deadlines-api
sudo chown -R deadlines:deadlines /opt/deadlines-api
```

Скопируйте unit-файл сервиса:

```bash
cd /opt/deadlines-api
sudo cp deploy/systemd/deadlines-api.service /etc/systemd/system/deadlines-api.service
sudo systemctl daemon-reload
sudo systemctl enable deadlines-api
```

## 9) Деплой одной командой (pull + build + restart)

> Пропустите этот раздел, если деплой в Render.

```bash
cd /opt/deadlines-api
bash scripts/deploy_restart.sh /opt/deadlines-api ~/.ssh/deadlines-api/github_deploy_key main deadlines-api
```

Быстрая проверка логов:

```bash
sudo journalctl -u deadlines-api -n 100 --no-pager
```

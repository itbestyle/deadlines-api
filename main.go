package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

var db *sql.DB
var jwtSecret []byte

var firebaseKeysCache = struct {
	mu       sync.RWMutex
	keys     map[string]string
	fetched  time.Time
	cacheTTL time.Duration
}{
	keys:     map[string]string{},
	cacheTTL: time.Hour,
}

type User struct {
	ID            int    `json:"id"`
	Email         string `json:"email"`
	Password      string `json:"password,omitempty"`
	EmailVerified bool   `json:"email_verified,omitempty"`
}

type Deadline struct {
	ID           string     `json:"id"`
	Title        string     `json:"title"`
	Subject      string     `json:"subject"`
	DueDate      string     `json:"due_date"`
	EffectiveDue string     `json:"effective_due_date,omitempty"`
	Status       string     `json:"status"`
	Tags         []string   `json:"tags"`
	RepeatType   string     `json:"repeat_type"`   // none, daily, weekly, monthly, yearly
	Notes        string     `json:"notes"`         // описание/заметки
	ReminderTime string     `json:"reminder_time"` // none, 1hour, 1day, 1week
	DeletedAt    *time.Time `json:"deleted_at,omitempty"`
	UserID       int        `json:"-"`
}

type AuthResponse struct {
	Token                string `json:"token,omitempty"`
	Email                string `json:"email"`
	RequiresVerification bool   `json:"requires_verification,omitempty"`
	Message              string `json:"message,omitempty"`
}

type VerificationRequest struct {
	Email string `json:"email"`
}

type FirebaseLoginRequest struct {
	IDToken string `json:"idToken"`
}

type contextKey string

const userIDKey contextKey = "userID"

var ErrEmailNotVerified = errors.New("email not verified")

// ===== Database =====
func initDB() error {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		log.Fatal("DATABASE_URL не установлен")
	}
	connStr = normalizeDatabaseURL(connStr)

	jwtSecretValue := strings.TrimSpace(os.Getenv("JWT_SECRET"))
	if jwtSecretValue == "" {
		return errors.New("JWT_SECRET не установлен")
	}
	if len(jwtSecretValue) < 32 {
		return fmt.Errorf("JWT_SECRET слишком короткий: минимум 32 символа")
	}
	jwtSecret = []byte(jwtSecretValue)

	var err error
	db, err = sql.Open("postgres", connStr)
	if err != nil {
		return err
	}

	if err = db.Ping(); err != nil {
		return err
	}

	// Создаём таблицы
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS public.users (
			id SERIAL PRIMARY KEY,
			email TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			email_verified BOOLEAN DEFAULT FALSE,
			email_verification_token TEXT,
			email_verification_expires_at TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return err
	}

	db.Exec(`ALTER TABLE public.users ADD COLUMN IF NOT EXISTS email_verified BOOLEAN DEFAULT FALSE`)
	db.Exec(`ALTER TABLE public.users ADD COLUMN IF NOT EXISTS email_verification_token TEXT`)
	db.Exec(`ALTER TABLE public.users ADD COLUMN IF NOT EXISTS email_verification_expires_at TIMESTAMP`)

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS public.deadlines (
			id SERIAL PRIMARY KEY,
			user_id INTEGER REFERENCES public.users(id) ON DELETE CASCADE,
			title TEXT NOT NULL,
			subject TEXT NOT NULL,
			due_date TEXT NOT NULL,
			status TEXT NOT NULL,
			tags TEXT[] DEFAULT '{}'
		)
	`)
	if err != nil {
		return err
	}

	// Добавим колонку user_id если её нет (для миграции)
	db.Exec(`ALTER TABLE public.deadlines ADD COLUMN IF NOT EXISTS user_id INTEGER REFERENCES public.users(id) ON DELETE CASCADE`)

	// Добавим колонку repeat_type
	db.Exec(`ALTER TABLE public.deadlines ADD COLUMN IF NOT EXISTS repeat_type TEXT DEFAULT 'none'`)

	// Добавим колонки notes и reminder_time
	db.Exec(`ALTER TABLE public.deadlines ADD COLUMN IF NOT EXISTS notes TEXT DEFAULT ''`)
	db.Exec(`ALTER TABLE public.deadlines ADD COLUMN IF NOT EXISTS reminder_time TEXT DEFAULT '1day'`)

	// Добавим колонку deleted_at
	db.Exec(`ALTER TABLE public.deadlines ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMP`)

	return nil
}

func normalizeDatabaseURL(connStr string) string {
	trimmed := strings.TrimSpace(connStr)
	parsedURL, err := url.Parse(trimmed)
	if err != nil {
		return trimmed
	}
	if parsedURL.Scheme != "postgres" && parsedURL.Scheme != "postgresql" {
		return trimmed
	}

	query := parsedURL.Query()
	query.Del("options")
	query.Del("search_path")
	parsedURL.RawQuery = query.Encode()

	return parsedURL.String()
}

// ===== Auth Functions =====
func registerUser(email, password string) (*User, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}

	verificationToken, err := generateVerificationToken()
	if err != nil {
		return nil, err
	}
	expiresAt := time.Now().Add(24 * time.Hour)

	var id int
	err = db.QueryRow(
		"INSERT INTO public.users (email, password_hash, email_verified, email_verification_token, email_verification_expires_at) VALUES ($1, $2, FALSE, $3, $4) RETURNING id",
		email, string(hash), verificationToken, expiresAt,
	).Scan(&id)
	if err != nil {
		return nil, err
	}

	if err := sendVerificationEmail(email, verificationToken); err != nil {
		log.Printf("Не удалось отправить verification email для %s: %v", email, err)
	}

	return &User{ID: id, Email: email, EmailVerified: false}, nil
}

func loginUser(email, password string) (*User, error) {
	var user User
	var hash string
	err := db.QueryRow(
		"SELECT id, email, password_hash, COALESCE(email_verified, FALSE) FROM public.users WHERE email = $1",
		email,
	).Scan(&user.ID, &user.Email, &hash, &user.EmailVerified)
	if err != nil {
		return nil, err
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err != nil {
		return nil, err
	}

	if !user.EmailVerified {
		return nil, ErrEmailNotVerified
	}

	return &user, nil
}

func generateVerificationToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func sendVerificationEmail(email, token string) error {
	baseURL := strings.TrimRight(os.Getenv("EMAIL_VERIFY_BASE_URL"), "/")
	if baseURL == "" {
		baseURL = "http://localhost:8080"
	}
	verifyURL := fmt.Sprintf("%s/auth/verify?token=%s", baseURL, token)

	if resendKey := strings.TrimSpace(os.Getenv("RESEND_API_KEY")); resendKey != "" {
		resendFrom := strings.TrimSpace(os.Getenv("RESEND_FROM"))
		if resendFrom == "" {
			resendFrom = "Deadlines <onboarding@resend.dev>"
		}
		return sendViaResend(resendKey, resendFrom, email, verifyURL)
	}

	smtpHost := os.Getenv("SMTP_HOST")
	smtpPort := os.Getenv("SMTP_PORT")
	smtpUser := os.Getenv("SMTP_USER")
	smtpPass := os.Getenv("SMTP_PASS")
	mailFrom := os.Getenv("SMTP_FROM")
	smtpMode := strings.ToLower(strings.TrimSpace(os.Getenv("SMTP_TLS_MODE"))) // auto | ssl | starttls

	if smtpPort == "" {
		if strings.Contains(strings.ToLower(smtpHost), "gmail.com") {
			smtpPort = "465"
		} else {
			smtpPort = "587"
		}
	}
	if smtpMode == "" {
		smtpMode = "auto"
	}
	if mailFrom == "" {
		mailFrom = smtpUser
	}

	if smtpHost == "" || smtpUser == "" || smtpPass == "" || mailFrom == "" {
		log.Printf("SMTP не настроен. Ссылка подтверждения для %s: %s", email, verifyURL)
		return nil
	}

	subject := "Подтверждение почты Deadlines"
	body := "Подтвердите вашу почту для входа в Deadlines:\n\n" + verifyURL + "\n\nСсылка действует 24 часа."

	message := strings.Join([]string{
		"From: " + mailFrom,
		"To: " + email,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		body,
	}, "\r\n")

	return sendViaSMTP(smtpHost, smtpPort, smtpMode, smtpUser, smtpPass, mailFrom, []string{email}, []byte(message))
}

func sendViaSMTP(host, port, mode, user, pass, from string, to []string, msg []byte) error {
	addr := net.JoinHostPort(host, port)
	auth := smtp.PlainAuth("", user, pass, host)

	useSSL := mode == "ssl" || (mode == "auto" && port == "465")
	if useSSL {
		dialer := &net.Dialer{Timeout: 8 * time.Second}
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: host})
		if err != nil {
			return err
		}
		defer conn.Close()

		client, err := smtp.NewClient(conn, host)
		if err != nil {
			return err
		}
		defer client.Quit()

		if err := client.Auth(auth); err != nil {
			return err
		}
		if err := client.Mail(from); err != nil {
			return err
		}
		for _, recipient := range to {
			if err := client.Rcpt(recipient); err != nil {
				return err
			}
		}
		wc, err := client.Data()
		if err != nil {
			return err
		}
		if _, err := wc.Write(msg); err != nil {
			_ = wc.Close()
			return err
		}
		return wc.Close()
	}

	result := make(chan error, 1)
	go func() {
		result <- smtp.SendMail(addr, auth, from, to, msg)
	}()

	select {
	case err := <-result:
		return err
	case <-time.After(8 * time.Second):
		return fmt.Errorf("smtp timeout")
	}
}

func sendViaResend(apiKey, from, to, verifyURL string) error {
	body := map[string]any{
		"from":    from,
		"to":      []string{to},
		"subject": "Подтверждение почты Deadlines",
		"text":    "Подтвердите вашу почту для входа в Deadlines:\n\n" + verifyURL + "\n\nСсылка действует 24 часа.",
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1200))
		message := strings.TrimSpace(string(bodyBytes))
		if message == "" {
			return fmt.Errorf("resend http %d", resp.StatusCode)
		}
		return fmt.Errorf("resend http %d: %s", resp.StatusCode, message)
	}

	return nil
}

func markUserVerifiedByToken(token string) (string, error) {
	if token == "" {
		return "", sql.ErrNoRows
	}

	var email string
	err := db.QueryRow(
		`UPDATE public.users
		 SET email_verified = TRUE,
		     email_verification_token = NULL,
		     email_verification_expires_at = NULL
		 WHERE email_verification_token = $1
		   AND email_verification_expires_at IS NOT NULL
		   AND email_verification_expires_at > NOW()
		 RETURNING email`,
		token,
	).Scan(&email)

	if err != nil {
		return "", err
	}

	return email, nil
}

func resendVerificationByEmail(email string) error {
	var userID int
	var verified bool
	err := db.QueryRow(
		"SELECT id, COALESCE(email_verified, FALSE) FROM public.users WHERE email = $1",
		email,
	).Scan(&userID, &verified)

	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if verified {
		return nil
	}

	token, err := generateVerificationToken()
	if err != nil {
		return err
	}
	expiresAt := time.Now().Add(24 * time.Hour)

	_, err = db.Exec(
		"UPDATE public.users SET email_verification_token = $1, email_verification_expires_at = $2 WHERE id = $3",
		token, expiresAt, userID,
	)
	if err != nil {
		return err
	}

	if err := sendVerificationEmail(email, token); err != nil {
		return fmt.Errorf("smtp send failed: %w", err)
	}
	return nil
}

func userFacingSMTPError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	lower := strings.ToLower(msg)

	switch {
	case strings.Contains(lower, "resend http"):
		if strings.Contains(lower, "testing emails") || strings.Contains(lower, "only send") {
			return "Resend test-mode restriction: onboarding@resend.dev может отправлять только на email владельца аккаунта Resend. Либо тестируйте на эту почту, либо добавьте свой домен/sender в Resend."
		}
		if strings.Contains(lower, "invalid_api_key") || strings.Contains(lower, "unauthorized") {
			return "Resend API key error. Проверьте RESEND_API_KEY в Render."
		}
		if strings.Contains(lower, "from") || strings.Contains(lower, "sender") {
			return "Resend sender error. Проверьте RESEND_FROM (например, Deadlines <onboarding@resend.dev> или verified sender вашего домена)."
		}
		raw := msg
		if len(raw) > 220 {
			raw = raw[:220] + "..."
		}
		return "Resend API error: " + raw
	case strings.Contains(lower, "535") || strings.Contains(lower, "username and password not accepted"):
		return "SMTP auth error (535). Проверьте SMTP_USER и App Password (SMTP_PASS) без пробелов."
	case strings.Contains(lower, "timeout"):
		return "SMTP timeout. Проверьте SMTP_HOST/SMTP_PORT и доступность smtp.gmail.com:587 на хостинге."
	case strings.Contains(lower, "from") && strings.Contains(lower, "not"):
		return "SMTP sender error. Убедитесь, что SMTP_FROM совпадает с SMTP_USER."
	default:
		return "SMTP error: " + msg
	}
}

func generateToken(userID int) (string, error) {
	claims := jwt.MapClaims{
		"user_id": userID,
		"exp":     time.Now().Add(30 * 24 * time.Hour).Unix(), // 30 дней
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(jwtSecret)
}

func firebaseProjectID() string {
	return strings.TrimSpace(os.Getenv("FIREBASE_PROJECT_ID"))
}

func userIDFromLegacyJWT(tokenString string) (int, error) {
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return jwtSecret, nil
	})
	if err != nil || !token.Valid {
		return 0, errors.New("invalid legacy token")
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return 0, errors.New("invalid legacy token claims")
	}

	userIDValue, ok := claims["user_id"].(float64)
	if !ok {
		return 0, errors.New("missing user_id")
	}

	return int(userIDValue), nil
}

func fetchFirebasePublicKeys() (map[string]string, error) {
	firebaseKeysCache.mu.RLock()
	if len(firebaseKeysCache.keys) > 0 && time.Since(firebaseKeysCache.fetched) < firebaseKeysCache.cacheTTL {
		copied := make(map[string]string, len(firebaseKeysCache.keys))
		for k, v := range firebaseKeysCache.keys {
			copied[k] = v
		}
		firebaseKeysCache.mu.RUnlock()
		return copied, nil
	}
	firebaseKeysCache.mu.RUnlock()

	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get("https://www.googleapis.com/robot/v1/metadata/x509/securetoken@system.gserviceaccount.com")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("firebase cert fetch http %d", resp.StatusCode)
	}

	var keys map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		return nil, err
	}

	firebaseKeysCache.mu.Lock()
	firebaseKeysCache.keys = keys
	firebaseKeysCache.fetched = time.Now()
	firebaseKeysCache.mu.Unlock()

	return keys, nil
}

func boolClaim(claims jwt.MapClaims, key string) bool {
	value, ok := claims[key]
	if !ok {
		return false
	}
	if b, ok := value.(bool); ok {
		return b
	}
	if s, ok := value.(string); ok {
		return strings.EqualFold(s, "true")
	}
	return false
}

func parseFirebaseIdentity(idToken string) (email string, emailVerified bool, firebaseUID string, err error) {
	projectID := firebaseProjectID()
	if projectID == "" {
		return "", false, "", errors.New("firebase auth is not configured")
	}

	keys, err := fetchFirebasePublicKeys()
	if err != nil {
		return "", false, "", err
	}

	token, err := jwt.Parse(idToken, func(token *jwt.Token) (interface{}, error) {
		if token.Method.Alg() != jwt.SigningMethodRS256.Alg() {
			return nil, fmt.Errorf("unexpected firebase token algorithm")
		}
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			return nil, fmt.Errorf("firebase token missing kid")
		}
		certPEM, ok := keys[kid]
		if !ok {
			return nil, fmt.Errorf("firebase certificate kid not found")
		}
		return jwt.ParseRSAPublicKeyFromPEM([]byte(certPEM))
	})
	if err != nil || !token.Valid {
		return "", false, "", errors.New("invalid firebase token")
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", false, "", errors.New("invalid firebase claims")
	}

	aud, _ := claims["aud"].(string)
	iss, _ := claims["iss"].(string)
	if aud != projectID || iss != "https://securetoken.google.com/"+projectID {
		return "", false, "", errors.New("firebase token audience/issuer mismatch")
	}

	sub, _ := claims["sub"].(string)
	if strings.TrimSpace(sub) == "" {
		return "", false, "", errors.New("firebase token missing subject")
	}

	email, _ = claims["email"].(string)
	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return "", false, "", errors.New("firebase token missing email")
	}

	emailVerified = boolClaim(claims, "email_verified")
	return email, emailVerified, sub, nil
}

func upsertUserForFirebase(email string, emailVerified bool) (int, bool, error) {
	var userID int
	var dbVerified bool
	err := db.QueryRow("SELECT id, COALESCE(email_verified, FALSE) FROM public.users WHERE email = $1", email).Scan(&userID, &dbVerified)
	if err == sql.ErrNoRows {
		err = db.QueryRow(
			"INSERT INTO public.users (email, password_hash, email_verified) VALUES ($1, $2, $3) RETURNING id",
			email, "firebase-auth", emailVerified,
		).Scan(&userID)
		if err != nil {
			return 0, false, err
		}
		return userID, emailVerified, nil
	}
	if err != nil {
		return 0, false, err
	}

	if emailVerified && !dbVerified {
		if _, err := db.Exec("UPDATE public.users SET email_verified = TRUE WHERE id = $1", userID); err != nil {
			return 0, false, err
		}
		dbVerified = true
	}

	return userID, dbVerified, nil
}

func resolveUserIDFromBearerToken(tokenString string) (int, int, string) {
	if userID, err := userIDFromLegacyJWT(tokenString); err == nil {
		verified, verifyErr := isUserVerified(userID)
		if verifyErr != nil {
			return 0, http.StatusInternalServerError, "Ошибка проверки пользователя"
		}
		if !verified {
			return 0, http.StatusForbidden, "Почта не подтверждена"
		}
		return userID, 0, ""
	}

	email, emailVerified, _, err := parseFirebaseIdentity(tokenString)
	if err != nil {
		return 0, http.StatusUnauthorized, "Неверный токен"
	}

	userID, dbVerified, err := upsertUserForFirebase(email, emailVerified)
	if err != nil {
		return 0, http.StatusInternalServerError, "Ошибка синхронизации пользователя"
	}
	if !dbVerified {
		return 0, http.StatusForbidden, "Почта не подтверждена"
	}

	return userID, 0, ""
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "Требуется авторизация", http.StatusUnauthorized)
			return
		}

		tokenString := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		if tokenString == "" {
			http.Error(w, "Требуется авторизация", http.StatusUnauthorized)
			return
		}

		userID, statusCode, message := resolveUserIDFromBearerToken(tokenString)
		if statusCode != 0 {
			http.Error(w, message, statusCode)
			return
		}

		ctx := context.WithValue(r.Context(), userIDKey, userID)
		next(w, r.WithContext(ctx))
	}
}

func isUserVerified(userID int) (bool, error) {
	var verified bool
	err := db.QueryRow("SELECT COALESCE(email_verified, FALSE) FROM public.users WHERE id = $1", userID).Scan(&verified)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return verified, nil
}

func getUserID(r *http.Request) int {
	if id, ok := r.Context().Value(userIDKey).(int); ok {
		return id
	}
	return 0
}

func getUserIDByEmail(email string) (int, error) {
	var userID int
	err := db.QueryRow("SELECT id FROM public.users WHERE email = $1", email).Scan(&userID)
	return userID, err
}

func deleteUserByID(userID int) error {
	result, err := db.Exec("DELETE FROM public.users WHERE id = $1", userID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func respondAccountDeleted(w http.ResponseWriter) {
	setHeaders(w)
	w.WriteHeader(http.StatusNoContent)
}

// ===== Deadline DB Functions =====
func getDeadlineByID(id string, userID int) (*Deadline, error) {
	var d Deadline
	var dbID int
	var tags []byte
	var deletedAt sql.NullTime
	err := db.QueryRow(
		"SELECT id, title, subject, due_date, status, tags, COALESCE(repeat_type, 'none'), COALESCE(notes, ''), COALESCE(reminder_time, '1day'), deleted_at FROM public.deadlines WHERE id = $1 AND user_id = $2",
		id, userID,
	).Scan(&dbID, &d.Title, &d.Subject, &d.DueDate, &d.Status, &tags, &d.RepeatType, &d.Notes, &d.ReminderTime, &deletedAt)
	if err != nil {
		return nil, err
	}
	d.ID = strconv.Itoa(dbID)
	d.Tags = parseTags(tags)
	if deletedAt.Valid {
		d.DeletedAt = &deletedAt.Time
	}
	return &d, nil
}

func nextDueDate(currentDate string, repeatType string) string {
	layout := "2006-01-02"
	outLayout := layout
	t, err := time.Parse(layout, currentDate)
	if err != nil {
		layoutWithTime := "2006-01-02 15:04"
		t, err = time.Parse(layoutWithTime, currentDate)
		if err != nil {
			return currentDate
		}
		outLayout = layoutWithTime
	}

	switch repeatType {
	case "daily":
		t = t.AddDate(0, 0, 1)
	case "weekly":
		t = t.AddDate(0, 0, 7)
	case "monthly":
		t = t.AddDate(0, 1, 0)
	case "yearly":
		t = t.AddDate(1, 0, 0)
	default:
		return currentDate
	}
	return t.Format(outLayout)
}

func parseDeadlineDate(currentDate string) (time.Time, bool, error) {
	dateOnlyLayout := "2006-01-02"
	dateTimeLayout := "2006-01-02 15:04"
	dateTimeSecondsLayout := "2006-01-02 15:04:05"

	if t, err := time.ParseInLocation(dateTimeSecondsLayout, currentDate, time.Local); err == nil {
		return t, true, nil
	}
	if t, err := time.ParseInLocation(dateTimeLayout, currentDate, time.Local); err == nil {
		return t, true, nil
	}
	if t, err := time.ParseInLocation(dateOnlyLayout, currentDate, time.Local); err == nil {
		return t, false, nil
	}

	return time.Time{}, false, fmt.Errorf("invalid due date format: %s", currentDate)
}

func computeEffectiveDueDate(currentDate, repeatType string, now time.Time) string {
	t, hasTime, err := parseDeadlineDate(currentDate)
	if err != nil {
		return currentDate
	}

	reference := now
	if !hasTime {
		reference = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	}

	if repeatType != "" && repeatType != "none" {
		for i := 0; i < 600 && t.Before(reference); i++ {
			switch repeatType {
			case "daily":
				t = t.AddDate(0, 0, 1)
			case "weekly":
				t = t.AddDate(0, 0, 7)
			case "monthly":
				t = t.AddDate(0, 1, 0)
			case "yearly":
				t = t.AddDate(1, 0, 0)
			default:
				i = 600
			}
		}
	}

	if !hasTime {
		t = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 0, 0, t.Location())
	}

	return t.Format("2006-01-02 15:04")
}

func getAllDeadlines(userID int) ([]Deadline, error) {
	if err := cleanupOldArchived(userID); err != nil {
		log.Printf("failed to cleanup archived deadlines: %v", err)
	}
	if err := ensureDeletedAtForArchived(userID); err != nil {
		log.Printf("failed to backfill deleted_at: %v", err)
	}

	rows, err := db.Query("SELECT id, title, subject, due_date, status, tags, COALESCE(repeat_type, 'none'), COALESCE(notes, ''), COALESCE(reminder_time, '1day'), deleted_at FROM public.deadlines WHERE user_id = $1 ORDER BY id", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deadlines []Deadline
	for rows.Next() {
		var d Deadline
		var id int
		var tags []byte
		var deletedAt sql.NullTime
		if err := rows.Scan(&id, &d.Title, &d.Subject, &d.DueDate, &d.Status, &tags, &d.RepeatType, &d.Notes, &d.ReminderTime, &deletedAt); err != nil {
			return nil, err
		}
		d.ID = strconv.Itoa(id)
		d.Tags = parseTags(tags)
		if deletedAt.Valid {
			d.DeletedAt = &deletedAt.Time
		}
		deadlines = append(deadlines, d)
	}
	return deadlines, rows.Err()
}

func parseTags(data []byte) []string {
	if len(data) < 2 {
		return []string{}
	}
	// PostgreSQL возвращает {tag1,tag2} формат
	s := string(data)
	s = strings.Trim(s, "{}")
	if s == "" {
		return []string{}
	}
	return strings.Split(s, ",")
}

func formatTags(tags []string) string {
	if len(tags) == 0 {
		return "{}"
	}
	return "{" + strings.Join(tags, ",") + "}"
}

func insertDeadline(d *Deadline, userID int) error {
	if d.RepeatType == "" {
		d.RepeatType = "none"
	}
	if d.ReminderTime == "" {
		d.ReminderTime = "1day"
	}
	normalizeDeletedAt(d)
	var id int
	err := db.QueryRow(
		"INSERT INTO public.deadlines (user_id, title, subject, due_date, status, tags, repeat_type, notes, reminder_time, deleted_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id",
		userID, d.Title, d.Subject, d.DueDate, d.Status, formatTags(d.Tags), d.RepeatType, d.Notes, d.ReminderTime, d.DeletedAt,
	).Scan(&id)
	if err != nil {
		return err
	}
	d.ID = strconv.Itoa(id)
	return nil
}

func updateDeadlineDB(id string, d Deadline, userID int) error {
	if d.RepeatType == "" {
		d.RepeatType = "none"
	}
	if d.ReminderTime == "" {
		d.ReminderTime = "1day"
	}
	result, err := db.Exec(
		"UPDATE public.deadlines SET title=$1, subject=$2, due_date=$3, status=$4, tags=$5, repeat_type=$6, notes=$7, reminder_time=$8, deleted_at = CASE WHEN lower($4) IN ('сдан', 'выполнен', 'completed', 'отменён', 'отменен', 'cancelled', 'canceled') THEN COALESCE($9, deleted_at, NOW()) ELSE NULL END WHERE id=$10 AND user_id=$11",
		d.Title, d.Subject, d.DueDate, d.Status, formatTags(d.Tags), d.RepeatType, d.Notes, d.ReminderTime, d.DeletedAt, id, userID,
	)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func deleteDeadlineDB(id string, userID int) error {
	result, err := db.Exec("DELETE FROM public.deadlines WHERE id=$1 AND user_id=$2", id, userID)
	if err != nil {
		return err
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ===== Валидация =====
func validateDeadline(d Deadline) error {
	if d.Title == "" || d.Subject == "" || d.DueDate == "" {
		return &BadRequestError{"Заполните все поля"}
	}
	allowedStatuses := map[string]bool{"в процессе": true, "сдан": true, "выполнен": true, "отменён": true}
	if !allowedStatuses[d.Status] {
		return &BadRequestError{"Неверный статус"}
	}
	if len(d.Tags) > 6 {
		return &BadRequestError{"Слишком много тегов"}
	}
	return nil
}

func isArchivedStatus(status string) bool {
	trimmed := strings.TrimSpace(strings.ToLower(status))
	switch trimmed {
	case "сдан", "выполнен", "completed", "отменён", "отменен", "canceled", "cancelled":
		return true
	default:
		return false
	}
}

func normalizeDeletedAt(d *Deadline) {
	if isArchivedStatus(d.Status) {
		if d.DeletedAt == nil {
			now := time.Now()
			d.DeletedAt = &now
		}
		return
	}
	d.DeletedAt = nil
}

func ensureDeletedAtForArchived(userID int) error {
	_, err := db.Exec("UPDATE public.deadlines SET deleted_at = NOW() WHERE user_id = $1 AND deleted_at IS NULL AND lower(status) IN ('сдан', 'выполнен', 'completed', 'отменён', 'отменен', 'cancelled', 'canceled')", userID)
	return err
}

func cleanupOldArchived(userID int) error {
	_, err := db.Exec("DELETE FROM public.deadlines WHERE user_id = $1 AND lower(status) IN ('сдан', 'выполнен', 'completed', 'отменён', 'отменен', 'cancelled', 'canceled') AND ((deleted_at IS NOT NULL AND deleted_at < NOW() - INTERVAL '30 days') OR (deleted_at IS NULL AND (CASE WHEN length(due_date) >= 16 THEN to_timestamp(due_date, 'YYYY-MM-DD HH24:MI') WHEN length(due_date) = 10 THEN to_timestamp(due_date, 'YYYY-MM-DD') ELSE NULL END) < NOW() - INTERVAL '30 days'))", userID)
	return err
}

type BadRequestError struct{ Message string }

func (e *BadRequestError) Error() string { return e.Message }

// ===== Auth Handlers =====
func handleRegister(w http.ResponseWriter, r *http.Request) {
	setHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	var req User
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Неверный формат данных", http.StatusBadRequest)
		return
	}

	if req.Email == "" || req.Password == "" {
		http.Error(w, "Email и пароль обязательны", http.StatusBadRequest)
		return
	}

	user, err := registerUser(req.Email, req.Password)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			http.Error(w, "Пользователь уже существует", http.StatusConflict)
		} else {
			http.Error(w, "Ошибка регистрации", http.StatusInternalServerError)
		}
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(AuthResponse{
		Email:                user.Email,
		RequiresVerification: true,
		Message:              "Проверьте почту и подтвердите email для входа",
	})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	setHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	var req User
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Неверный формат данных", http.StatusBadRequest)
		return
	}

	user, err := loginUser(req.Email, req.Password)
	if err != nil {
		if errors.Is(err, ErrEmailNotVerified) {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(AuthResponse{
				Email:                req.Email,
				RequiresVerification: true,
				Message:              "Почта не подтверждена. Проверьте email.",
			})
			return
		}
		http.Error(w, "Неверный email или пароль", http.StatusUnauthorized)
		return
	}

	token, err := generateToken(user.ID)
	if err != nil {
		http.Error(w, "Ошибка генерации токена", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(AuthResponse{Token: token, Email: user.Email})
}

func handleFirebaseLogin(w http.ResponseWriter, r *http.Request) {
	setHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}

	var req FirebaseLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Неверный формат данных", http.StatusBadRequest)
		return
	}

	idToken := strings.TrimSpace(req.IDToken)
	if idToken == "" {
		http.Error(w, "idToken обязателен", http.StatusBadRequest)
		return
	}

	email, emailVerified, _, err := parseFirebaseIdentity(idToken)
	if err != nil {
		http.Error(w, "Неверный Firebase токен", http.StatusUnauthorized)
		return
	}

	userID, dbVerified, err := upsertUserForFirebase(email, emailVerified)
	if err != nil {
		http.Error(w, "Ошибка синхронизации пользователя", http.StatusInternalServerError)
		return
	}
	if !dbVerified {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(AuthResponse{
			Email:                email,
			RequiresVerification: true,
			Message:              "Почта не подтверждена. Проверьте email в Firebase Auth.",
		})
		return
	}

	jwtToken, err := generateToken(userID)
	if err != nil {
		http.Error(w, "Ошибка генерации токена", http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(AuthResponse{Token: jwtToken, Email: email})
}

func handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	setHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}

	token := strings.TrimSpace(r.URL.Query().Get("token"))
	_, err := markUserVerifiedByToken(token)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("<html><body><h2>Ссылка недействительна или истекла.</h2></body></html>"))
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("<html><body><h2>Email успешно подтвержден. Можно возвращаться в приложение.</h2></body></html>"))
}

func handleResendVerification(w http.ResponseWriter, r *http.Request) {
	setHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}

	var req VerificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Неверный формат данных", http.StatusBadRequest)
		return
	}

	email := strings.TrimSpace(req.Email)
	if email == "" {
		http.Error(w, "Email обязателен", http.StatusBadRequest)
		return
	}

	if err := resendVerificationByEmail(email); err != nil {
		friendly := userFacingSMTPError(err)
		log.Printf("resend verification failed for %s: %v", email, err)
		http.Error(w, friendly, http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"message": "Если аккаунт существует, письмо отправлено"})
}

func handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	setHeaders(w)
	if r.Method != http.MethodDelete {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}

	userID := getUserID(r)
	if userID == 0 {
		http.Error(w, "Требуется авторизация", http.StatusUnauthorized)
		return
	}

	if err := deleteUserByID(userID); err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Пользователь не найден", http.StatusNotFound)
		} else {
			log.Printf("delete account failed for user %d: %v", userID, err)
			http.Error(w, "Ошибка удаления аккаунта", http.StatusInternalServerError)
		}
		return
	}

	log.Printf("account deleted for user_id=%d", userID)
	respondAccountDeleted(w)
}

func handleFirebaseDeleteAccount(w http.ResponseWriter, r *http.Request) {
	setHeaders(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}

	var req FirebaseLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Неверный формат данных", http.StatusBadRequest)
		return
	}

	idToken := strings.TrimSpace(req.IDToken)
	if idToken == "" {
		http.Error(w, "idToken обязателен", http.StatusBadRequest)
		return
	}

	email, _, _, err := parseFirebaseIdentity(idToken)
	if err != nil {
		http.Error(w, "Неверный Firebase токен", http.StatusUnauthorized)
		return
	}

	userID, err := getUserIDByEmail(email)
	if err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Пользователь не найден", http.StatusNotFound)
		} else {
			log.Printf("firebase delete account lookup failed for %s: %v", email, err)
			http.Error(w, "Ошибка удаления аккаунта", http.StatusInternalServerError)
		}
		return
	}

	if err := deleteUserByID(userID); err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Пользователь не найден", http.StatusNotFound)
		} else {
			log.Printf("firebase delete account failed for user %d: %v", userID, err)
			http.Error(w, "Ошибка удаления аккаунта", http.StatusInternalServerError)
		}
		return
	}

	log.Printf("firebase account deleted for user_id=%d email=%s", userID, email)
	respondAccountDeleted(w)
}

// ===== Deadline Handlers =====
func getDeadlines(w http.ResponseWriter, r *http.Request) {
	setHeaders(w)
	userID := getUserID(r)

	deadlines, err := getAllDeadlines(userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// фильтры по query
	status := r.URL.Query().Get("status")
	subject := r.URL.Query().Get("subject")

	now := time.Now()
	var result []Deadline
	for _, d := range deadlines {
		if (status == "" || d.Status == status) && (subject == "" || d.Subject == subject) {
			d.EffectiveDue = computeEffectiveDueDate(d.DueDate, d.RepeatType, now)
			result = append(result, d)
		}
	}

	if result == nil {
		result = []Deadline{}
	}
	json.NewEncoder(w).Encode(result)
}

func addDeadline(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	setHeaders(w)
	userID := getUserID(r)

	var newDeadline Deadline
	if err := json.NewDecoder(r.Body).Decode(&newDeadline); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	newDeadline.Tags = normalizeTags(newDeadline.Tags)
	normalizeDeletedAt(&newDeadline)

	if err := validateDeadline(newDeadline); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := insertDeadline(&newDeadline, userID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(newDeadline)
}

func updateDeadline(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	setHeaders(w)
	userID := getUserID(r)

	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.Error(w, "Не указан ID", http.StatusBadRequest)
		return
	}
	id := parts[2]

	var updated Deadline
	if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	updated.Tags = normalizeTags(updated.Tags)

	if err := validateDeadline(updated); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := updateDeadlineDB(id, updated, userID); err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Задача не найдена", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Если задача отмечена как "сдана" и включено повторение — создаём следующую по актуальным данным.
	if updated.Status == "сдан" && updated.RepeatType != "" && updated.RepeatType != "none" {
		nextDeadline := Deadline{
			Title:        updated.Title,
			Subject:      updated.Subject,
			DueDate:      nextDueDate(updated.DueDate, updated.RepeatType),
			Status:       "в процессе",
			Tags:         updated.Tags,
			RepeatType:   updated.RepeatType,
			Notes:        updated.Notes,
			ReminderTime: updated.ReminderTime,
		}
		insertDeadline(&nextDeadline, userID)
	}

	updated.ID = id
	json.NewEncoder(w).Encode(updated)
}

func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(tags))
	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		t := strings.TrimSpace(tag)
		if t == "" {
			continue
		}
		key := strings.ToLower(t)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		runes := []rune(strings.ToLower(t))
		if len(runes) > 0 {
			runes[0] = []rune(strings.ToUpper(string(runes[0])))[0]
		}
		result = append(result, string(runes))
	}
	sort.Strings(result)
	return result
}

func deleteDeadline(w http.ResponseWriter, r *http.Request) {
	setHeaders(w)
	userID := getUserID(r)

	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.Error(w, "Не указан ID", http.StatusBadRequest)
		return
	}
	id := parts[2]

	if err := deleteDeadlineDB(id, userID); err != nil {
		if err == sql.ErrNoRows {
			http.Error(w, "Задача не найдена", http.StatusNotFound)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ===== Утилита CORS =====
func setHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	w.Header().Set("Content-Type", "application/json")
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		setHeaders(w)
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	setHeaders(w)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		setHeaders(w)
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	setHeaders(w)
	json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
}

// ===== Main =====
func main() {
	if err := initDB(); err != nil {
		log.Fatal("Ошибка подключения к БД:", err)
	}
	defer db.Close()

	http.HandleFunc("/", handleRoot)
	http.HandleFunc("/health", handleHealth)

	// Auth endpoints (без авторизации)
	http.HandleFunc("/auth/register", handleRegister)
	http.HandleFunc("/auth/login", handleLogin)
	http.HandleFunc("/auth/firebase/login", handleFirebaseLogin)
	http.HandleFunc("/auth/verify", handleVerifyEmail)
	http.HandleFunc("/auth/resend-verification", handleResendVerification)
	http.HandleFunc("/auth/firebase/delete-account", handleFirebaseDeleteAccount)
	http.HandleFunc("/auth/firebase/account", handleFirebaseDeleteAccount)
	http.HandleFunc("/auth/account", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			setHeaders(w)
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodDelete {
			authMiddleware(handleDeleteAccount)(w, r)
			return
		}
		setHeaders(w)
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
	})

	// Deadline endpoints (с авторизацией)
	http.HandleFunc("/deadlines", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			setHeaders(w)
			w.WriteHeader(http.StatusOK)
			return
		}
		switch r.Method {
		case http.MethodGet:
			authMiddleware(getDeadlines)(w, r)
		case http.MethodPost:
			authMiddleware(addDeadline)(w, r)
		default:
			http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		}
	})

	http.HandleFunc("/deadlines/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			setHeaders(w)
			w.WriteHeader(http.StatusOK)
			return
		}
		switch r.Method {
		case http.MethodPut:
			authMiddleware(updateDeadline)(w, r)
		case http.MethodDelete:
			authMiddleware(deleteDeadline)(w, r)
		default:
			http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		}
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("Сервер запущен на :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/sirupsen/logrus"
)

type EnvData struct {
	senderEmail     string
	senderPassword  string
	recipientEmails []string
	smtpHost        string
	smtpPort        string
	dbURL           string
	serverPort      string
	telegramBotApi  string
	chatIds         []int64
	corsOrigins     []string
	backendURL      string
}

type ReserveRequest struct {
	Name        string `json:"name"`
	Email       string `json:"email"`
	PhoneNumber string `json:"phoneNumber"`
	Guests      int    `json:"guests"`
	CheckIn     string `json:"checkIn"`
	CheckOut    string `json:"checkOut"`
	RoomType    string `json:"roomType"`
}

type ContactRequest struct {
	Name        string `json:"name"`
	Email       string `json:"email"`
	PhoneNumber string `json:"phoneNumber"`
	Message     string `json:"message"`
}

type TrackRequest struct {
	Page     string `json:"page"`
	Referrer string `json:"referrer"`
}

type Response struct {
	Message string `json:"message"`
}

type loginAuth struct {
	username, password string
}

// rateLimiter tracks requests per IP using a sliding window.
type rateLimiter struct {
	mu       sync.Mutex
	requests map[string][]time.Time
	limit    int
	window   time.Duration
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{
		requests: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
	}
	go rl.cleanup()
	return rl
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-rl.window)
	var recent []time.Time
	for _, t := range rl.requests[ip] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) >= rl.limit {
		rl.requests[ip] = recent
		return false
	}
	rl.requests[ip] = append(recent, now)
	return true
}

func (rl *rateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-rl.window)
		for ip, times := range rl.requests {
			var recent []time.Time
			for _, t := range times {
				if t.After(cutoff) {
					recent = append(recent, t)
				}
			}
			if len(recent) == 0 {
				delete(rl.requests, ip)
			} else {
				rl.requests[ip] = recent
			}
		}
		rl.mu.Unlock()
	}
}

func LoginAuth(username, password string) smtp.Auth {
	return &loginAuth{username, password}
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", []byte{}, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		switch string(fromServer) {
		case "Username:":
			return []byte(a.username), nil
		case "Password:":
			return []byte(a.password), nil
		default:
			return nil, errors.New("unknown fromServer")
		}
	}
	return nil, nil
}

func enableCORS(next http.HandlerFunc, allowedOrigins []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		for _, allowed := range allowedOrigins {
			if origin == allowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				break
			}
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

func rateLimit(next http.HandlerFunc, rl *rateLimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		if !rl.allow(ip) {
			http.Error(w, "Too many requests", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

func validateContactRequest(r ContactRequest) error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("name is required")
	}
	if !strings.Contains(r.Email, "@") {
		return errors.New("invalid email")
	}
	if len(r.Message) > 5000 {
		return errors.New("message too long")
	}
	return nil
}

func validateReserveRequest(r ReserveRequest) error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("name is required")
	}
	if !strings.Contains(r.Email, "@") {
		return errors.New("invalid email")
	}
	if r.Guests <= 0 || r.Guests > 20 {
		return errors.New("invalid guest count")
	}
	if strings.TrimSpace(r.CheckIn) == "" || strings.TrimSpace(r.CheckOut) == "" {
		return errors.New("check-in and check-out dates are required")
	}
	return nil
}

func saveContactToDB(contactRequest ContactRequest, pool *pgxpool.Pool) error {
	_, err := pool.Exec(context.Background(),
		`INSERT INTO contacts (email, name, phone_number, message) VALUES ($1, $2, $3, $4)`,
		contactRequest.Email, contactRequest.Name, contactRequest.PhoneNumber, contactRequest.Message)
	if err != nil {
		logrus.Errorf("Failed to insert contact record: %s", err)
	}
	return err
}

func saveReservationToDB(reserveRequest ReserveRequest, pool *pgxpool.Pool) error {
	_, err := pool.Exec(context.Background(),
		`INSERT INTO reservations (email, name, phone_number, check_in, check_out, guests, room_type) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		reserveRequest.Email, reserveRequest.Name, reserveRequest.PhoneNumber,
		reserveRequest.CheckIn, reserveRequest.CheckOut, reserveRequest.Guests, reserveRequest.RoomType)
	if err != nil {
		logrus.Errorf("Failed to insert reservation record: %s", err)
	}
	return err
}

func savePageViewToDB(req TrackRequest, ip, userAgent string, pool *pgxpool.Pool) {
	_, err := pool.Exec(context.Background(),
		`INSERT INTO page_views (ip, user_agent, page, referrer) VALUES ($1, $2, $3, $4)`,
		ip, userAgent, req.Page, req.Referrer)
	if err != nil {
		logrus.Errorf("Failed to insert page view: %s", err)
	}
}

func sendEmail(subject, body string, envData EnvData) error {
	auth := LoginAuth(envData.senderEmail, envData.senderPassword)
	recipientList := strings.Join(envData.recipientEmails, ", ")
	msg := fmt.Sprintf("From: %s\nTo: %s\nSubject: %s\n\n%s",
		envData.senderEmail, recipientList, subject, body)
	err := smtp.SendMail(
		envData.smtpHost+":"+envData.smtpPort,
		auth,
		envData.senderEmail,
		envData.recipientEmails,
		[]byte(msg),
	)
	if err != nil {
		logrus.Errorf("Failed to send email: %s", err)
	}
	return err
}

func sendMessage(bot *tgbotapi.BotAPI, envData EnvData, text string) {
	for _, chatID := range envData.chatIds {
		msg := tgbotapi.NewMessage(chatID, text)
		_, err := bot.Send(msg)
		if err != nil {
			log.Printf("Failed to send Telegram message: %v", err)
		}
	}
}

// runHealthChecks POSTs to each API endpoint and returns alert messages for any failures.
func runHealthChecks(backendURL string) []string {
	var alerts []string
	client := &http.Client{Timeout: 10 * time.Second}

	// Test contact endpoint
	contactBody := `{"name":"Monitor","email":"test@test.com","phoneNumber":"","message":"Health check"}`
	resp, err := client.Post(backendURL+"/api/contact", "application/json", bytes.NewBufferString(contactBody))
	if err != nil {
		alerts = append(alerts, fmt.Sprintf("Contact endpoint FAILED: %v", err))
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			alerts = append(alerts, fmt.Sprintf("Contact endpoint returned HTTP %d", resp.StatusCode))
		}
	}

	// Test reserve endpoint for each room type
	for _, roomType := range []string{"Standard Suite", "Deluxe Suite", "Double Bed"} {
		body := fmt.Sprintf(
			`{"name":"Monitor","email":"test@test.com","phoneNumber":"","guests":1,"checkIn":"2026-01-01","checkOut":"2026-01-02","roomType":"%s"}`,
			roomType,
		)
		resp, err := client.Post(backendURL+"/api/reserve", "application/json", bytes.NewBufferString(body))
		if err != nil {
			alerts = append(alerts, fmt.Sprintf("Reserve (%s) endpoint FAILED: %v", roomType, err))
		} else {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				alerts = append(alerts, fmt.Sprintf("Reserve (%s) endpoint returned HTTP %d", roomType, resp.StatusCode))
			}
		}
	}

	return alerts
}

func getWeeklyStats(pool *pgxpool.Pool) string {
	ctx := context.Background()

	var totalViews, uniqueIPs, newContacts, newReservations int
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM page_views WHERE created_at > NOW() - INTERVAL '7 days'`).Scan(&totalViews)
	pool.QueryRow(ctx, `SELECT COUNT(DISTINCT ip) FROM page_views WHERE created_at > NOW() - INTERVAL '7 days'`).Scan(&uniqueIPs)
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM contacts WHERE created_at > NOW() - INTERVAL '7 days' AND email != 'test@test.com'`).Scan(&newContacts)
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM reservations WHERE created_at > NOW() - INTERVAL '7 days' AND email != 'test@test.com'`).Scan(&newReservations)

	// Top 5 pages
	var topPages []string
	rows, err := pool.Query(ctx, `
		SELECT page, COUNT(*) AS cnt
		FROM page_views
		WHERE created_at > NOW() - INTERVAL '7 days'
		GROUP BY page ORDER BY cnt DESC LIMIT 5`)
	if err == nil {
		for rows.Next() {
			var page string
			var cnt int
			rows.Scan(&page, &cnt)
			topPages = append(topPages, fmt.Sprintf("  %s (%d views)", page, cnt))
		}
		rows.Close()
	}

	// Top 5 user agents
	var topAgents []string
	rows, err = pool.Query(ctx, `
		SELECT user_agent, COUNT(*) AS cnt
		FROM page_views
		WHERE created_at > NOW() - INTERVAL '7 days'
		GROUP BY user_agent ORDER BY cnt DESC LIMIT 5`)
	if err == nil {
		for rows.Next() {
			var ua string
			var cnt int
			rows.Scan(&ua, &cnt)
			if len(ua) > 80 {
				ua = ua[:80] + "..."
			}
			topAgents = append(topAgents, fmt.Sprintf("  %s (%d)", ua, cnt))
		}
		rows.Close()
	}

	// Top 5 referrers
	var topReferrers []string
	rows, err = pool.Query(ctx, `
		SELECT COALESCE(NULLIF(referrer,''), '(direct)') AS ref, COUNT(*) AS cnt
		FROM page_views
		WHERE created_at > NOW() - INTERVAL '7 days'
		GROUP BY ref ORDER BY cnt DESC LIMIT 5`)
	if err == nil {
		for rows.Next() {
			var ref string
			var cnt int
			rows.Scan(&ref, &cnt)
			topReferrers = append(topReferrers, fmt.Sprintf("  %s (%d)", ref, cnt))
		}
		rows.Close()
	}

	// Recent contacts
	var contactLines []string
	rows, err = pool.Query(ctx, `
		SELECT name, email, phone_number, created_at
		FROM contacts
		WHERE created_at > NOW() - INTERVAL '7 days' AND email != 'test@test.com'
		ORDER BY created_at DESC`)
	if err == nil {
		for rows.Next() {
			var name, email, phone string
			var t time.Time
			rows.Scan(&name, &email, &phone, &t)
			contactLines = append(contactLines, fmt.Sprintf("  %s | %s | %s | %s",
				name, email, phone, t.Format("Jan 2 15:04")))
		}
		rows.Close()
	}

	// Recent reservations
	var reservationLines []string
	rows, err = pool.Query(ctx, `
		SELECT name, email, phone_number, room_type, guests, check_in, check_out, created_at
		FROM reservations
		WHERE created_at > NOW() - INTERVAL '7 days' AND email != 'test@test.com'
		ORDER BY created_at DESC`)
	if err == nil {
		for rows.Next() {
			var name, email, phone, roomType string
			var guests int
			var checkIn, checkOut, createdAt time.Time
			rows.Scan(&name, &email, &phone, &roomType, &guests, &checkIn, &checkOut, &createdAt)
			reservationLines = append(reservationLines, fmt.Sprintf("  %s | %s | %s | %s | %d guests | %s to %s | booked %s",
				name, email, phone, roomType, guests,
				checkIn.Format("Jan 2"), checkOut.Format("Jan 2"), createdAt.Format("Jan 2 15:04")))
		}
		rows.Close()
	}

	join := func(lines []string, empty string) string {
		if len(lines) == 0 {
			return "  " + empty
		}
		return strings.Join(lines, "\n")
	}

	return fmt.Sprintf(`Turaco Weekly Report - %s

=== WEBSITE TRAFFIC (Last 7 Days) ===
Total Page Views : %d
Unique Visitors  : %d (by IP)

Top Pages:
%s

Top Browsers / Devices:
%s

Top Referrers:
%s

=== NEW CONTACTS (%d) ===
%s

=== NEW RESERVATIONS (%d) ===
%s`,
		time.Now().Format("Jan 2, 2006"),
		totalViews, uniqueIPs,
		join(topPages, "(none)"),
		join(topAgents, "(none)"),
		join(topReferrers, "(none)"),
		newContacts, join(contactLines, "(none)"),
		newReservations, join(reservationLines, "(none)"),
	)
}

func startMonitoringBot(ctx context.Context, pool *pgxpool.Pool, bot *tgbotapi.BotAPI, envData EnvData) {
	healthTicker := time.NewTicker(1 * time.Hour)
	weeklyTicker := time.NewTicker(7 * 24 * time.Hour)

	for {
		select {
		case <-healthTicker.C:
			alerts := runHealthChecks(envData.backendURL)
			if len(alerts) > 0 {
				msg := "MONITORING ALERT\n\n" + strings.Join(alerts, "\n")
				sendMessage(bot, envData, msg)
			}

		case <-weeklyTicker.C:
			report := getWeeklyStats(pool)
			subject := "Turaco Weekly Report - " + time.Now().Format("Jan 2, 2006")
			sendEmail(subject, report, envData)

		case <-ctx.Done():
			return
		}
	}
}

func getClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

func main() {
	ctx := context.Background()
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	var envData EnvData
	envData.senderEmail = os.Getenv("SENDEREMAIL")
	envData.senderPassword = os.Getenv("SENDERPASSWORD")

	recipientEmailsStr := os.Getenv("RECEPIENTEMAIL")
	if recipientEmailsStr == "" {
		log.Fatal("RECEPIENTEMAIL environment variable is required")
	}
	for _, email := range strings.Split(recipientEmailsStr, ",") {
		if t := strings.TrimSpace(email); t != "" {
			envData.recipientEmails = append(envData.recipientEmails, t)
		}
	}
	if len(envData.recipientEmails) == 0 {
		log.Fatal("At least one recipient email is required")
	}

	envData.smtpHost = os.Getenv("SMTPHOST")
	envData.smtpPort = os.Getenv("SMTPPORT")
	envData.dbURL = os.Getenv("DBURL")
	envData.serverPort = os.Getenv("SERVERPORT")

	corsOriginStr := os.Getenv("CORSORIGIN")
	if corsOriginStr == "" {
		log.Fatal("CORSORIGIN environment variable is required")
	}
	for _, o := range strings.Split(corsOriginStr, ",") {
		if o = strings.TrimSpace(o); o != "" {
			envData.corsOrigins = append(envData.corsOrigins, o)
		}
	}

	envData.backendURL = os.Getenv("BACKENDURL")
	if envData.backendURL == "" {
		envData.backendURL = fmt.Sprintf("http://localhost:%s", envData.serverPort)
	}

	pool, err := pgxpool.New(ctx, envData.dbURL)
	if err != nil {
		log.Fatal("Unable to connect to database:", err)
	}
	defer pool.Close()

	envData.telegramBotApi = os.Getenv("TELEGRAMBOTAPI")
	bot, err := tgbotapi.NewBotAPI(envData.telegramBotApi)
	if err != nil {
		log.Fatal("Unable to create bot:", err)
	}

	chatIdsStr := os.Getenv("CHATIDLIST")
	for _, idStr := range strings.Split(chatIdsStr, ",") {
		idStr = strings.TrimSpace(idStr)
		if idStr == "" {
			continue
		}
		chatID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			log.Printf("Invalid chat ID: %s", idStr)
			continue
		}
		envData.chatIds = append(envData.chatIds, chatID)
	}

	go startMonitoringBot(ctx, pool, bot, envData)

	rl := newRateLimiter(10, time.Minute)

	http.HandleFunc("/api/reserve", enableCORS(rateLimit(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Unable to read body", http.StatusBadRequest)
			return
		}

		var reqData ReserveRequest
		if err = json.Unmarshal(body, &reqData); err != nil {
			http.Error(w, "Invalid JSON format", http.StatusBadRequest)
			return
		}

		if err = validateReserveRequest(reqData); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err = saveReservationToDB(reqData, pool); err != nil {
			http.Error(w, "Failed to save reservation", http.StatusInternalServerError)
			return
		}

		if reqData.Email != "test@test.com" {
			msg := fmt.Sprintf(
				"New Reservation\n\nName: %s\nEmail: %s\nPhone: %s\nRoom: %s\nGuests: %d\nCheck-in: %s\nCheck-out: %s",
				reqData.Name, reqData.Email, reqData.PhoneNumber, reqData.RoomType,
				reqData.Guests, reqData.CheckIn, reqData.CheckOut,
			)
			sendMessage(bot, envData, msg)
		}

		json.NewEncoder(w).Encode(Response{Message: "Received request"})
	}, rl), envData.corsOrigins))

	http.HandleFunc("/api/contact", enableCORS(rateLimit(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Unable to read body", http.StatusBadRequest)
			return
		}

		var reqData ContactRequest
		if err = json.Unmarshal(body, &reqData); err != nil {
			http.Error(w, "Invalid JSON format", http.StatusBadRequest)
			return
		}

		if err = validateContactRequest(reqData); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err = saveContactToDB(reqData, pool); err != nil {
			http.Error(w, "Failed to save contact", http.StatusInternalServerError)
			return
		}

		if reqData.Email != "test@test.com" {
			msg := fmt.Sprintf(
				"New Contact\n\nName: %s\nEmail: %s\nPhone: %s\nMessage:\n%s",
				reqData.Name, reqData.Email, reqData.PhoneNumber, reqData.Message,
			)
			sendMessage(bot, envData, msg)
		}

		json.NewEncoder(w).Encode(Response{Message: "Received Request"})
	}, rl), envData.corsOrigins))

	http.HandleFunc("/api/track", enableCORS(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method != "POST" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Unable to read body", http.StatusBadRequest)
			return
		}

		var reqData TrackRequest
		if err = json.Unmarshal(body, &reqData); err != nil {
			http.Error(w, "Invalid JSON format", http.StatusBadRequest)
			return
		}

		ip := getClientIP(r)
		userAgent := r.Header.Get("User-Agent")
		savePageViewToDB(reqData, ip, userAgent, pool)

		json.NewEncoder(w).Encode(Response{Message: "Tracked"})
	}, envData.corsOrigins))

	fmt.Printf("\nServer is running on port %s...\n", envData.serverPort)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", envData.serverPort), nil))
}

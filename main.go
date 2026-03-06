package main

import (
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
	"github.com/jackc/pgx/v5"
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
	insertSQL := `
    INSERT INTO contacts (email, name, phone_number, message)
    VALUES ($1, $2, $3, $4);
    `
	_, err := pool.Exec(context.Background(), insertSQL,
		contactRequest.Email, contactRequest.Name, contactRequest.PhoneNumber, contactRequest.Message)
	if err != nil {
		logrus.Errorf("Failed to insert contact record: %s", err)
		return err
	}
	return nil
}

func saveReservationToDB(reserveRequest ReserveRequest, pool *pgxpool.Pool) error {
	insertSQL := `
    INSERT INTO reservations (email, name, phone_number, check_in, check_out, guests, room_type)
    VALUES ($1, $2, $3, $4, $5, $6, $7);
    `
	_, err := pool.Exec(context.Background(), insertSQL,
		reserveRequest.Email, reserveRequest.Name, reserveRequest.PhoneNumber,
		reserveRequest.CheckIn, reserveRequest.CheckOut, reserveRequest.Guests, reserveRequest.RoomType)
	if err != nil {
		logrus.Errorf("Failed to insert reservation record: %s", err)
		return err
	}
	return nil
}

func composeContactEmail(contact ContactRequest, envData EnvData) string {
	subject := "Customer Contact Request"
	body := fmt.Sprintf("You have a new contact request:\n\nName: %s\nEmail: %s\nPhone Number: %s\nMessage: %s",
		contact.Name, contact.Email, contact.PhoneNumber, contact.Message)
	recipientList := strings.Join(envData.recipientEmails, ", ")
	return fmt.Sprintf("From: %s\nTo: %s\nSubject: %s\n\n%s", envData.senderEmail, recipientList, subject, body)
}

func composeReserveEmail(reservation ReserveRequest, envData EnvData) string {
	subject := "Customer Reserve Request"
	body := fmt.Sprintf("You have a new reservations request:\n\nName: %s\nEmail: %s\nPhone Number: %s\nCheck-in: %s\nCheck-out: %s\nGuests: %d\nRoom Type: %s",
		reservation.Name, reservation.Email, reservation.PhoneNumber,
		reservation.CheckIn, reservation.CheckOut, reservation.Guests, reservation.RoomType)
	recipientList := strings.Join(envData.recipientEmails, ", ")
	return fmt.Sprintf("From: %s\nTo: %s\nSubject: %s\n\n%s", envData.senderEmail, recipientList, subject, body)
}

func sendEmail(msg string, envData EnvData) error {
	auth := LoginAuth(envData.senderEmail, envData.senderPassword)
	err := smtp.SendMail(envData.smtpHost+":"+envData.smtpPort, auth, envData.senderEmail, envData.recipientEmails, []byte(msg))
	if err != nil {
		logrus.Errorf("Failed to send email: %s", err)
	}
	return err
}

func checkContactRequestTest(pool *pgxpool.Pool) []string {
	var alerts []string
	var contactTime time.Time

	err := pool.QueryRow(context.Background(), `
		SELECT created_at
		FROM contacts
		WHERE email = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, "test@test.com").Scan(&contactTime)

	if err != nil {
		if err == pgx.ErrNoRows {
			return alerts
		}
		logrus.Errorf("Error querying contacts: %v", err)
		return alerts
	}

	timeDiff := time.Since(contactTime)
	if timeDiff > time.Hour {
		alertMsg := fmt.Sprintf("🚨 **Contact Alert**: Latest test contact is more than 1 hour old\n"+
			"Contact Time: %s\n⏰ Age: %.1f hours", contactTime.Format("2006-01-02 15:04"), timeDiff.Hours())
		alerts = append(alerts, alertMsg)
	}

	return alerts
}

func checkReservationRequestTest(roomType string, pool *pgxpool.Pool) []string {
	var alerts []string
	var contactTime time.Time

	err := pool.QueryRow(context.Background(), `
		SELECT created_at
		FROM reservations
		WHERE email = $1 AND room_type = $2
		ORDER BY created_at DESC
		LIMIT 1
	`, "test@test.com", roomType).Scan(&contactTime)

	if err != nil {
		if err == pgx.ErrNoRows {
			return alerts
		}
		logrus.Errorf("Error querying %s reservations: %v", roomType, err)
		return alerts
	}

	timeDiff := time.Since(contactTime)
	if timeDiff > time.Hour {
		alertMsg := fmt.Sprintf("🚨 **%s Reservation Alert**: Latest test reservation is more than 1 hour old\n"+
			"Contact Time: %s\n⏰ Age: %.1f hours", roomType, contactTime.Format("2006-01-02 15:04"), timeDiff.Hours())
		alerts = append(alerts, alertMsg)
	}

	return alerts
}

func sendMessage(bot *tgbotapi.BotAPI, envData EnvData, finalMessage string) {
	for _, chatID := range envData.chatIds {
		msg := tgbotapi.NewMessage(chatID, finalMessage)
		_, err := bot.Send(msg)
		if err != nil {
			log.Printf("Failed to send message: %v", err)
		}
	}
}

func startMonitoringBot(ctx context.Context, pool *pgxpool.Pool, bot *tgbotapi.BotAPI, envData EnvData) {
	ticker := time.NewTicker(1 * time.Hour)

	for {
		select {
		case <-ticker.C:
			var allMessages []string
			allMessages = append(allMessages, checkContactRequestTest(pool)...)
			allMessages = append(allMessages, checkReservationRequestTest("Standard Suite", pool)...)
			allMessages = append(allMessages, checkReservationRequestTest("Deluxe Suite", pool)...)
			allMessages = append(allMessages, checkReservationRequestTest("Double Bed", pool)...)

			if len(allMessages) > 0 {
				finalMessage := strings.Join(allMessages, "\n")
				sendMessage(bot, envData, finalMessage)
			}
		case <-ctx.Done():
			return
		}
	}
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
	envData.recipientEmails = make([]string, 0)
	for _, email := range strings.Split(recipientEmailsStr, ",") {
		trimmedEmail := strings.TrimSpace(email)
		if trimmedEmail != "" {
			envData.recipientEmails = append(envData.recipientEmails, trimmedEmail)
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
		o = strings.TrimSpace(o)
		if o != "" {
			envData.corsOrigins = append(envData.corsOrigins, o)
		}
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
	envData.chatIds = make([]int64, 0)
	for _, idStr := range strings.Split(chatIdsStr, ",") {
		idStr = strings.TrimSpace(idStr)
		if idStr != "" {
			chatID, err := strconv.ParseInt(idStr, 10, 64)
			if err != nil {
				log.Printf("Invalid chat ID: %s", idStr)
				continue
			}
			envData.chatIds = append(envData.chatIds, chatID)
		}
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
			msg := composeReserveEmail(reqData, envData)
			sendEmail(msg, envData)
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
			msg := composeContactEmail(reqData, envData)
			sendEmail(msg, envData)
		}

		json.NewEncoder(w).Encode(Response{Message: "Received Request"})
	}, rl), envData.corsOrigins))

	fmt.Printf("\nServer is running on port %s...\n", envData.serverPort)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", envData.serverPort), nil))
}

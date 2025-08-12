package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/jackc/pgx/v5"
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

func LogFunction(args ...interface{}) {
	// Get the name of the calling function
	pc, _, _, _ := runtime.Caller(1)
	funcName := runtime.FuncForPC(pc).Name()

	// Log the function name and arguments using logrus
	logrus.WithFields(logrus.Fields{
		"function": funcName,
		"args":     fmt.Sprintf("%v", args),
	}).Info("Function called")
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
			return nil, errors.New("unkown fromServer")
		}
	}
	return nil, nil
}

// CORS middleware to enable cross-origin requests
func enableCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*") //TODO: change to domain
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}
func saveContactToDB(contactRequest ContactRequest, conn *pgx.Conn) {
	LogFunction(contactRequest, conn)

	insertSQL := `
    INSERT INTO contacts (email, name, phone_number, message)
    VALUES ($1, $2, $3, $4);
    `

	email := contactRequest.Email
	name := contactRequest.Name
	phoneNumber := contactRequest.PhoneNumber
	message := contactRequest.Message

	// Execute the SQL insert statement
	_, err := conn.Exec(context.Background(), insertSQL, email, name, phoneNumber, message)
	if err != nil {
		logrus.Errorf("Failed to insert record: %s", err)
	}
}

func saveReservationToDB(reserveRequest ReserveRequest, conn *pgx.Conn) {
	LogFunction(reserveRequest, conn)

	insertSQL := `
    INSERT INTO reservations (email, name, phone_number, check_in, check_out, guests, room_type)
    VALUES ($1, $2, $3, $4, $5, $6, $7);
    `
	// TODO: SQL Injection
	email := reserveRequest.Email
	name := reserveRequest.Name
	phoneNumber := reserveRequest.PhoneNumber
	checkIn := reserveRequest.CheckIn
	checkOut := reserveRequest.CheckOut
	guests := reserveRequest.Guests
	roomType := reserveRequest.RoomType

	// Execute the SQL insert statement
	_, err := conn.Exec(context.Background(), insertSQL, email, name, phoneNumber, checkIn, checkOut, guests, roomType)
	if err != nil {
		logrus.Errorf("Failed to insert record: %s", err)
	}

}

func composeContactEmail(contact ContactRequest, envData EnvData) string {
	LogFunction(contact)
	subject := "Customer Contact Request"
	body := fmt.Sprintf("You have a new contact request:\n\nName: %s\nEmail: %s\nPhone Number: %s\nMessage: %s", contact.Name, contact.Email, contact.PhoneNumber, contact.Message)

	recipientList := strings.Join(envData.recipientEmails, ", ")
	msg := fmt.Sprintf("From: %s\nTo: %s\nSubject: %s\n\n%s", envData.senderEmail, recipientList, subject, body)

	return msg
}

func composeReserveEmail(reservation ReserveRequest, envData EnvData) string {
	LogFunction(reservation)
	subject := "Customer Reserve Request"
	body := fmt.Sprintf("You have a new reservations request:\n\nName: %s\nEmail: %s\nPhone Number: %s\nCheck-in: %s\nCheck-out: %s\nGuests: %d\n Room Type: %s", reservation.Name, reservation.Email, reservation.PhoneNumber, reservation.CheckIn, reservation.CheckOut, reservation.Guests, reservation.RoomType)

	recipientList := strings.Join(envData.recipientEmails, ", ")
	msg := fmt.Sprintf("From: %s\nTo: %s\nSubject: %s\n\n%s", envData.senderEmail, recipientList, subject, body)

	return msg
}

// Send email notification
func sendEmail(msg string, envData EnvData) error {
	LogFunction(msg)

	auth := LoginAuth(envData.senderEmail, envData.senderPassword)
	err := smtp.SendMail(envData.smtpHost+":"+envData.smtpPort, auth, envData.senderEmail, envData.recipientEmails, []byte(msg))
	if err != nil {
		logrus.Errorf("Err sending email: %s", err)
	}
	return err
}
func checkContactRequestTest(conn *pgx.Conn) []string {
	var alerts []string
	email := "test@test.com"
	var contactTime time.Time

	err := conn.QueryRow(context.Background(), `
		SELECT name, phone_number, message, created_at 
		FROM contacts 
		WHERE email = $1 
		ORDER BY created_at DESC 
		LIMIT 1
	`, email).Scan(&contactTime)

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

func checkReservationRequestTest(roomType string, conn *pgx.Conn) []string {
	var alerts []string
	email := "test@test.com"
	var contactTime time.Time

	err := conn.QueryRow(context.Background(), `
		SELECT name, phone_number, message, created_at 
		FROM reservations 
		WHERE email = $1 AND room_type = $2
		ORDER BY created_at DESC 
		LIMIT 1
	`, email, roomType).Scan(&contactTime)

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
func startMonitoringBot(ctx context.Context, conn *pgx.Conn, bot *tgbotapi.BotAPI, envData EnvData) {
	ticker := time.NewTicker(1 * time.Hour)

	for {
		select {
		case <-ticker.C:
			var allMessages []string
			allMessages = append(allMessages, checkContactRequestTest(conn)...)
			allMessages = append(allMessages, checkReservationRequestTest("Standard Suite", conn)...)
			allMessages = append(allMessages, checkReservationRequestTest("Deluxe Suite", conn)...)
			allMessages = append(allMessages, checkReservationRequestTest("Double Bed", conn)...)

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
	// Split by comma and trim whitespace
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

	envData.smtpHost = os.Getenv("SMTPHOST") // Outlook SMTP server
	envData.smtpPort = os.Getenv("SMTPPORT")
	envData.dbURL = os.Getenv("DBURL")
	envData.serverPort = os.Getenv("SEREVRPORT")
	// Connect to the PostgreSQL database
	conn, err := pgx.Connect(ctx, envData.dbURL)
	if err != nil {
		log.Fatal("Unable to connect to database:", err)
	}
	defer conn.Close(ctx)

	envData.telegramBotApi = os.Getenv("TELEGRAMBOTAPI")
	chatIdsStr := os.Getenv("CHATIDLIST")
	bot, err := tgbotapi.NewBotAPI(envData.telegramBotApi)

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

	if err != nil {
		log.Fatal("Unable to create bot:", err)
	}
	go startMonitoringBot(ctx, conn, bot, envData)
	http.HandleFunc("/api/reserve", enableCORS(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Handle POST request
		if r.Method == "POST" {
			body, err := ioutil.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Unable to read body", http.StatusBadRequest)
				return
			}

			// Parse the request body (JSON) into a struct
			var reqData ReserveRequest
			err = json.Unmarshal(body, &reqData)
			if err != nil {
				http.Error(w, "Invalid JSON format", http.StatusBadRequest)
				return
			}
			saveReservationToDB(reqData, conn)
			if reqData.Email != "test@test.com" {
				msg := composeReserveEmail(reqData, envData)

				sendEmail(msg, envData)
			}

			// Respond with a message containing the received data
			response := Response{Message: "Received request"}
			json.NewEncoder(w).Encode(response)
		}
	}))

	http.HandleFunc("/api/contact", enableCORS(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Handle POST request
		if r.Method == "POST" {
			body, err := ioutil.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "Unable to read body", http.StatusBadRequest)
				return
			}

			// Parse the request body (JSON) into a struct
			var reqData ContactRequest
			err = json.Unmarshal(body, &reqData) // TODO: Check if correct
			if err != nil {
				http.Error(w, "Invalid JSON format", http.StatusBadRequest)
				return
			}

			saveContactToDB(reqData, conn)
			if reqData.Email != "test@test.com" {
				msg := composeContactEmail(reqData, envData)

				sendEmail(msg, envData)
			}

			// Respond with a message containing the received data
			response := Response{Message: "Received Request"}
			json.NewEncoder(w).Encode(response)
		}
	}))

	// Start the server
	fmt.Printf("\nServer is running on port %s...\n", envData.serverPort) //TODO: Rate Limit
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", envData.serverPort), nil))
}

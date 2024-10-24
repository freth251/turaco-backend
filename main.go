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

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/sirupsen/logrus"
)

type EnvData struct {
	senderEmail    string
	senderPassword string
	recipientEmail string
	smtpHost       string
	smtpPort       string
	dbURL          string
	serverPort     string
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
		w.Header().Set("Access-Control-Allow-Origin", "*")
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
	msg := fmt.Sprintf("From: %s\nTo: %s\nSubject: %s\n\n%s", envData.senderEmail, envData.recipientEmail, subject, body)

	return msg

}

func composeReserveEmail(reservation ReserveRequest, envData EnvData) string {
	LogFunction(reservation)
	subject := "Customer Reserve Request"
	body := fmt.Sprintf("You have a new reservations request:\n\nName: %s\nEmail: %s\nPhone Number: %s\nCheck-in: %s\nCheck-out: %s\nGuests: %d\n Room Type: %s", reservation.Name, reservation.Email, reservation.PhoneNumber, reservation.CheckIn, reservation.CheckOut, reservation.Guests, reservation.RoomType)
	msg := fmt.Sprintf("From: %s\nTo: %s\nSubject: %s\n\n%s", envData.senderEmail, envData.recipientEmail, subject, body)

	return msg

}

// Send email notification
func sendEmail(msg string, envData EnvData) error {
	LogFunction(msg)

	auth := LoginAuth(envData.senderEmail, envData.senderPassword)
	err := smtp.SendMail(envData.smtpHost+":"+envData.smtpPort, auth, envData.senderEmail, []string{envData.recipientEmail}, []byte(msg))
	if err != nil {
		logrus.Errorf("Err sending email: %s", err)
	}
	return err
}
func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}
	var envData EnvData
	envData.senderEmail = os.Getenv("SENDEREMAIL")
	envData.senderPassword = os.Getenv("SENDERPASSWORD")
	envData.recipientEmail = os.Getenv("RECEPIENTEMAIL")
	envData.smtpHost = os.Getenv("SMTPHOST") // Outlook SMTP server
	envData.smtpPort = os.Getenv("SMTPPORT")
	envData.dbURL = os.Getenv("DBURL")
	envData.serverPort = os.Getenv("SEREVRPORT")
	// Connect to the PostgreSQL database
	conn, err := pgx.Connect(context.Background(), envData.dbURL)
	if err != nil {
		log.Fatal("Unable to connect to database:", err)
	}
	defer conn.Close(context.Background())

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
			msg := composeReserveEmail(reqData, envData)

			sendEmail(msg, envData)

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
			err = json.Unmarshal(body, &reqData)
			if err != nil {
				http.Error(w, "Invalid JSON format", http.StatusBadRequest)
				return
			}

			saveContactToDB(reqData, conn)
			msg := composeContactEmail(reqData, envData)

			sendEmail(msg, envData)

			// Respond with a message containing the received data
			response := Response{Message: "Received Request"}
			json.NewEncoder(w).Encode(response)
		}
	}))

	// Start the server
	fmt.Printf("\nServer is running on port %s...\n", envData.serverPort)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", envData.serverPort), nil))
}

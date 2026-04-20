package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/gorilla/websocket"
)

type Player struct {
	ID string  `json:"id"`
	X  float64 `json:"x"`
	Z  float64 `json:"z"`
}

var players = make(map[string]Player)
var clients = make(map[*websocket.Conn]string)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	log.Println("New connection")

	var id string

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Println("Disconnected:", id)
			delete(clients, conn)
			delete(players, id)
			conn.Close()
			break
		}

		var p Player
		err = json.Unmarshal(msg, &p)
		if err != nil {
			continue
		}

		// 🔥 SET ID ONLY ONCE
		if id == "" {
			id = p.ID
			clients[conn] = id
		}

		// 🔥 ALWAYS use same ID
		p.ID = id
		players[id] = p

		broadcast()
	}
}
func broadcast() {
	data, _ := json.Marshal(players)

	// 🔥 DEBUG: shows all players in logs
	log.Println("PLAYERS:", players)

	for conn := range clients {
		err := conn.WriteMessage(websocket.TextMessage, data)
		if err != nil {
			conn.Close()
			delete(clients, conn)
		}
	}
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/ws", wsHandler)

	log.Println("🚀 Server running on port", port)

	err := http.ListenAndServe(":"+port, nil)
	if err != nil {
		log.Fatal("Server error:", err)
	}
}

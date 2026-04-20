package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/gorilla/websocket"
)

type Player struct {
	X float64 `json:"x"`
	Z float64 `json:"z"`
}

var players = make(map[string]Player)
var clients = make(map[*websocket.Conn]string)

var nextID = 1
var mu sync.Mutex

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	// 🔥 Assign ID (simple incremental)
	mu.Lock()
	id := strconv.Itoa(nextID)
	nextID++
	clients[conn] = id
	mu.Unlock()

	log.Println("Player connected:", id)

	// 🔥 SEND ID TO CLIENT (NEW)
	err = conn.WriteJSON(map[string]string{
		"your_id": id,
	})
	if err != nil {
		log.Println("Error sending ID:", err)
		conn.Close()
		return
	}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Println("Disconnected:", id)

			mu.Lock()
			delete(players, id)
			delete(clients, conn)
			mu.Unlock()

			conn.Close()
			break
		}

		var p Player
		err = json.Unmarshal(msg, &p)
		if err != nil {
			continue
		}

		mu.Lock()

		// update player state
		players[id] = p

		// 🔥 broadcast to ALL clients
		data, _ := json.Marshal(players)

		for c := range clients {
			err := c.WriteMessage(websocket.TextMessage, data)
			if err != nil {
				c.Close()
				delete(clients, c)
			}
		}

		mu.Unlock()
	}
}
func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/ws", wsHandler)

	log.Println("🚀 Server running on port", port)
	http.ListenAndServe(":"+port, nil)
}

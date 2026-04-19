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
		return
	}

	id := conn.RemoteAddr().String()
	clients[conn] = id

	log.Println("Player connected:", id)

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			delete(clients, conn)
			delete(players, id)
			conn.Close()
			break
		}

		var p Player
		json.Unmarshal(msg, &p)

		p.ID = id
		players[id] = p

		broadcast()
	}
}

func broadcast() {
	data, _ := json.Marshal(players)

	for conn := range clients {
		conn.WriteMessage(websocket.TextMessage, data)
	}
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/ws", wsHandler)

	log.Println("Server running on port", port)
	http.ListenAndServe(":"+port, nil)
}

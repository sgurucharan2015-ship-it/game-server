package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		fmt.Println("Upgrade error:", err)
		return
	}
	defer conn.Close()

	for {
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		conn.WriteMessage(mt, msg)
	}
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/ws", wsHandler)

	fmt.Println("Server running on port", port)
	http.ListenAndServe(":"+port, nil)
}
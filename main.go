package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	hostTimeout   = 30 * time.Second
	memberTimeout = 35 * time.Second
	maxEvents     = 4096
	maxBodyBytes  = 4 << 20 // 4 MiB
)

type Pose struct {
	X, Y, Z    float64
	Yaw, Pitch float64
	Health     float64
	Hunger     float64
	Selected   int
	Held       int
	Inventory  string
}

type Member struct {
	ID       int
	Name     string
	Skin     int
	Token    string
	LastSeen time.Time
	Pose     Pose
}

type Event struct {
	Seq      uint64
	SenderID int
	Type     string
	Data     string
}

type Room struct {
	Code             string
	Limit            int // 0 means unlimited
	LimitEnabled     bool
	Public           bool
	ServerName       string
	Description      string
	HostToken        string
	NextID           int
	NextSeq          uint64
	Created          time.Time
	Config           map[string]string
	Snapshot         string
	SnapshotVersion  uint64
	FullState        string
	FullStateVersion uint64
	Members          map[string]*Member // token -> member
	Events           []Event
}

var state = struct {
	sync.Mutex
	Rooms map[string]*Room
}{Rooms: make(map[string]*Room)}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", root)
	mux.HandleFunc("/health", health)
	mux.HandleFunc("/public/list", publicList)
	mux.HandleFunc("/create", createRoom)
	mux.HandleFunc("/join", joinRoom)
	mux.HandleFunc("/leave", leaveRoom)
	mux.HandleFunc("/stop", stopRoom)
	mux.HandleFunc("/event", postEvent)
	mux.HandleFunc("/tick", tick)
	mux.HandleFunc("/snapshot/set", setSnapshot)
	mux.HandleFunc("/snapshot/get", getSnapshot)

	go reaper()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           withCommonHeaders(mux),
		ReadHeaderTimeout: 8 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("VoxelCraft relay listening on :%s", port)
	log.Fatal(srv.ListenAndServe())
}

func withCommonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func root(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "VoxelCraft temporary-room relay is online.")
}

func health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "OK")
}

func readKV(w http.ResponseWriter, r *http.Request) (map[string]string, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, false
	}
	values := make(map[string]string)
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n") {
		if line == "" {
			continue
		}
		if i := strings.IndexByte(line, '='); i >= 0 {
			k := strings.TrimSpace(line[:i])
			v := line[i+1:]
			values[k] = v
		}
	}
	return values, true
}

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
		if b.Len() >= 16 {
			break
		}
	}
	if b.Len() == 0 {
		return "Player"
	}
	return b.String()
}

func sanitizeListingText(s string, maxLen int, fallback string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "/")

	var b strings.Builder
	for _, r := range s {
		if r < 32 {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= maxLen {
			break
		}
	}

	result := strings.TrimSpace(b.String())
	if result == "" {
		return fallback
	}
	return result
}

func cleanEventData(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "/")
	if len(s) > 768 {
		s = s[:768]
	}
	return s
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func atoi(s string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fallback
	}
	return n
}

func atof(s string, fallback float64) float64 {
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return fallback
	}
	return n
}

func token() string {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should be available on Render; this fallback is only
		// here to avoid crashing the relay if the OS entropy source fails.
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func newRoomCodeLocked() (string, bool) {
	var buf [4]byte
	for tries := 0; tries < 100; tries++ {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", false
		}
		n := int(uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3]))
		if n < 0 {
			n = -n
		}
		code := fmt.Sprintf("%06d", 100000+n%900000)
		if _, exists := state.Rooms[code]; !exists {
			return code, true
		}
	}
	return "", false
}

var configKeys = []string{
	"worldName", "seed", "gameMode", "difficulty", "worldType", "singleBiome",
	"allowCommands", "generateStructures", "bonusChest", "keepInventory",
	"doMobSpawning", "doWaterFlow", "doDaylightCycle", "experiments",
	"pvpEnabled",
	"voidGateBuilt", "voidUnlocked", "voidArenaBuilt", "gameWon", "advancementMask",
}

func publicList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	type publicRoom struct {
		Code        string
		Name        string
		Description string
		Players     int
		Limit       int
		PVP         bool
		Created     time.Time
	}

	state.Lock()
	rooms := make([]publicRoom, 0, len(state.Rooms))
	for _, room := range state.Rooms {
		if !room.Public {
			continue
		}
		rooms = append(rooms, publicRoom{
			Code:        room.Code,
			Name:        room.ServerName,
			Description: room.Description,
			Players:     len(room.Members),
			Limit:       room.Limit,
			PVP:         room.Config["pvpEnabled"] == "1",
			Created:     room.Created,
		})
	}
	state.Unlock()

	// Keep the browser stable instead of exposing Go map iteration order.
	sort.Slice(rooms, func(i, j int) bool {
		if strings.EqualFold(rooms[i].Name, rooms[j].Name) {
			return rooms[i].Created.Before(rooms[j].Created)
		}
		return strings.ToLower(rooms[i].Name) < strings.ToLower(rooms[j].Name)
	})

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "OK|%d\n", len(rooms))
	for _, room := range rooms {
		pvp := 0
		if room.PVP {
			pvp = 1
		}
		fmt.Fprintf(
			w,
			"R|%s|%s|%s|%d|%d|%d\n",
			room.Code,
			room.Name,
			room.Description,
			room.Players,
			room.Limit,
			pvp,
		)
	}
}

func createRoom(w http.ResponseWriter, r *http.Request) {
	v, ok := readKV(w, r)
	if !ok {
		return
	}

	name := sanitizeName(v["name"])
	skin := clampInt(atoi(v["skin"], 0), 0, 7)
	serverName := sanitizeListingText(v["serverName"], 48, name+"'s Server")
	description := sanitizeListingText(v["description"], 180, "No description.")
	isPublic := strings.TrimSpace(v["public"]) == "1"
	limitEnabled := strings.TrimSpace(v["limitEnabled"]) != "0"
	limit := 0
	if limitEnabled {
		limit = clampInt(atoi(v["limit"], 4), 2, 32)
	}

	state.Lock()
	defer state.Unlock()

	code, good := newRoomCodeLocked()
	if !good {
		writeErr(w, "ROOM_CODE_FAILED", "Could not allocate a room number")
		return
	}

	hostToken := token()
	cfg := make(map[string]string)
	for _, k := range configKeys {
		cfg[k] = v[k]
	}

	host := &Member{
		ID:       1,
		Name:     name,
		Skin:     skin,
		Token:    hostToken,
		LastSeen: time.Now(),
		Pose:     Pose{Health: 20},
	}

	room := &Room{
		Code:         code,
		Limit:        limit,
		LimitEnabled: limitEnabled,
		Public:       isPublic,
		ServerName:   serverName,
		Description:  description,
		HostToken:    hostToken,
		NextID:       2,
		NextSeq:      1,
		Created:      time.Now(),
		Config:       cfg,
		Members:      map[string]*Member{hostToken: host},
	}
	state.Rooms[code] = room

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "ok=1\nroom=%s\ntoken=%s\nplayerId=1\nlimit=%d\n", code, hostToken, limit)
	fmt.Fprintf(w, "serverName=%s\ndescription=%s\npublic=%d\nlimitEnabled=%d\n",
		room.ServerName, room.Description, boolInt(room.Public), boolInt(room.LimitEnabled))
}

func joinRoom(w http.ResponseWriter, r *http.Request) {
	v, ok := readKV(w, r)
	if !ok {
		return
	}

	code := strings.TrimSpace(v["room"])
	name := sanitizeName(v["name"])
	skin := clampInt(atoi(v["skin"], 0), 0, 7)

	state.Lock()
	defer state.Unlock()

	room := state.Rooms[code]
	if room == nil {
		writeErr(w, "ROOM_NOT_FOUND", "That temporary room does not exist")
		return
	}
	if room.LimitEnabled && room.Limit > 0 && len(room.Members) >= room.Limit {
		writeErr(w, "SERVER_FULL", "That temporary room is full")
		return
	}

	t := token()
	id := room.NextID
	room.NextID++
	room.Members[t] = &Member{
		ID: id, Name: name, Skin: skin, Token: t,
		LastSeen: time.Now(), Pose: Pose{Health: 20},
	}
	appendEventLocked(room, 0, "SYSTEM", fmt.Sprintf("%s joined the room", name))

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "ok=1\nroom=%s\ntoken=%s\nplayerId=%d\nlimit=%d\n", code, t, id, room.Limit)
	fmt.Fprintf(w, "serverName=%s\ndescription=%s\npublic=%d\nlimitEnabled=%d\n",
		room.ServerName, room.Description, boolInt(room.Public), boolInt(room.LimitEnabled))
	for _, k := range configKeys {
		fmt.Fprintf(w, "%s=%s\n", k, room.Config[k])
	}
}

func memberForLocked(room *Room, t string) *Member {
	if room == nil {
		return nil
	}
	return room.Members[t]
}

func leaveRoom(w http.ResponseWriter, r *http.Request) {
	v, ok := readKV(w, r)
	if !ok {
		return
	}

	state.Lock()
	defer state.Unlock()

	room := state.Rooms[strings.TrimSpace(v["room"])]
	if room == nil {
		writeErr(w, "ROOM_GONE", "Room already closed")
		return
	}
	t := strings.TrimSpace(v["token"])
	member := room.Members[t]
	if member == nil {
		writeErr(w, "BAD_TOKEN", "Unknown player token")
		return
	}

	if t == room.HostToken {
		delete(state.Rooms, room.Code)
		fmt.Fprintln(w, "ok=1")
		return
	}

	delete(room.Members, t)
	appendEventLocked(room, 0, "SYSTEM", fmt.Sprintf("%s left the room", member.Name))
	fmt.Fprintln(w, "ok=1")
}

func stopRoom(w http.ResponseWriter, r *http.Request) {
	v, ok := readKV(w, r)
	if !ok {
		return
	}

	state.Lock()
	defer state.Unlock()

	room := state.Rooms[strings.TrimSpace(v["room"])]
	if room == nil {
		writeErr(w, "ROOM_GONE", "Room already closed")
		return
	}
	if strings.TrimSpace(v["token"]) != room.HostToken {
		writeErr(w, "NOT_HOST", "Only the host can stop this room")
		return
	}
	delete(state.Rooms, room.Code)
	fmt.Fprintln(w, "ok=1")
}

func postEvent(w http.ResponseWriter, r *http.Request) {
	v, ok := readKV(w, r)
	if !ok {
		return
	}

	state.Lock()
	defer state.Unlock()

	room := state.Rooms[strings.TrimSpace(v["room"])]
	if room == nil {
		writeErr(w, "ROOM_GONE", "Room closed")
		return
	}
	member := memberForLocked(room, strings.TrimSpace(v["token"]))
	if member == nil {
		writeErr(w, "BAD_TOKEN", "Unknown player token")
		return
	}
	member.LastSeen = time.Now()

	typ := strings.ToUpper(strings.TrimSpace(v["type"]))

	// PvP is chosen by the room host. Validate it on the public relay too,
	// so a modified client cannot bypass a combat-disabled room or hit from
	// across the map.
	if typ == "PVP" {
		if room.Config["pvpEnabled"] != "1" {
			writeErr(w, "PVP_DISABLED", "Player combat is disabled in this room")
			return
		}

		parts := strings.Split(cleanEventData(v["data"]), ",")
		if len(parts) < 2 {
			writeErr(w, "BAD_PVP", "Malformed player combat event")
			return
		}

		targetID := atoi(strings.TrimSpace(parts[0]), -1)
		damage, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err != nil || targetID <= 0 || targetID == member.ID {
			writeErr(w, "BAD_PVP", "Invalid combat target or damage")
			return
		}

		var target *Member
		for _, candidate := range room.Members {
			if candidate.ID == targetID {
				target = candidate
				break
			}
		}
		if target == nil {
			writeErr(w, "BAD_PVP", "Target player is no longer in the room")
			return
		}

		dx := member.Pose.X - target.Pose.X
		dy := member.Pose.Y - target.Pose.Y
		dz := member.Pose.Z - target.Pose.Z
		if dx*dx+dy*dy+dz*dz > 64.0 {
			writeErr(w, "PVP_TOO_FAR", "Target is out of combat range")
			return
		}

		if damage < 0.0 {
			damage = 0.0
		}
		if damage > 10.0 {
			damage = 10.0
		}

		appendEventLocked(room, member.ID, "PVP", fmt.Sprintf("%d,%.2f", targetID, damage))
		fmt.Fprintln(w, "ok=1")
		return
	}

	switch typ {
	case "BLOCK", "BREAK", "PLACE", "CHAT", "SYSTEM", "HIT", "PICKUP", "GRANT", "PROGRESS", "DAMAGE":
	default:
		writeErr(w, "BAD_EVENT", "Unsupported event type")
		return
	}

	appendEventLocked(room, member.ID, typ, cleanEventData(v["data"]))
	fmt.Fprintln(w, "ok=1")
}

func appendEventLocked(room *Room, sender int, typ, data string) {
	e := Event{Seq: room.NextSeq, SenderID: sender, Type: typ, Data: data}
	room.NextSeq++
	room.Events = append(room.Events, e)
	if len(room.Events) > maxEvents {
		room.Events = append([]Event(nil), room.Events[len(room.Events)-maxEvents:]...)
	}
}

func tick(w http.ResponseWriter, r *http.Request) {
	v, ok := readKV(w, r)
	if !ok {
		return
	}

	state.Lock()
	defer state.Unlock()

	code := strings.TrimSpace(v["room"])
	room := state.Rooms[code]
	if room == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ERR|ROOM_GONE|Host stopped or disconnected")
		return
	}

	t := strings.TrimSpace(v["token"])
	member := memberForLocked(room, t)
	if member == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ERR|BAD_TOKEN|Session is no longer valid")
		return
	}

	member.LastSeen = time.Now()
	member.Pose = Pose{
		X:         atof(v["x"], member.Pose.X),
		Y:         atof(v["y"], member.Pose.Y),
		Z:         atof(v["z"], member.Pose.Z),
		Yaw:       atof(v["yaw"], member.Pose.Yaw),
		Pitch:     atof(v["pitch"], member.Pose.Pitch),
		Health:    atof(v["health"], member.Pose.Health),
		Hunger:    atof(v["hunger"], member.Pose.Hunger),
		Selected:  clampInt(atoi(v["selected"], member.Pose.Selected), 0, 8),
		Held:      clampInt(atoi(v["held"], member.Pose.Held), 0, 255),
		Inventory: v["inventory"],
	}

	// The room host is the sole authority for dynamic world simulation.
	// FullState contains day/night, mobs, projectiles, boss state and other
	// shared runtime state.  It intentionally lives only in RAM.
	if t == room.HostToken {
		if fs := strings.TrimSpace(v["fullState"]); fs != "" {
			if len(fs) <= maxBodyBytes/2 && fs != room.FullState {
				room.FullState = fs
				room.FullStateVersion++
			}
		}
	}

	since, _ := strconv.ParseUint(strings.TrimSpace(v["since"]), 10, 64)
	stateVersion, _ := strconv.ParseUint(strings.TrimSpace(v["stateVersion"]), 10, 64)
	snapshotVersion, _ := strconv.ParseUint(strings.TrimSpace(v["snapshotVersion"]), 10, 64)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "OK|%d|%d|%d\n", room.NextSeq-1, len(room.Members), room.Limit)

	for _, m := range room.Members {
		fmt.Fprintf(w, "P|%d|%s|%d|%.4f|%.4f|%.4f|%.5f|%.5f|%.2f|%.2f|%d|%d|%s\n",
			m.ID, m.Name, m.Skin,
			m.Pose.X, m.Pose.Y, m.Pose.Z,
			m.Pose.Yaw, m.Pose.Pitch, m.Pose.Health,
			m.Pose.Hunger, m.Pose.Selected, m.Pose.Held, m.Pose.Inventory)
	}

	if room.FullState != "" && stateVersion < room.FullStateVersion {
		fmt.Fprintf(w, "S|%d|%s\n", room.FullStateVersion, room.FullState)
	}

	if snapshotVersion < room.SnapshotVersion {
		fmt.Fprintf(w, "W|%d|%s\n", room.SnapshotVersion, room.Snapshot)
	}

	for _, e := range room.Events {
		if e.Seq > since {
			fmt.Fprintf(w, "E|%d|%d|%s|%s\n", e.Seq, e.SenderID, e.Type, e.Data)
		}
	}
}

func setSnapshot(w http.ResponseWriter, r *http.Request) {
	v, ok := readKV(w, r)
	if !ok {
		return
	}

	state.Lock()
	defer state.Unlock()

	room := state.Rooms[strings.TrimSpace(v["room"])]
	if room == nil {
		writeErr(w, "ROOM_GONE", "Room closed")
		return
	}
	t := strings.TrimSpace(v["token"])
	if t != room.HostToken {
		writeErr(w, "NOT_HOST", "Only the host can publish the world snapshot")
		return
	}
	if m := room.Members[t]; m != nil {
		m.LastSeen = time.Now()
	}
	nextSnapshot := v["snapshot"]
	if nextSnapshot != room.Snapshot {
		room.Snapshot = nextSnapshot
		room.SnapshotVersion++
	}
	fmt.Fprintf(w, "ok=1\nversion=%d\n", room.SnapshotVersion)
}

func getSnapshot(w http.ResponseWriter, r *http.Request) {
	v, ok := readKV(w, r)
	if !ok {
		return
	}

	state.Lock()
	defer state.Unlock()

	room := state.Rooms[strings.TrimSpace(v["room"])]
	if room == nil {
		writeErr(w, "ROOM_GONE", "Room closed")
		return
	}
	m := memberForLocked(room, strings.TrimSpace(v["token"]))
	if m == nil {
		writeErr(w, "BAD_TOKEN", "Unknown player token")
		return
	}
	m.LastSeen = time.Now()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok=1")
	fmt.Fprintf(w, "version=%d\n", room.SnapshotVersion)
	fmt.Fprintf(w, "snapshot=%s\n", room.Snapshot)
}

func writeErr(w http.ResponseWriter, code, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "ok=0\nerror=%s\nmessage=%s\n", code, strings.ReplaceAll(message, "\n", " "))
}

func reaper() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for now := range ticker.C {
		state.Lock()
		for code, room := range state.Rooms {
			host := room.Members[room.HostToken]
			if host == nil || now.Sub(host.LastSeen) > hostTimeout {
				delete(state.Rooms, code)
				continue
			}

			for t, m := range room.Members {
				if t == room.HostToken {
					continue
				}
				if now.Sub(m.LastSeen) > memberTimeout {
					delete(room.Members, t)
					appendEventLocked(room, 0, "SYSTEM", fmt.Sprintf("%s timed out", m.Name))
				}
			}
		}
		state.Unlock()
	}
}

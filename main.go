package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"html"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/gorilla/mux"
	_ "github.com/mattn/go-sqlite3"
)

var (
	pageHook      = ""
	userHook      = ""
	metrics       = "http://localhost:2019/metrics"
	db            *sql.DB
	dbMutex       sync.RWMutex
	reloadEvery   = 5 * time.Minute
	monitorPeriod = 30 * 24 * 60 * 60
)

const (
	// <userRoot>/<user>/preferences.json is the live file;
	// <userRoot>/<user>/preferences.json.<unix timestamp>.archive are old revisions.
	userRoot = "/usr/src/anodyne/anodyne-frontend/user"
	maxBody  = 1 << 20 // 1 MiB
	fileMode = 0o644   // world-readable so Caddy's file_server can serve it
)

type Status struct {
	Uptime float64 `json:"uptime"`
}

type Link struct {
	Name string `json:"Name"`
	Url  string `json:"Url"`
}

type Psyche struct {
	Interests  []string `json:"Interests"`
	Attributes []string `json:"Attributes"`
}

type DrugCategory struct {
	Title   string   `json:"Title"` // name
	Entries []string `json:"Entries"`
}
type IngestionSummary struct {
	Substance string `json:"Substance"`
	Route     string `json:"Route"`
	Dosage    string `json:"Dosage"`
}
type Accident struct {
	Ingestions []IngestionSummary `json:"Ingestions"`
	Notes      []string           `json:"Notes"`
}

type Complication struct {
	Title   []string   `json:"Title"` // name
	Entries []Accident `json:"Entries"`
}

type Route struct {
	Title    string   `json:"Title"` // name
	Dosages  []string `json:"Dosages"`
	Onset    string   `json:"Onset"`
	Duration string   `json:"Duration"`
}
type SubjectiveEffect struct {
	Title string `json:"Title"`
	// <Light>,<Moderate/Common>,<Strong/Heavy>,<Severe/Intense/Extreme>,<Etc>
	Severity    string `json:"Severity"`
	EffectIndex string `json:"EffectIndex"`
}
type Substance struct {
	Title   string             `json:"Title"` // name
	Routes  []Route            `json:"Routes"`
	Salts   []string           `json:"Salts"`
	Effects []SubjectiveEffect `json:"Effects"`
	Notes   []string           `json:"Notes"`
}
type UserPage struct {
	Title         string         `json:"Title"`
	Name          []string       `json:"Names"`
	About         string         `json:"About"`
	Location      string         `json:"Location"`
	Birth         string         `json:"Birth"`
	Deceased      string         `json:"Deceased"`
	Links         []Link         `json:"Links"`
	Psyche        Psyche         `json:"Psyche"`
	Preferences   []DrugCategory `json:"Preferences"`
	Substances    []Substance    `json:"Substances"`
	Complications []Complication `json:"Complications"`
}

func fetchUptimeSeconds() (float64, error) {
	resp, err := http.Get(metrics)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "caddy_admin_uptime_seconds") {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				continue
			}
			return strconv.ParseFloat(parts[1], 64)
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, nil // uptime metric not found
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	uptimeSeconds, err := fetchUptimeSeconds()
	if err != nil {
		http.Error(w, "Failed to fetch metrics: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if uptimeSeconds == 0 {
		http.Error(w, "Uptime metric not found", http.StatusInternalServerError)
		return
	}

	uptimePercent := (uptimeSeconds / float64(monitorPeriod)) * 100
	if uptimePercent > 100 {
		uptimePercent = 100
	}

	status := Status{
		Uptime: round(uptimePercent, 2),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func round(val float64, precision int) float64 {
	pow := 1.0
	for i := 0; i < precision; i++ {
		pow *= 10
	}
	return float64(int(val*pow+0.5)) / pow
}

// Strict whitelist: prevents path traversal ("../") via the {user} segment.
var validUser = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
 
// One mutex per user so concurrent uploads for the same user are serialized.
var (
	locksMu sync.Mutex
	locks   = map[string]*sync.Mutex{}
)
 
func userLock(user string) *sync.Mutex {
	locksMu.Lock()
	defer locksMu.Unlock()
	if m, ok := locks[user]; ok {
		return m
	}
	m := &sync.Mutex{}
	locks[user] = m
	return m
}
 
func etagOf(b []byte) string {
	h := sha256.Sum256(b)
	return `"` + hex.EncodeToString(h[:16]) + `"`
}
 
// writeAtomic writes to a temp file in the same directory, fsyncs, then renames,
// so readers (and Caddy's file_server) never see a half-written file.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), fileMode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
 
// archiveCurrent preserves the live file as <path>.<unix timestamp>.archive and
// returns the archive's base name ("" if there was no live file yet).
//
// It hard-links instead of renaming, so the live file never disappears: the
// archive and the live file share the old contents until writeAtomic swaps a new
// file into place. If two uploads land in the same second, a numeric suffix keeps
// the archive names unique.
func archiveCurrent(path string) (string, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
 
	ts := time.Now().Unix()
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("%s.%d.archive", path, ts)
		if i > 0 {
			name = fmt.Sprintf("%s.%d-%d.archive", path, ts, i)
		}
		err := os.Link(path, name)
		if err == nil {
			return filepath.Base(name), nil
		}
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		// Filesystem without hard-link support: fall back to a copy.
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return "", rerr
		}
		f, oerr := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
		if errors.Is(oerr, fs.ErrExist) {
			continue
		}
		if oerr != nil {
			return "", oerr
		}
		if _, werr := f.Write(data); werr != nil {
			f.Close()
			os.Remove(name)
			return "", werr
		}
		if cerr := f.Close(); cerr != nil {
			os.Remove(name)
			return "", cerr
		}
		return filepath.Base(name), nil
	}
	return "", fmt.Errorf("could not find a free archive name for %s", path)
}
 
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("no .env file found, relying on real env vars")
	}
	pageHook = os.Getenv("PAGE_HOOK")
	userHook = os.Getenv("USER_HOOK")
	//db, err := sql.Open("sqlite3", "file:db.sqlite?_journal_mode=WAL&_busy_timeout=5000")
	err := loadDB()
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	go func() {
		for range time.Tick(reloadEvery) {
			if err := reloadDB(); err != nil {
				log.Printf("Failed to reload DB: %v", err)
			}
		}
	}()
	r := mux.NewRouter()
	r.HandleFunc("/structure/{substance}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		input := /*strings.ReplaceAll(*/strings.TrimSpace(vars["substance"])/*, "_", " ")*/
		if input == "" {
			http.Error(w, "Substance name or abbreviation required in URL", http.StatusBadRequest)
			return
		}
		var titleE string
		var abrE string
		var jsonData string

		query := `
			SELECT title, aliases, data_json
			FROM substances
			WHERE title = ? COLLATE NOCASE
			  OR EXISTS (
				  SELECT 1 FROM json_each(aliases)
				  WHERE json_each.value = ? COLLATE NOCASE
			  )
			LIMIT 1
		`
		row := db.QueryRow(query, input, input)
		err := row.Scan(&titleE, &abrE, &jsonData)

		if err == sql.ErrNoRows {
			w.Header().Set("Content-Type", "application/json")
			fmt.Println("missing")
			return
		}
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(jsonData), &data); err != nil {
			http.Error(w, "Invalid JSON in database", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if data["Structure"] != nil {
			fmt.Fprintf(w, html.UnescapeString(data["Structure"].(string)))
		}
	})
	r.HandleFunc("/substance/{substance}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		input := strings.ReplaceAll(strings.TrimSpace(vars["substance"]), "_", " ")
		if input == "" {
			http.Error(w, "Substance name or abbreviation required in URL", http.StatusBadRequest)
			return
		}
		var titleE string
		var abrE string
		var jsonData string

		query := `
			SELECT title, aliases, data_json
			FROM substances
			WHERE title = ? COLLATE NOCASE
			  OR EXISTS (
				  SELECT 1 FROM json_each(aliases)
				  WHERE json_each.value = ? COLLATE NOCASE
			  )
			LIMIT 1
		`
		row := db.QueryRow(query, input, input)
		err := row.Scan(&titleE, &abrE, &jsonData)

		if err == sql.ErrNoRows {
			fail_data := make(map[string]interface{})
			fail_data["Title"] = input
			fail_data["NotFound"] = true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(fail_data)
			log.Println(fail_data)
			//http.Error(w, "substance not found", http.StatusNotFound)
			return
		}
		log.Println("{ \"Title\": \"" + input + "\" }")
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(jsonData), &data); err != nil {
			http.Error(w, "Invalid JSON in database", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	})
	r.HandleFunc("/route/{route}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		input := strings.ReplaceAll(strings.TrimSpace(vars["route"]), "_", " ")
		log.Println(input)
		if input == "" {
			http.Error(w, "Substance name or abbreviation required in URL", http.StatusBadRequest)
			return
		}
		var titleE string
		var abrE string
		var sitesE string
		var jsonData string

		query := `
			SELECT title, aliases, sites, data_json
			FROM routes
			WHERE title = ? COLLATE NOCASE
			LIMIT 1
		`
		row := db.QueryRow(query, input)
		err := row.Scan(&titleE, &abrE, &sitesE, &jsonData)

		if err == sql.ErrNoRows {
			fail_data := make(map[string]interface{})
			fail_data["Title"] = input
			fail_data["NotFound"] = true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(fail_data)
			log.Println(fail_data)
			//http.Error(w, "substance not found", http.StatusNotFound)
			return
		}
		log.Println(abrE)
		log.Println("{ \"Title\": \"" + input + "\" }")
		log.Println(sitesE)
		log.Println(jsonData)
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(jsonData), &data); err != nil {
			http.Error(w, "Invalid JSON in database", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	})
	r.HandleFunc("/composite/{substance}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		input := strings.ReplaceAll(strings.TrimSpace(vars["substance"]), "_", " ")
		log.Println(input)
		if input == "" {
			http.Error(w, "Substance name or abbreviation required in URL", http.StatusBadRequest)
			return
		}
		var titleE string
		var abrE string
		var subsE string
		var jsonData string

		query := `
			SELECT title, aliases, substances, data_json
			FROM composites
			WHERE title = ? COLLATE NOCASE
			LIMIT 1
		`
		row := db.QueryRow(query, input)
		err := row.Scan(&titleE, &abrE, &subsE, &jsonData)

		if err == sql.ErrNoRows {
			fail_data := make(map[string]interface{})
			fail_data["Title"] = input
			fail_data["NotFound"] = true
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(fail_data)
			log.Println(fail_data)
			//http.Error(w, "substance not found", http.StatusNotFound)
			return
		}
		log.Println(abrE)
		log.Println("{ \"Title\": \"" + input + "\" }")
		log.Println(subsE)
		log.Println(jsonData)
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(jsonData), &data); err != nil {
			http.Error(w, "Invalid JSON in database", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	})
	r.HandleFunc("/protein/{protein}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		input := /*strings.ReplaceAll(*/strings.TrimSpace(vars["protein"])/*, "_", " ")*/
		if input == "" {
			http.Error(w, "Protein name or abbreviation required in URL", http.StatusBadRequest)
			return
		}
		var titleE string
		var abrE string
		var jsonData string

		query := `
			SELECT title, aliases, data_json
			FROM proteins
			WHERE title = ? COLLATE NOCASE
			  OR EXISTS (
				  SELECT 1 FROM json_each(aliases)
				  WHERE json_each.value = ? COLLATE NOCASE
			  )
			LIMIT 1
		`
		row := db.QueryRow(query, input, input)
		err := row.Scan(&titleE, &abrE, &jsonData)

		if err == sql.ErrNoRows {
			fail_data := make(map[string]interface{})
			fail_data["Title"] = input
			fail_data["NotFound"] = "true"
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(fail_data)
			log.Println(fail_data)
			//http.Error(w, "protein not found", http.StatusNotFound)
			return
		}
		log.Println("{ \"Title\": \"" + input + "\" }")
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(jsonData), &data); err != nil {
			http.Error(w, "Invalid JSON in database", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	})
	r.HandleFunc("/status", statusHandler)
	r.HandleFunc("/usersync/{user}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		user := vars["user"]

		if !validUser.MatchString(user) {
			http.Error(w, "invalid user", http.StatusBadRequest)
			return
		}

		// Identity comes from Caddy (basic_auth or forward_auth), never from the client.
		// This service must only listen on localhost so the header can't be spoofed.
		authed := r.Header.Get("X-Remote-User")
		if authed == "" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		if authed != user {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
 
		current := filepath.Join(userRoot, user, "preferences.json")
 
		switch r.Method {
		case http.MethodGet:
			b, err := os.ReadFile(current)
			if errors.Is(err, fs.ErrNotExist) {
				http.Error(w, "no data yet", http.StatusNotFound)
				return
			}
			if err != nil {
				log.Printf("read %s: %v", current, err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("ETag", etagOf(b))
			w.Write(b)
 
		case http.MethodPut, http.MethodPost:
			// Requiring application/json forces a CORS preflight on cross-origin
			// requests, which blocks CSRF using the browser's stored credentials.
			mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if mt != "application/json" {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
 
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
				return
			}
 
			// json.Indent validates and pretty-prints without round-tripping through
			// Go types, so large integers and key order are preserved exactly.
			var buf bytes.Buffer
			if err := json.Indent(&buf, raw, "", "  "); err != nil {
				http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			buf.WriteByte('\n')
			body := buf.Bytes()
			// TODO: validate against your JSON Schema here.
 
			mu := userLock(user)
			mu.Lock()
			defer mu.Unlock()
 
			// Optimistic concurrency: if the client sends If-Match with the ETag it
			// last fetched, reject the upload when someone else changed the file since.
			if match := r.Header.Get("If-Match"); match != "" {
				old, err := os.ReadFile(current)
				if err != nil && !errors.Is(err, fs.ErrNotExist) {
					log.Printf("read %s: %v", current, err)
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				if etagOf(old) != match {
					http.Error(w, "file changed since you loaded it", http.StatusPreconditionFailed)
					return
				}
			}
 
			// 1. Archive the current preferences.json (if any) as
			//    preferences.json.<unix timestamp>.archive.
			archived, err := archiveCurrent(current)
			if err != nil {
				log.Printf("archive %s: %v", current, err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
 
			// 2. Atomically replace preferences.json with the received data.
			if err := writeAtomic(current, body); err != nil {
				log.Printf("write %s: %v", current, err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
 
			etag := etagOf(body)
			w.Header().Set("ETag", etag)
			writeJSON(w, http.StatusOK, map[string]string{"archived": archived, "etag": etag})
			log.Printf("user=%s archived=%q bytes=%d", user, archived, len(body))
 
		default:
			w.Header().Set("Allow", "GET, PUT, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	//r.HandleFunc("/userpage", func(w http.ResponseWriter, r *http.Request) {
	//	//vars := mux.Vars(r)
	//	if r.Method != http.MethodPost {
	//		http.Error(w, "Only POST allowed", http.StatusMethodNotAllowed)
	//		return
	//	}
	//
	//	body, err := io.ReadAll(r.Body)
	//	if err != nil {
	//		http.Error(w, "cannot read body", http.StatusBadRequest)
	//		return
	//	}
	//	defer r.Body.Close()

	//	var userData map[string]any
	//	var hookData map[string]any
	//	if userData, err = json.Unmarshal(body, &userData); err != nil {
	//		fmt.Println("Failed to parse json")
	//		return
	//	}

	//	out, err := json.MarshalIndent(page, "", "  ")
	//	if err != nil {
	//		return
	//	}

	//	fmt.Println("Commiting usepage: " + userData{}.Title + string(out))
	//
	//	payload := map[string]string{
	//		"content": "",
	//	}

	//	hookData, err := json.Marshal(payload)
	//	if err != nil {
	//		panic(err)
	//	}

	//	req, err := http.NewRequest("POST", userHook, bytes.NewBuffer(hookData))
	//	if err != nil {
	//		panic(err)
	//	}

	//	req.Header.Set("Accept",       "application/json")
	//	req.Header.Set("Content-Type", "application/json")

	//	client := &http.Client{}
	//	resp, err := client.Do(req)
	//	if err != nil {
	//		panic(err)
	//	}

	//	w.Header().Set("Content-Type", "application/json")
	//	http.Redirect(w, r, "/form/" + userData{}["Title"], http.StatusFound)
	//	fmt.Println("Request send: " + userData["Title"])
	//})
	r.HandleFunc("/request/{request}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		input := vars["request"]
		payload := map[string]string{
			"content": input,
		}
		jsonData, err := json.Marshal(payload)
		if err != nil {
			panic(err)
		}
		req, err := http.NewRequest("POST", pageHook, bytes.NewBuffer(jsonData))
		if err != nil {
			panic(err)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			panic(err)
		}
		defer resp.Body.Close()

		w.Header().Set("Content-Type", "application/json")
		//fmt.Fprintf(w, "{ \"Content\": \"" + input + "\" }")
		//fmt.Fprintf(w, "Request send!")
		http.Redirect(w, r, "/send/"+input, http.StatusFound)
		fmt.Println("Request send: " + input)
	})

	log.Fatal(http.ListenAndServe(":8080", r))
}

func loadDB() error {
	newDB, err := sql.Open("sqlite3", "file:db.sqlite?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return err
	}
	// Test connection
	if err := newDB.Ping(); err != nil {
		newDB.Close()
		return err
	}
	dbMutex.Lock()
	defer dbMutex.Unlock()
	if db != nil {
		db.Close()
	}
	db = newDB
	log.Println("Database loaded")
	return nil
}

func reloadDB() error {
	return loadDB()
}

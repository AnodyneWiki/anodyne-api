package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"os"

	//"io"

	//"strings"
	"fmt"
	"html"
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
		input := /*strings.ReplaceAll(*/ vars["substance"] /*, "_", " ")*/
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
		input := strings.ReplaceAll(vars["substance"], "_", " ")
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
	r.HandleFunc("/composite/{substance}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		input := strings.ReplaceAll(vars["substance"], "_", " ")
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
		input := /*strings.ReplaceAll(*/ vars["protein"] /*, "_", " ")*/
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

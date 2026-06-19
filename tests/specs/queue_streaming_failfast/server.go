//go:build ignore

// Test HTTP server: /search (queue producer) + /detail (queue consumers).
// Env: PORT, NUM_ITEMS (default 30), FAIL_ON_ID (422s that id), FAIL_DELAY_MS (default 150).
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

func main() {
	// Bind to a free port (PORT=0) and write it to PORT_FILE to avoid port races.
	port := os.Getenv("PORT")
	if port == "" {
		port = "0"
	}

	numItems := 30
	if v := os.Getenv("NUM_ITEMS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			numItems = n
		}
	}

	failOnID := os.Getenv("FAIL_ON_ID")
	failOnGroup := os.Getenv("FAIL_ON_GROUP") // only fail when ?grp= matches (empty = any)
	failDelay := 150 * time.Millisecond
	if v := os.Getenv("FAIL_DELAY_MS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			failDelay = time.Duration(n) * time.Millisecond
		}
	}

	mux := http.NewServeMux()

	// /search returns {"results":[{id}...]} for the producer to enqueue.
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[`)
		for i := 1; i <= numItems; i++ {
			if i > 1 {
				fmt.Fprint(w, ",")
			}
			fmt.Fprintf(w, `{"id":"item-%03d"}`, i)
		}
		fmt.Fprint(w, `]}`)
	})

	// /detail returns one id's record; FAIL_ON_ID (+ optional FAIL_ON_GROUP via
	// ?grp=) returns a non-retryable 422.
	mux.HandleFunc("/detail", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		grp := r.URL.Query().Get("grp")
		if failOnID != "" && id == failOnID && (failOnGroup == "" || grp == failOnGroup) {
			time.Sleep(failDelay)
			http.Error(w, `{"error":"injected failure for `+id+` grp=`+grp+`"}`, http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":%q,"name":"Name for %s","value":%d}`, id, id, len(id))
	})

	// readiness probe
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})

	addr := "127.0.0.1:" + port
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen error:", err)
		os.Exit(1)
	}

	chosen := ln.Addr().(*net.TCPAddr).Port
	if pf := os.Getenv("PORT_FILE"); pf != "" {
		if err := os.WriteFile(pf, []byte(strconv.Itoa(chosen)), 0644); err != nil {
			fmt.Fprintln(os.Stderr, "could not write PORT_FILE:", err)
			os.Exit(1)
		}
	}

	fmt.Fprintf(os.Stderr, "test server listening on 127.0.0.1:%d (num_items=%d fail_on_id=%q)\n", chosen, numItems, failOnID)
	if err := http.Serve(ln, mux); err != nil {
		fmt.Fprintln(os.Stderr, "server error:", err)
		os.Exit(1)
	}
}

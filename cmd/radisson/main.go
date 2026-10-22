// Command radisson prints Radisson hotel rates for a city and a stay as JSON.
//
//	export HYPER_API_KEY=...
//	export PROXY_URL='http://user-session-{session}:pass@proxy.example.com:8080'
//	go run ./cmd/radisson -city london -checkin 2026-11-20 -checkout 2026-11-22
//
// {session} in the proxy URL is replaced with a random id, so every run gets
// its own sticky residential IP.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Hyper-Solutions/radisson-scraper/radisson"
)

type output struct {
	Query     radisson.Query            `json:"query"`
	Place     radisson.Place            `json:"place"`
	ExitIP    string                    `json:"exitIp"`
	Bootstrap *radisson.BootstrapReport `json:"bootstrap"`
	Hotels    []hotel                   `json:"hotels"`
}

type hotel struct {
	*radisson.Hotel
	DistanceKm float64        `json:"distanceKm"`
	Rate       *radisson.Rate `json:"rate"`
}

func main() {
	city := flag.String("city", "london", "destination, resolved through the site's autocomplete")
	checkIn := flag.String("checkin", time.Now().AddDate(0, 0, 30).Format("2006-01-02"), "check-in date")
	checkOut := flag.String("checkout", time.Now().AddDate(0, 0, 32).Format("2006-01-02"), "check-out date")
	adults := flag.Int("adults", 2, "adults in the room")
	currency := flag.String("currency", "USD", "rate currency")
	out := flag.String("out", "", "write JSON here instead of stdout")
	proxy := flag.String("proxy", os.Getenv("PROXY_URL"), "sticky residential proxy URL; {session} is replaced with a random id")
	apiKey := flag.String("api-key", os.Getenv("HYPER_API_KEY"), "Hyper Solutions API key")
	debugProxy := flag.String("debug-proxy", "", "send traffic through a local debugging proxy instead, e.g. http://127.0.0.1:8888 for powhttp")
	flag.Parse()

	if *apiKey == "" {
		log.Fatal("set HYPER_API_KEY or -api-key")
	}
	opts := radisson.Options{APIKey: *apiKey, Currency: *currency}
	switch {
	case *debugProxy != "":
		opts.ProxyURL, opts.InsecureSkipVerify = *debugProxy, true
	case *proxy != "":
		opts.ProxyURL = strings.ReplaceAll(*proxy, "{session}", sessionID())
	}
	s, err := radisson.NewSession(opts)
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()

	res := output{Query: radisson.Query{CheckIn: *checkIn, CheckOut: *checkOut, Adults: *adults}}
	if res.Bootstrap, err = s.Bootstrap(ctx); err != nil {
		log.Fatalf("bootstrap: %v", err)
	}
	res.ExitIP = s.IP
	log.Printf("bootstrap ok from %s: %d sbsd posts, %d sensor posts", s.IP, res.Bootstrap.SbsdPosts, res.Bootstrap.SensorPosts)

	places, err := s.Autocomplete(ctx, *city)
	if err != nil {
		log.Fatalf("autocomplete: %v", err)
	}
	if len(places) == 0 {
		log.Fatalf("autocomplete: no place for %q", *city)
	}
	res.Place = places[0]
	res.Query.PlaceID = places[0].PlaceID

	refs, err := s.Hotels(ctx, res.Query)
	if err != nil {
		log.Fatalf("hotels: %v", err)
	}
	codes := make([]string, len(refs))
	for i, r := range refs {
		codes[i] = r.Code
	}
	rates, err := s.Rates(ctx, res.Query, codes)
	if err != nil {
		log.Fatalf("rates: %v", err)
	}
	byCode := map[string]*radisson.Rate{}
	for i := range rates {
		byCode[rates[i].Code] = &rates[i]
	}
	for _, r := range refs {
		h, err := s.Hotel(ctx, res.Query, r.Code)
		if err != nil {
			log.Fatalf("hotel %s: %v", r.Code, err)
		}
		res.Hotels = append(res.Hotels, hotel{Hotel: h, DistanceKm: r.Distance.Km, Rate: byCode[r.Code]})
	}
	log.Printf("%s: %d hotels, %d with a rate", res.Place.Name, len(res.Hotels), countAvailable(rates))

	enc := json.NewEncoder(os.Stdout)
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		enc = json.NewEncoder(f)
	}
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(res); err != nil {
		log.Fatal(err)
	}
}

func countAvailable(rates []radisson.Rate) int {
	n := 0
	for _, r := range rates {
		if r.Available {
			n++
		}
	}
	return n
}

func sessionID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

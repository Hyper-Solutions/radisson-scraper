# Radisson hotel rates, request-based

A Go scraper for [radissonhotels.com](https://www.radissonhotels.com) hotel rates that runs without a browser. It clears Akamai Bot Manager with the [Hyper Solutions API](https://hypersolutions.co) and then calls the same JSON endpoints the site's booking funnel calls.

It was written for the guest post on the Massive blog about reverse engineering the Radisson request flow.

## What it does

1. Loads the home page with a Chrome TLS and HTTP/2 fingerprint ([bogdanfinn/tls-client](https://github.com/bogdanfinn/tls-client)) and the headers, header order and client hints of a recorded Chrome 155 session.
2. Clears Akamai:
   * **SBSD.** If the home page is the SBSD challenge page (a script URL with `?v=` and `t=`), it answers it and loads the page again. On the real page it posts two SBSD payloads to the `?v=` script.
   * **Sensor.** It posts sensor data to the `_abck` script until the cookie carries the `~0~` stop signal, three posts at most.
   * **Pixel.** It answers the pixel challenge after the first sensor post.
3. Calls the booking funnel's APIs:
   * `GET /zimba-api/autocomplete/{text}` resolves a city to a Google place ID.
   * `GET /zimba-api/search-result/hotels/search?placeId=` lists the hotels for that place.
   * `POST /availability-api/multiple-hotels-rates` returns the cheapest rate of each hotel for the stay.
   * `GET /zimba-api/search-result/hotels?code=` returns a hotel's name, address and rating.

If an API call comes back with a challenge (`429` with an SBSD token, or an invalidated `_abck`), the session answers it and retries once.

## Usage

You need Go 1.27 or newer, a Hyper Solutions API key and a residential proxy with sticky sessions.

```bash
git clone https://github.com/Hyper-Solutions/radisson-scraper.git
cd radisson-scraper
export HYPER_API_KEY=your-key
export PROXY_URL='http://username-session-{session}:password@proxy.example.com:8080'
go run ./cmd/radisson -city london -checkin 2026-11-20 -checkout 2026-11-22 -currency GBP
```

`{session}` in the proxy URL is replaced with a random ID, so each run gets its own IP and keeps it. The Akamai payloads are generated for that IP, so the proxy must not rotate during a run.

Flags:

| Flag | Default | Meaning |
|---|---|---|
| `-city` | `london` | Destination, resolved through the site's autocomplete |
| `-checkin`, `-checkout` | 30 and 32 days from now | Stay dates, `YYYY-MM-DD` |
| `-adults` | `2` | Adults in the room |
| `-currency` | `USD` | Currency of the rates |
| `-out` | stdout | Write the JSON to a file |
| `-proxy` | `$PROXY_URL` | Proxy URL |
| `-api-key` | `$HYPER_API_KEY` | Hyper Solutions API key |
| `-debug-proxy` | | Send everything through a local debugging proxy such as powhttp (`http://127.0.0.1:8888`) and accept its certificate |

The output lists every hotel with its distance from the place, its content and its cheapest rate:

```json
{
  "place": { "name": "London, UK", "placeId": "ChIJdd4hrwug2EcRmSrV3Vo6llI" },
  "hotels": [
    {
      "code": "GBLHRHTR",
      "name": "Radisson RED Hotel London Heathrow",
      "rate": {
        "available": true,
        "currency": "GBP",
        "nightlyAmount": 89.55,
        "totalWithTax": 179.1,
        "roomsLeft": 89
      }
    }
  ]
}
```

## Using the package

```bash
go get github.com/Hyper-Solutions/radisson-scraper
```

```go
import "github.com/Hyper-Solutions/radisson-scraper/radisson"

s, _ := radisson.NewSession(radisson.Options{APIKey: key, ProxyURL: proxy, Currency: "GBP"})
if _, err := s.Bootstrap(ctx); err != nil { ... }
places, _ := s.Autocomplete(ctx, "london")
q := radisson.Query{PlaceID: places[0].PlaceID, CheckIn: "2026-11-20", CheckOut: "2026-11-22", Adults: 2}
hotels, _ := s.Hotels(ctx, q)
rates, _ := s.Rates(ctx, q, []string{hotels[0].Code})
```

A `Session` is one identity: one TLS client, one cookie jar and one proxy IP. Bootstrap once, then reuse the session for as many searches as it lasts.

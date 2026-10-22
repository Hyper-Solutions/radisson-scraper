package radisson

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"

	http "github.com/bogdanfinn/fhttp"
)

// Query is one hotel search: a place and a stay.
type Query struct {
	PlaceID  string `json:"placeId"`
	CheckIn  string `json:"checkIn"`  // YYYY-MM-DD
	CheckOut string `json:"checkOut"` // YYYY-MM-DD
	Adults   int    `json:"adults"`
}

// SearchURL is the results page a browser would be on for q. The API calls
// send it as their referer.
func (s *Session) SearchURL(q Query) string {
	return Origin + "/" + s.locale + "/booking/search-results?placeId=" + url.QueryEscape(q.PlaceID) +
		"&checkInDate=" + q.CheckIn + "&checkOutDate=" + q.CheckOut +
		"&adults%5B%5D=" + strconv.Itoa(q.Adults) + "&children%5B%5D=0&aoc%5B%5D=" +
		"&searchType=lowest&promotionCode=&voucher=&brands=&brandFirst=&so=CLASSIC&usePoints=false"
}

// Place is an autocomplete suggestion.
type Place struct {
	Name    string `json:"name"`
	PlaceID string `json:"placeId"`
}

// HotelRef is a hotel matched to a place.
type HotelRef struct {
	Code     string `json:"code"`
	Distance struct {
		Km float64 `json:"km"`
	} `json:"distance"`
}

// Rate is the cheapest rate of one hotel for a stay.
type Rate struct {
	Code           string  `json:"code"`
	Available      bool    `json:"available"`
	RateCode       string  `json:"rateCode,omitempty"`
	RoomID         string  `json:"roomId,omitempty"`
	Currency       string  `json:"currency,omitempty"`
	NightlyAmount  float64 `json:"nightlyAmount,omitempty"`
	NightlyUndisc  float64 `json:"nightlyUndiscountedAmount,omitempty"`
	TotalWithTax   float64 `json:"totalWithTax,omitempty"`
	TotalTaxAndFee float64 `json:"totalTaxAndFee,omitempty"`
	RoomsLeft      int     `json:"roomsLeft,omitempty"`
	MemberOnly     bool    `json:"memberOnly,omitempty"`
	Flexible       bool    `json:"flexible,omitempty"`
}

// Hotel is the static content of a hotel.
type Hotel struct {
	Code   string `json:"code"`
	Name   string `json:"name"`
	Brand  string `json:"brand"`
	Stars  string `json:"stars"`
	URL    string `json:"url"`
	Rating struct {
		Score   float64 `json:"score"`
		Reviews int     `json:"reviews"`
	} `json:"rating"`
	ContactInfo struct {
		Address string `json:"address"`
		Zip     string `json:"zip"`
		Phone   string `json:"phone"`
	} `json:"contactInfo"`
}

// StatusError is a non-2xx answer from an API endpoint.
type StatusError struct {
	Endpoint string
	Status   int
	Body     []byte
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: status %d body %.120q", e.Endpoint, e.Status, e.Body)
}

// api sends one API request, answers an Akamai challenge if one comes back,
// and retries once.
func (s *Session) api(ctx context.Context, step, method, path, referer string, body any, extra map[string]string, out any) error {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	for attempt := 0; ; attempt++ {
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, _ := http.NewRequestWithContext(ctx, method, Origin+path, rd)
		req.Header = s.apiHeaders(referer, method == http.MethodPost, extra)
		status, resp, err := s.do(step, req)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if status == http.StatusOK {
			return json.Unmarshal(resp, out)
		}
		if attempt == 0 {
			retry, err := s.recover(ctx, status, resp)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if retry {
				continue
			}
		}
		return &StatusError{Endpoint: path, Status: status, Body: resp}
	}
}

// Autocomplete resolves free text to places, e.g. "amsterdam".
func (s *Session) Autocomplete(ctx context.Context, text string) ([]Place, error) {
	var out struct {
		Places []Place `json:"places"`
	}
	err := s.api(ctx, "autocomplete", http.MethodGet, "/zimba-api/autocomplete/"+url.PathEscape(text), s.HomeURL(), nil, nil, &out)
	return out.Places, err
}

// Hotels lists the Radisson hotels the site matches to a place.
func (s *Session) Hotels(ctx context.Context, q Query) ([]HotelRef, error) {
	var out struct {
		Hotels []HotelRef `json:"hotels"`
	}
	err := s.api(ctx, "hotels", http.MethodGet, "/zimba-api/search-result/hotels/search?placeId="+url.QueryEscape(q.PlaceID), s.SearchURL(q), nil, nil, &out)
	return out.Hotels, err
}

type ratesRequest struct {
	CheckInDate        string       `json:"checkInDate"`
	CheckOutDate       string       `json:"checkOutDate"`
	Occupancies        []occupancy  `json:"occupancies"`
	SearchType         string       `json:"searchType"`
	TaxIncludedRegion  bool         `json:"taxIncludedRegion"`
	ExcludeMemberRates bool         `json:"excludeMemberRates"`
	SourceOfBusiness   string       `json:"sourceOfBusiness"`
	Hotels             []ratesHotel `json:"hotels"`
	PayWithPointsRates bool         `json:"payWithPointsRates"`
}

type occupancy struct {
	Adults   int   `json:"adults"`
	Children []int `json:"children"`
}

type ratesHotel struct {
	Code       string `json:"code"`
	MemberOnly bool   `json:"memberOnly"`
}

type money struct {
	Amount             float64 `json:"amount"`
	Currency           string  `json:"currency"`
	UndiscountedAmount float64 `json:"undiscountedAmount"`
	TotalTaxAndFee     float64 `json:"totalTaxAndFee"`
}

type ratesResponse struct {
	Hotels []struct {
		Code             string `json:"code"`
		SearchApplicable bool   `json:"searchApplicable"`
		MinimumCash      *struct {
			Code           string `json:"code"`
			RoomID         string `json:"roomId"`
			MemberOnly     bool   `json:"memberOnly"`
			Flexible       bool   `json:"flexible"`
			RoomsLeft      int    `json:"numberOfRoomsLeft"`
			AveragePricing struct {
				Price money `json:"price"`
			} `json:"averagePricing"`
			TotalPriceWithTax money `json:"totalPriceWithTax"`
		} `json:"minimumCash"`
	} `json:"hotels"`
}

// Rates asks the availability API for the cheapest rate of each hotel.
func (s *Session) Rates(ctx context.Context, q Query, codes []string) ([]Rate, error) {
	body := ratesRequest{
		CheckInDate:        q.CheckIn,
		CheckOutDate:       q.CheckOut,
		Occupancies:        []occupancy{{Adults: q.Adults, Children: []int{}}},
		SearchType:         "lowest",
		TaxIncludedRegion:  true,
		ExcludeMemberRates: true,
		SourceOfBusiness:   "desktop",
		PayWithPointsRates: false,
	}
	for _, c := range codes {
		body.Hotels = append(body.Hotels, ratesHotel{Code: c, MemberOnly: true})
	}
	var out ratesResponse
	extra := map[string]string{"x-version": "V2", "x-currency": s.currency}
	if err := s.api(ctx, "rates", http.MethodPost, "/availability-api/multiple-hotels-rates", s.SearchURL(q), body, extra, &out); err != nil {
		return nil, err
	}
	rates := make([]Rate, 0, len(out.Hotels))
	for _, h := range out.Hotels {
		r := Rate{Code: h.Code}
		if m := h.MinimumCash; h.SearchApplicable && m != nil {
			r.Available = true
			r.RateCode, r.RoomID = m.Code, m.RoomID
			r.Currency = m.AveragePricing.Price.Currency
			r.NightlyAmount = m.AveragePricing.Price.Amount
			r.NightlyUndisc = m.AveragePricing.Price.UndiscountedAmount
			r.TotalWithTax = m.TotalPriceWithTax.Amount
			r.TotalTaxAndFee = m.TotalPriceWithTax.TotalTaxAndFee
			r.RoomsLeft, r.MemberOnly, r.Flexible = m.RoomsLeft, m.MemberOnly, m.Flexible
		}
		rates = append(rates, r)
	}
	return rates, nil
}

// Hotel fetches the static content for one hotel code.
func (s *Session) Hotel(ctx context.Context, q Query, code string) (*Hotel, error) {
	var out struct {
		Hotels []Hotel `json:"hotels"`
	}
	err := s.api(ctx, "hotel", http.MethodGet, "/zimba-api/search-result/hotels?code="+url.QueryEscape(code), s.SearchURL(q), nil, map[string]string{"x-origin": "SR"}, &out)
	if err != nil {
		return nil, err
	}
	if len(out.Hotels) == 0 {
		return nil, fmt.Errorf("hotel %s: empty response", code)
	}
	return &out.Hotels[0], nil
}

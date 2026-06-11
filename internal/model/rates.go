package model

import (
	"fmt"
	"strings"
)

// RateType selects which FX rate translates an account (see AccountType.RateType).
type RateType string

const (
	RateAverage RateType = "Average"
	RateClosing RateType = "Closing"
)

// RateTable stores FX rates expressed as "group-currency units per 1 unit of
// Currency", keyed by "scenario|time|currency|type" (time is a month member).
// The string key keeps the table directly JSON-serializable.
type RateTable map[string]float64

func rateKey(scenario, timeMonth, currency string, t RateType) string {
	return strings.Join([]string{scenario, timeMonth, currency, string(t)}, "|")
}

// Set stores a rate. Rate must be > 0.
func (r RateTable) Set(scenario, timeMonth, currency string, t RateType, rate float64) error {
	if rate <= 0 {
		return fmt.Errorf("rate must be positive, got %v", rate)
	}
	r[rateKey(scenario, timeMonth, currency, t)] = rate
	return nil
}

// Delete removes a rate if present.
func (r RateTable) Delete(scenario, timeMonth, currency string, t RateType) {
	delete(r, rateKey(scenario, timeMonth, currency, t))
}

// Get looks up a rate; ok=false when absent.
func (r RateTable) Get(scenario, timeMonth, currency string, t RateType) (float64, bool) {
	v, ok := r[rateKey(scenario, timeMonth, currency, t)]
	return v, ok
}

// ToGroup returns the multiplier that converts an amount in currency into the
// group currency. Same currency → 1. ok=false when the rate is missing (the
// caller decides the fallback policy).
func (r RateTable) ToGroup(scenario, timeMonth, currency, group string, t RateType) (float64, bool) {
	if currency == group || currency == "" {
		return 1, true
	}
	return r.Get(scenario, timeMonth, currency, t)
}

// ParsedRate is the exploded form of one rate entry, for listing/UI.
type ParsedRate struct {
	Scenario string   `json:"scenario"`
	Time     string   `json:"time"`
	Currency string   `json:"currency"`
	Type     RateType `json:"type"`
	Value    float64  `json:"value"`
}

// List returns all rates, optionally filtered by scenario and/or time
// (empty filter = match all).
func (r RateTable) List(scenario, timeMonth string) []ParsedRate {
	var out []ParsedRate
	for k, v := range r {
		parts := strings.Split(k, "|")
		if len(parts) != 4 {
			continue
		}
		if scenario != "" && parts[0] != scenario {
			continue
		}
		if timeMonth != "" && parts[1] != timeMonth {
			continue
		}
		out = append(out, ParsedRate{parts[0], parts[1], parts[2], RateType(parts[3]), v})
	}
	return out
}

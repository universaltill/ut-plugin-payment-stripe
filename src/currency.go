// Currency resolution (ut-docs#6604), ported from the SumUp plugin. No
// //go:build tag: pure logic, covered by a plain `go test`.
package main

import (
	"encoding/json"
	"strings"
)

// supportedCurrencies is the picker: 2-decimal currencies only (amounts are
// sent to Stripe in minor units with that assumption).
var supportedCurrencies = []string{
	"AED", "AUD", "BRL", "CAD", "CHF", "CZK", "DKK", "EUR", "GBP",
	"MXN", "MYR", "NOK", "PLN", "RON", "SEK", "TRY", "USD",
}

// currencyAuto is the setting's default: "use the shop's own currency".
const currencyAuto = "auto"

func isSupportedCurrency(code string) bool {
	code = strings.ToUpper(strings.TrimSpace(code))
	for _, c := range supportedCurrencies {
		if c == code {
			return true
		}
	}
	return false
}

// shopInfo is what the core view shop.context.v2 reports about the till.
type shopInfo struct {
	Code     string // ISO 4217, upper case
	Decimals int
	Known    bool
}

// parseShopContext decodes shop.context.v2's one-row JSON array.
func parseShopContext(body []byte) (code string, decimals int, ok bool) {
	var rows []struct {
		Code     string `json:"currency_code"`
		Decimals int    `json:"currency_decimals"`
	}
	if err := json.Unmarshal(body, &rows); err != nil || len(rows) == 0 {
		return "", 0, false
	}
	code = strings.ToUpper(strings.TrimSpace(rows[0].Code))
	if code == "" {
		return "", 0, false
	}
	return code, rows[0].Decimals, true
}

// resolveCurrency picks the currency to charge (lower case, as this plugin
// has always reported it) or returns a decline code. Order: an explicit
// saved choice, else the sale's own currency, else the shop's currency;
// with none of them known it declines currency_unknown . Whatever is chosen must be one Stripe supports here here and must
// equal the sale's currency (the event's if it carries one, else the
// shop's): amounts are minor units of THAT currency, so charging another
// would bill the wrong money.
func resolveCurrency(saved, saleCurrency string, shop shopInfo) (code, decline string) {
	saved = strings.ToUpper(strings.TrimSpace(saved))
	sale := strings.ToUpper(strings.TrimSpace(saleCurrency))
	chosen := saved
	if chosen == "" || chosen == strings.ToUpper(currencyAuto) {
		switch {
		case sale != "":
			chosen = sale
		case shop.Known:
			chosen = shop.Code
		default:
			// Nothing tells us the sale's currency (the authorize event
			// carries none today and shop.context.v2 was unavailable):
			// never guess a currency for real money.
			return "", "currency_unknown"
		}
	}
	if !isSupportedCurrency(chosen) {
		return "", "unsupported_currency"
	}
	expected := sale
	if expected == "" && shop.Known {
		expected = shop.Code
	}
	if expected != "" && expected != chosen {
		return "", "currency_mismatch"
	}
	if shop.Known && shop.Decimals != 2 {
		return "", "unsupported_decimals"
	}
	return strings.ToLower(chosen), ""
}

// declinedCurrency names the currency to report on a currency decline: the
// sale's own, else the shop's, else the saved pick (never the word "auto").
func declinedCurrency(saved, saleCurrency string, shop shopInfo) string {
	saved = strings.TrimSpace(saved)
	if strings.EqualFold(saved, currencyAuto) {
		saved = ""
	}
	switch {
	case strings.TrimSpace(saleCurrency) != "":
		return strings.ToLower(strings.TrimSpace(saleCurrency))
	case shop.Known:
		return strings.ToLower(shop.Code)
	}
	return strings.ToLower(saved)
}

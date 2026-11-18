package main

import "testing"

func TestResolveCurrency(t *testing.T) {
	gbp := shopInfo{Code: "GBP", Decimals: 2, Known: true}
	cases := []struct {
		name, saved, sale string
		shop              shopInfo
		want, decline     string
	}{
		{"auto uses sale currency", "auto", "EUR", gbp, "eur", ""},
		{"auto sale matches shop", "auto", "GBP", gbp, "gbp", ""},
		{"auto without sale uses shop", "auto", "", gbp, "gbp", ""},
		{"empty saved is auto", "", "", gbp, "gbp", ""},
		{"pinned differs from shop", "usd", "", gbp, "", "currency_mismatch"},
		{"pinned matches sale", "gbp", "GBP", gbp, "gbp", ""},
		{"nothing known", "auto", "", shopInfo{}, "", "currency_unknown"},
		{"unsupported", "jpy", "JPY", shopInfo{}, "", "unsupported_currency"},
		{"shop not 2 decimals", "auto", "", shopInfo{Code: "GBP", Decimals: 0, Known: true}, "", "unsupported_decimals"},
	}
	for _, c := range cases {
		got, dec := resolveCurrency(c.saved, c.sale, c.shop)
		if got != c.want || dec != c.decline {
			t.Errorf("%s: got %q/%q want %q/%q", c.name, got, dec, c.want, c.decline)
		}
	}
}

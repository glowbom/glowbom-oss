package main

import (
	"math"
	"testing"
)

func TestGlowbomCreditConversion(t *testing.T) {
	for _, test := range []struct {
		usd  float64
		want string
	}{{20, "4,000"}, {10, "2,000"}, {12.5, "2,500"}, {0, "0"}, {0.0123, "2.46"}, {0.000001, "<0.01"}, {3000000, "600,000,000"}, {-1, "Unavailable"}, {math.Inf(1), "Unavailable"}, {math.NaN(), "Unavailable"}} {
		if got := formatCredits(creditsFromUSD(&test.usd)); got != test.want {
			t.Errorf("%v USD: got %s, want %s", test.usd, got, test.want)
		}
	}
	if creditsFromUSD(nil) != nil {
		t.Fatal("missing amount must stay missing")
	}
}

func TestCreditBalanceRoundsDown(t *testing.T) {
	for _, tc := range []struct {
		value float64
		want  string
	}{{17.83, "17"}, {20, "20"}, {0.9, "0"}, {4000.99, "4,000"}} {
		if got := formatCreditBalance(&tc.value); got != tc.want {
			t.Errorf("got %s, want %s", got, tc.want)
		}
	}
	if formatCreditBalance(nil) != "Unavailable" {
		t.Fatal("missing balance must stay unknown")
	}
}

package main

import (
	"fmt"
	"math"
	"strings"
)

const glowbomCreditsPerUSD = 200

func creditsFromUSD(usd *float64) *float64 {
	if usd == nil || *usd < 0 || math.IsNaN(*usd) || math.IsInf(*usd, 0) {
		return nil
	}
	credits := *usd * glowbomCreditsPerUSD
	if math.IsInf(credits, 0) {
		return nil
	}
	return &credits
}

func formatCredits(credits *float64) string {
	if credits == nil {
		return "Unavailable"
	}
	if *credits > 0 && *credits < 0.01 {
		return "<0.01"
	}
	parts := strings.Split(fmt.Sprintf("%.2f", *credits), ".")
	whole := parts[0]
	for i := len(whole) - 3; i > 0; i -= 3 {
		whole = whole[:i] + "," + whole[i:]
	}
	fraction := strings.TrimRight(parts[1], "0")
	if fraction != "" {
		return whole + "." + fraction
	}
	return whole
}

func formatCreditBalance(credits *float64) string {
	if credits == nil {
		return "Unavailable"
	}
	whole := math.Floor(*credits)
	return formatCredits(&whole)
}

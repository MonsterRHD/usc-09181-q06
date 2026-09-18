package main

import (
	"errors"
	"strconv"
	"strings"
)

// 各币种最小单位（小数位）。金额一律以最小单位整数传输与记账。
var currencyExponent = map[string]int{
	"CNY": 2, "USD": 2, "EUR": 2, "GBP": 2, "HKD": 2,
	"SGD": 2, "AUD": 2, "CAD": 2, "CHF": 2, "JPY": 0,
}

func knownCurrency(c string) bool {
	_, ok := currencyExponent[c]
	return ok
}

// parseRateMicros 把 "7.1234" 解析为 6 位小数定点整数（7.123400 -> 7123400）。
// 超过 6 位按四舍五入截断；零或负数视为无效。
func parseRateMicros(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty rate")
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	parts := strings.SplitN(s, ".", 2)
	intPart, frac := parts[0], ""
	if len(parts) == 2 {
		frac = parts[1]
	}
	if intPart == "" {
		intPart = "0"
	}
	for _, ch := range intPart {
		if ch < '0' || ch > '9' {
			return 0, errors.New("invalid rate")
		}
	}
	for _, ch := range frac {
		if ch < '0' || ch > '9' {
			return 0, errors.New("invalid rate")
		}
	}
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return 0, errors.New("invalid rate")
	}
	var micros int64
	switch {
	case len(frac) >= 6:
		v, _ := strconv.ParseInt(frac[:6], 10, 64)
		micros = whole*1_000_000 + v
		if len(frac) > 6 && frac[6] >= '5' {
			micros++
		}
	default:
		v := int64(0)
		if frac != "" {
			v, _ = strconv.ParseInt(frac, 10, 64)
		}
		for i := len(frac); i < 6; i++ {
			v *= 10
		}
		micros = whole*1_000_000 + v
	}
	if neg {
		micros = -micros
	}
	if micros <= 0 {
		return 0, errors.New("rate must be positive")
	}
	return micros, nil
}

func formatRate(micros int64) string {
	return strconv.FormatInt(micros/1_000_000, 10) + "." + pad6(micros%1_000_000)
}

func pad6(v int64) string {
	s := strconv.FormatInt(v, 10)
	for len(s) < 6 {
		s = "0" + s
	}
	return s
}

// convert 按定点汇率换算最小单位金额，四舍五入。
func convert(amountMinor, rateMicros int64) int64 {
	q := amountMinor * rateMicros
	return (q + 500_000) / 1_000_000
}

// feeBpsAmount 按基点（1bp=0.01%）计算费用，四舍五入。
func feeBpsAmount(amount int64, bps int) int64 {
	return (amount*int64(bps) + 5_000) / 10_000
}

// devBps 计算 new 相对 old 的偏离基点（绝对值）。
func devBps(old, new int64) int64 {
	d := new - old
	if d < 0 {
		d = -d
	}
	return d * 10_000 / old
}

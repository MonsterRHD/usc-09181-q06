// Package travelmoney 实现「多币种出行资金管家」的领域内核。
//
// 领域术语对照：
//   - 刷卡(Authorization/purchase)：在线消费授权请求，批准后冻结可用余额
//   - 预授权(preauth)：酒店/租车类的预先冻结，与刷卡走同一条授权生命周期
//   - 撤销(reversal/void)：商户在清算前释放冻结，支持部分撤销
//   - 清算/补扣(clearing/presentment)：商户请款，最终金额可能高于或低于冻结额
//   - 离线补传(force post)：脱机终端先扣款后补送，可能跨午夜、可能没有在线授权
//   - 汇率快照(FX snapshot)：每次换算所依据的固定汇率口径，展示时逐笔钉住
//
// 所有状态变更都是只追加的领域事件；Service 重放事件即可恢复全部状态。
package travelmoney

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Currency 为 ISO-4217 三字币种代码。
type Currency string

// CcyPair 货币对：Base=交易币种，Quote=记账（账单）币种。
type CcyPair struct {
	Base  Currency `json:"base"`
	Quote Currency `json:"quote"`
}

func (p CcyPair) String() string { return string(p.Base) + "/" + string(p.Quote) }

// 金额一律使用币种最小单位（如“分”）的整数；汇率以 1e6 倍率的整数表示，
// 全链路不使用浮点数。
const RateScale int64 = 1_000_000

var (
	// ErrInvalidRate 汇率非法（非正数等）。
	ErrInvalidRate = errors.New("invalid fx rate")
	// ErrAmountOverflow 换算结果溢出 int64。
	ErrAmountOverflow = errors.New("converted amount overflow")
	// ErrNotFound 实体不存在。
	ErrNotFound = errors.New("not found")
	// ErrForbidden 角色或授权范围不允许该操作。
	ErrForbidden = errors.New("forbidden")
	// ErrInvalidArgument 请求参数非法。
	ErrInvalidArgument = errors.New("invalid argument")
)

// Error 带错误码的领域错误，便于接口层映射 HTTP 状态。
type Error struct {
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string {
	if e.Message == "" && e.Cause != nil {
		return e.Code + ": " + e.Cause.Error()
	}
	return e.Code + ": " + e.Message
}

func (e *Error) Unwrap() error { return e.Cause }

func serr(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func wrapErr(code string, cause error) *Error {
	return &Error{Code: code, Cause: cause}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// ConvertMinor 按 micro 汇率把 base 币种最小单位金额换算为 quote 最小单位金额，
// 采用 half-up 四舍五入；负数保持符号。
func ConvertMinor(amount, rateMicro int64) (int64, error) {
	if rateMicro <= 0 {
		return 0, ErrInvalidRate
	}
	neg := amount < 0
	n := abs64(amount)
	num := new(big.Int).Mul(big.NewInt(n), big.NewInt(rateMicro))
	num.Add(num, big.NewInt(RateScale/2))
	q := num.Quo(num, big.NewInt(RateScale))
	if !q.IsInt64() {
		return 0, ErrAmountOverflow
	}
	v := q.Int64()
	if neg {
		v = -v
	}
	return v, nil
}

// FeeBps 按基点（1bp = 0.01%）计算手续费，half-up。
func FeeBps(amount, bps int64) int64 {
	return (abs64(amount)*bps + 5_000) / 10_000
}

// FormatRate 把 micro 汇率还原为小数字符串，用于固化展示口径。
func FormatRate(rateMicro int64) string {
	neg := rateMicro < 0
	v := abs64(rateMicro)
	whole, frac := v/RateScale, v%RateScale
	s := fmt.Sprintf("%d.%06d", whole, frac)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	if neg {
		s = "-" + s
	}
	return s
}

// MaskPAN 卡号脱敏：保留 BIN 前 6 位与末 4 位，其余以 * 替换。
// 系统在任何持久化发生之前就只保留脱敏结果，原始 PAN 不进事件、不落盘。
func MaskPAN(pan string) string {
	digits := make([]rune, 0, len(pan))
	for _, r := range pan {
		if r >= '0' && r <= '9' {
			digits = append(digits, r)
		}
	}
	n := len(digits)
	switch {
	case n == 0:
		return ""
	case n <= 10:
		return strings.Repeat("*", n)
	default:
		return string(digits[:6]) + strings.Repeat("*", n-10) + string(digits[n-4:])
	}
}

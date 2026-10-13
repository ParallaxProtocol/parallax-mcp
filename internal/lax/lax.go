// Package lax converts between wei and LAX and reproduces the protocol's
// emission schedule (kernel/xhash calcBlockReward and
// cumulativeEmissionThrough).
package lax

import (
	"fmt"
	"math/big"
	"strings"
)

const (
	// Decimals is the number of decimal places of one LAX: 1 LAX = 10^18 wei.
	Decimals = 18
	// HalvingInterval is the number of blocks between reward halvings.
	HalvingInterval = 210_000
	// TargetBlockSeconds is the block time difficulty adjustment aims for.
	TargetBlockSeconds = 600
	// CoinbaseMaturity is the number of blocks before a block reward is
	// spendable.
	CoinbaseMaturity = 100
)

var (
	weiPerLAX = new(big.Int).Exp(big.NewInt(10), big.NewInt(Decimals), nil)
	// InitialReward is the era 0 block reward, 50 LAX, in wei.
	InitialReward = new(big.Int).Mul(big.NewInt(50), weiPerLAX)
	// SupplyCap is the 21,000,000 LAX limit, in wei. The schedule's sum
	// approaches it from below, as in Bitcoin.
	SupplyCap = new(big.Int).Mul(big.NewInt(21_000_000), weiPerLAX)
)

// Amount is a value in wei with its LAX equivalent. Wei is the exact figure;
// LAX is a decimal string for people to read.
type Amount struct {
	Wei string `json:"wei"`
	LAX string `json:"lax"`
}

// NewAmount formats wei as an Amount. A nil value is zero.
func NewAmount(wei *big.Int) Amount {
	if wei == nil {
		wei = new(big.Int)
	}
	return Amount{Wei: wei.String(), LAX: FormatUnits(wei, Decimals)}
}

// FormatUnits renders value / 10^decimals exactly, without trailing zeros:
// FormatUnits(1500000000000000000, 18) is "1.5".
func FormatUnits(value *big.Int, decimals int) string {
	if value == nil {
		return "0"
	}
	neg := value.Sign() < 0
	abs := new(big.Int).Abs(value)
	if decimals <= 0 {
		return value.String()
	}
	s := abs.String()
	if len(s) <= decimals {
		s = strings.Repeat("0", decimals-len(s)+1) + s
	}
	whole, frac := s[:len(s)-decimals], strings.TrimRight(s[len(s)-decimals:], "0")
	out := whole
	if frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

// ParseLAX converts a decimal LAX string such as "1.25" to wei. More than
// 18 decimal places is an error rather than a silent rounding.
func ParseLAX(s string) (*big.Int, error) {
	return ParseUnits(s, Decimals)
}

// ParseUnits converts a decimal string to an integer of 10^-decimals units.
func ParseUnits(s string, decimals int) (*big.Int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty amount")
	}
	if strings.HasPrefix(s, "-") {
		return nil, fmt.Errorf("amount %q is negative", s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > decimals {
		return nil, fmt.Errorf("amount %q has more than %d decimal places", s, decimals)
	}
	if whole == "" {
		whole = "0"
	}
	digits := whole + frac + strings.Repeat("0", decimals-len(frac))
	v, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return nil, fmt.Errorf("amount %q is not a decimal number", s)
	}
	return v, nil
}

// BlockReward is the subsidy of the block at height, in wei: 50 LAX halved
// every 210,000 blocks, nothing for genesis and nothing after 63 halvings.
func BlockReward(height uint64) *big.Int {
	if height == 0 {
		return new(big.Int)
	}
	halvings := height / HalvingInterval
	if halvings > 63 {
		return new(big.Int)
	}
	return new(big.Int).Rsh(InitialReward, uint(halvings))
}

// IssuedThrough is the sum of block rewards for blocks 1..height, in wei:
// the total (and circulating) supply once that block is mined. Parallax has
// no premine, burns or protocol locks.
func IssuedThrough(height uint64) *big.Int {
	total := new(big.Int)
	lastEra := height / HalvingInterval
	if lastEra > 63 {
		lastEra = 63
	}
	for era := uint64(0); era <= lastEra; era++ {
		first := era * HalvingInterval
		last := first + HalvingInterval - 1
		if last > height {
			last = height
		}
		if first == 0 {
			first = 1
		}
		if last < first {
			continue
		}
		n := new(big.Int).SetUint64(last - first + 1)
		total.Add(total, n.Mul(n, BlockReward(first)))
	}
	return total
}

// Era is the halving era of height: 0 for blocks 0..209,999.
func Era(height uint64) uint64 { return height / HalvingInterval }

// NextHalving is the first height of the era after height's.
func NextHalving(height uint64) uint64 { return (Era(height) + 1) * HalvingInterval }

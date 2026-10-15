package lax

import (
	"math/big"
	"testing"
)

func lax(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), weiPerLAX) }

func TestBlockReward(t *testing.T) {
	half := new(big.Int).Rsh(InitialReward, 1)
	for _, tc := range []struct {
		height uint64
		want   *big.Int
	}{
		{0, new(big.Int)},
		{1, InitialReward},
		{209_999, InitialReward},
		{210_000, half},
		{419_999, half},
		{420_000, new(big.Int).Rsh(InitialReward, 2)},
		{63 * HalvingInterval, new(big.Int).Rsh(InitialReward, 63)},
		{64 * HalvingInterval, new(big.Int)},
	} {
		if got := BlockReward(tc.height); got.Cmp(tc.want) != 0 {
			t.Errorf("BlockReward(%d) = %s, want %s", tc.height, got, tc.want)
		}
	}
}

func TestIssuedThrough(t *testing.T) {
	// Live figure from xhash_getTotalSupply at height 59,633.
	if got, want := IssuedThrough(59_633), lax(2_981_650); got.Cmp(want) != 0 {
		t.Fatalf("IssuedThrough(59633) = %s, want %s", got, want)
	}
	if got := IssuedThrough(0); got.Sign() != 0 {
		t.Fatalf("genesis issues nothing, got %s", got)
	}
	// End of era 0: 209,999 blocks of 50 (genesis pays nothing).
	if got, want := IssuedThrough(209_999), lax(10_499_950); got.Cmp(want) != 0 {
		t.Fatalf("end of era 0: %s, want %s", got, want)
	}
	// The first block of era 1 pays 25.
	if got, want := IssuedThrough(210_000), lax(10_499_975); got.Cmp(want) != 0 {
		t.Fatalf("first block of era 1: %s, want %s", got, want)
	}
	// The schedule never reaches the cap.
	final := IssuedThrough(100 * HalvingInterval)
	if final.Cmp(SupplyCap) >= 0 {
		t.Fatalf("total issuance %s reaches the cap", final)
	}
	if left := new(big.Int).Sub(SupplyCap, final); left.Cmp(lax(51)) > 0 {
		t.Fatalf("total issuance falls short of the cap by %s wei", left)
	}
}

func TestFormatUnits(t *testing.T) {
	for _, tc := range []struct {
		in   string
		dec  int
		want string
	}{
		{"0", 18, "0"},
		{"1", 18, "0.000000000000000001"},
		{"1500000000000000000", 18, "1.5"},
		{"50000000000000000000", 18, "50"},
		{"-2500000000000000000", 18, "-2.5"},
		{"129130911424", 9, "129.130911424"},
		{"42", 0, "42"},
	} {
		v, _ := new(big.Int).SetString(tc.in, 10)
		if got := FormatUnits(v, tc.dec); got != tc.want {
			t.Errorf("FormatUnits(%s, %d) = %q, want %q", tc.in, tc.dec, got, tc.want)
		}
	}
}

func TestParseLAX(t *testing.T) {
	for in, want := range map[string]string{
		"1":                    "1000000000000000000",
		"0.5":                  "500000000000000000",
		".25":                  "250000000000000000",
		"21000000":             "21000000000000000000000000",
		"0.000000000000000001": "1",
	} {
		got, err := ParseLAX(in)
		if err != nil || got.String() != want {
			t.Errorf("ParseLAX(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "1.0000000000000000001", "abc", "1e18"} {
		if _, err := ParseLAX(bad); err == nil {
			t.Errorf("ParseLAX(%q): want error", bad)
		}
	}
}

func TestHalvingHelpers(t *testing.T) {
	if Era(209_999) != 0 || Era(210_000) != 1 {
		t.Fatal("era boundaries")
	}
	if NextHalving(59_633) != 210_000 || NextHalving(210_000) != 420_000 {
		t.Fatal("next halving")
	}
}

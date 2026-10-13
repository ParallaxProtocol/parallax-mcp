package evm

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ParallaxProtocol/parallax/v2/util"

	"github.com/ParallaxProtocol/parallax-mcp/internal/untrusted"
)

func raws(t *testing.T, vals ...string) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, len(vals))
	for i, v := range vals {
		out[i] = json.RawMessage(v)
	}
	return out
}

func TestParseSignature(t *testing.T) {
	for sig, want := range map[string]string{
		"balanceOf(address)": "balanceOf(address)",
		"function transfer(address to, uint amount) returns (bool)": "transfer(address,uint256)",
		"getReserves() view returns (uint112,uint112,uint32)":       "getReserves()",
		"f((address,uint256)[] orders, bytes32 id)":                 "f((address,uint256)[],bytes32)",
		"g(uint8[3] xs, string memory s)":                           "g(uint8[3],string)",
	} {
		m, err := ParseSignature(sig)
		if err != nil {
			t.Errorf("%s: %v", sig, err)
			continue
		}
		if m.Sig != want {
			t.Errorf("%s: Sig = %s, want %s", sig, m.Sig, want)
		}
	}
	m, _ := ParseSignature("transfer(address,uint256) returns (bool)")
	if hex.EncodeToString(m.ID) != "a9059cbb" || len(m.Outputs) != 1 {
		t.Fatalf("transfer: id %x, outputs %d", m.ID, len(m.Outputs))
	}
	for _, bad := range []string{"", "noparens", "f(uint256", "f(uint7)", "f() returns bool", "f() junk"} {
		if _, err := ParseSignature(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestEncodeArgs(t *testing.T) {
	m, _ := ParseSignature("transfer(address,uint256)")
	data, err := EncodeArgs(m.Inputs, raws(t, `"0x45a117abe265623239a3c7a050ea7e8d7b040248"`, `"1000000000000000000000"`))
	if err != nil {
		t.Fatal(err)
	}
	want := "00000000000000000000000045a117abe265623239a3c7a050ea7e8d7b040248" +
		"00000000000000000000000000000000000000000000003635c9adc5dea00000"
	if hex.EncodeToString(data) != want {
		t.Fatalf("got %x", data)
	}

	// Numbers, hex, small ints, bytes32, arrays and tuples.
	m, _ = ParseSignature("f(uint8 a, int64 b, bytes32 c, uint256[] d, (address x, bool y) e, bytes f)")
	_, err = EncodeArgs(m.Inputs, raws(t,
		`7`, `"-5"`, `"0x`+strings.Repeat("11", 32)+`"`, `["1","0x2",3]`,
		`{"x":"0x0000000000000000000000000000000000000001","y":true}`, `"0xdead"`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = EncodeArgs(m.Inputs, raws(t,
		`7`, `"-5"`, `"0x`+strings.Repeat("11", 32)+`"`, `[]`,
		`["0x0000000000000000000000000000000000000001",false]`, `"0x"`))
	if err != nil {
		t.Fatalf("tuple as array: %v", err)
	}
}

func TestEncodeArgsErrors(t *testing.T) {
	m, _ := ParseSignature("f(uint8,address,bytes4)")
	for name, args := range map[string][]string{
		"count":     {`1`},
		"overflow":  {`256`, `"0x0000000000000000000000000000000000000001"`, `"0x00000000"`},
		"negative":  {`"-1"`, `"0x0000000000000000000000000000000000000001"`, `"0x00000000"`},
		"address":   {`1`, `"0x01"`, `"0x00000000"`},
		"bytes4":    {`1`, `"0x0000000000000000000000000000000000000001"`, `"0x00"`},
		"not a num": {`"ten"`, `"0x0000000000000000000000000000000000000001"`, `"0x00000000"`},
	} {
		if _, err := EncodeArgs(m.Inputs, raws(t, args...)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestDecodeValuesWrapsStrings(t *testing.T) {
	m, _ := ParseSignature("name() returns (string)")
	data, _ := hex.DecodeString("0000000000000000000000000000000000000000000000000000000000000020" +
		"0000000000000000000000000000000000000000000000000000000000000005" +
		"4150455045000000000000000000000000000000000000000000000000000000")
	vals, err := DecodeValues(m.Outputs, data)
	if err != nil {
		t.Fatal(err)
	}
	txt, ok := vals[0].Value.(untrusted.Text)
	if !ok || txt.String() != "APEPE" {
		t.Fatalf("got %#v", vals[0].Value)
	}
}

func TestDecodeStandardLogERC20AndERC721(t *testing.T) {
	transfer := util.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	from := util.HexToHash("0x000000000000000000000000db34c086828e1874cf515491f8dc0eeb7ffa09fc")
	to := util.HexToHash("0x00000000000000000000000045a117abe265623239a3c7a050ea7e8d7b040248")
	amount, _ := hex.DecodeString("00000000000000000000000000000000000000000000003635c9adc5dea00000")

	ev := DecodeStandardLog([]util.Hash{transfer, from, to}, amount)
	if ev == nil || ev.Name != "Transfer" || ev.Args[2].Name != "value" || ev.Args[2].Value != "1000000000000000000000" {
		t.Fatalf("erc20: %+v", ev)
	}
	tokenID := util.BigToHash(util.Big1)
	ev = DecodeStandardLog([]util.Hash{transfer, from, to, tokenID}, nil)
	if ev == nil || ev.Args[2].Name != "tokenId" || ev.Args[2].Value != "1" {
		t.Fatalf("erc721: %+v", ev)
	}
	if DecodeStandardLog([]util.Hash{util.HexToHash("0x01")}, nil) != nil {
		t.Fatal("unknown topic decoded")
	}
}

func TestDecodeCall(t *testing.T) {
	input, _ := hex.DecodeString("a9059cbb00000000000000000000000045a117abe265623239a3c7a050ea7e8d7b040248" +
		"0000000000000000000000000000000000000000000000000000000000000001")
	c := DecodeCall(StandardABI, input, "standard")
	if c == nil || c.Signature != "transfer(address,uint256)" || c.Args[0].Value != "0x45a117ABe265623239a3c7A050EA7e8D7b040248" {
		t.Fatalf("got %+v", c)
	}
	if DecodeCall(StandardABI, []byte{1, 2, 3, 4}, "standard") != nil {
		t.Fatal("unknown selector decoded")
	}
}

func TestParseAddress(t *testing.T) {
	good := []string{
		"0x45a117abe265623239a3c7a050ea7e8d7b040248",
		"0x45A117ABE265623239A3C7A050EA7E8D7B040248",
		"0x45a117ABe265623239a3c7A050EA7e8D7b040248",
	}
	for _, s := range good {
		if _, err := ParseAddress(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	// The last one is the checksummed address with one letter's case flipped.
	bad := []string{"", "45a117abe265623239a3c7a050ea7e8d7b040248", "0x45a1", "0x45a117ABe265623239a3c7A050EA7e8D7B040248"}
	for _, s := range bad {
		if _, err := ParseAddress(s); err == nil {
			t.Errorf("%q: want error", s)
		}
	}
}

func TestParseBlockRef(t *testing.T) {
	for in, want := range map[string]string{"": "latest", "latest": "latest", "pending": "pending", "59633": "0xe8f1", "0xe8f1": "0xe8f1", "0": "0x0"} {
		ref, err := ParseBlockRef(in)
		if err != nil || ref.Tag != want {
			t.Errorf("%q: got %+v, %v; want %s", in, ref, err, want)
		}
	}
	ref, err := ParseBlockRef("0x7019150ce38f57e1ae90f0a15d9b76ba13a83097bb62500f4d8a9e9745bc31e0")
	if err != nil || ref.Hash == nil {
		t.Fatalf("hash: %+v %v", ref, err)
	}
	if p, ok := ref.Param().(map[string]any); !ok || p["blockHash"] == nil {
		t.Fatalf("hash param: %#v", ref.Param())
	}
	for _, bad := range []string{"-1", "abc", "0x", "18446744073709551616"} {
		if _, err := ParseBlockRef(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

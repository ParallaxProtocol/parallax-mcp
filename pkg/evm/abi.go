package evm

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"

	"github.com/ParallaxProtocol/parallax/v2/script/abi"
	"github.com/ParallaxProtocol/parallax/v2/util"

	"github.com/ParallaxProtocol/parallax-mcp/pkg/untrusted"
)

// ParseABI parses a contract ABI in its JSON form.
func ParseABI(raw []byte) (*abi.ABI, error) {
	a, err := abi.JSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid ABI: %w", err)
	}
	return &a, nil
}

// ParseSignature turns a human-readable function signature into a method:
// "balanceOf(address)", "transfer(address to, uint256 amount) returns (bool)",
// "getReserves() view returns (uint112,uint112,uint32)". Tuples are written in
// parentheses: "f((address,uint256)[] orders)".
func ParseSignature(sig string) (abi.Method, error) {
	sig = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sig), "function "))
	open := strings.IndexByte(sig, '(')
	if open <= 0 {
		return abi.Method{}, fmt.Errorf("signature %q: expected name(types...)", sig)
	}
	name := strings.TrimSpace(sig[:open])
	inner, rest, err := matchParen(sig[open:])
	if err != nil {
		return abi.Method{}, fmt.Errorf("signature %q: %w", sig, err)
	}
	inputs, err := parseParams(inner)
	if err != nil {
		return abi.Method{}, fmt.Errorf("signature %q: %w", sig, err)
	}
	var outputs abi.Arguments
	rest = strings.TrimSpace(rest)
	for _, kw := range []string{"external", "public", "view", "pure", "payable", "nonpayable"} {
		rest = strings.TrimSpace(strings.ReplaceAll(rest, kw+" ", ""))
		rest = strings.TrimSpace(strings.TrimSuffix(rest, kw))
	}
	if strings.HasPrefix(rest, "returns") {
		outInner, tail, err := matchParen(strings.TrimSpace(strings.TrimPrefix(rest, "returns")))
		if err != nil {
			return abi.Method{}, fmt.Errorf("signature %q: returns: %w", sig, err)
		}
		if strings.TrimSpace(tail) != "" {
			return abi.Method{}, fmt.Errorf("signature %q: unexpected %q", sig, tail)
		}
		if outputs, err = parseParams(outInner); err != nil {
			return abi.Method{}, fmt.Errorf("signature %q: %w", sig, err)
		}
	} else if rest != "" {
		return abi.Method{}, fmt.Errorf("signature %q: unexpected %q", sig, rest)
	}
	return abi.NewMethod(name, name, abi.Function, "view", true, false, inputs, outputs), nil
}

// matchParen splits "(a,(b,c)) rest" into "a,(b,c)" and " rest".
func matchParen(s string) (string, string, error) {
	if !strings.HasPrefix(s, "(") {
		return "", "", fmt.Errorf("expected (")
	}
	depth := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[1:i], s[i+1:], nil
			}
		}
	}
	return "", "", fmt.Errorf("unbalanced parentheses")
}

func splitTop(s string) []string {
	var parts []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	if strings.TrimSpace(s[start:]) != "" || len(parts) > 0 {
		parts = append(parts, s[start:])
	}
	return parts
}

func parseParams(s string) (abi.Arguments, error) {
	var args abi.Arguments
	for i, p := range splitTop(s) {
		m, err := parseParam(strings.TrimSpace(p))
		if err != nil {
			return nil, err
		}
		if m.Name == "" {
			m.Name = fmt.Sprintf("arg%d", i)
		}
		typ, err := abi.NewType(m.Type, "", m.Components)
		if err != nil {
			return nil, fmt.Errorf("type %q: %w", m.Type, err)
		}
		args = append(args, abi.Argument{Name: m.Name, Type: typ})
	}
	return args, nil
}

// parseParam parses "uint256 amount", "address indexed from" or
// "(address,uint256)[] orders" into the ABI JSON form.
func parseParam(p string) (abi.ArgumentMarshaling, error) {
	if p == "" {
		return abi.ArgumentMarshaling{}, fmt.Errorf("empty parameter")
	}
	var m abi.ArgumentMarshaling
	var rest string
	if strings.HasPrefix(p, "(") {
		inner, tail, err := matchParen(p)
		if err != nil {
			return m, err
		}
		suffix := tail
		if sp := strings.IndexAny(tail, " \t"); sp >= 0 {
			suffix, rest = tail[:sp], tail[sp:]
		} else {
			rest = ""
		}
		m.Type = "tuple" + suffix
		for i, c := range splitTop(inner) {
			cm, err := parseParam(strings.TrimSpace(c))
			if err != nil {
				return m, err
			}
			if cm.Name == "" {
				cm.Name = fmt.Sprintf("field%d", i)
			}
			m.Components = append(m.Components, cm)
		}
	} else {
		fields := strings.Fields(p)
		m.Type, rest = fields[0], strings.Join(fields[1:], " ")
	}
	for _, f := range strings.Fields(rest) {
		switch f {
		case "indexed":
			m.Indexed = true
		case "memory", "calldata", "storage", "payable":
		default:
			m.Name = f
		}
	}
	m.Type = canonicalType(m.Type)
	if err := checkSized(m.Type); err != nil {
		return m, err
	}
	return m, nil
}

// checkSized rejects intN, uintN and bytesN sizes Solidity does not have,
// which the ABI package would otherwise accept.
func checkSized(t string) error {
	base := t
	if i := strings.IndexByte(t, '['); i >= 0 {
		base = t[:i]
	}
	var prefix string
	switch {
	case strings.HasPrefix(base, "uint"):
		prefix = "uint"
	case strings.HasPrefix(base, "int"):
		prefix = "int"
	case strings.HasPrefix(base, "bytes") && base != "bytes":
		prefix = "bytes"
	default:
		return nil
	}
	n, err := strconv.Atoi(base[len(prefix):])
	if err != nil {
		return fmt.Errorf("invalid type %q", t)
	}
	if prefix == "bytes" && (n < 1 || n > 32) || prefix != "bytes" && (n < 8 || n > 256 || n%8 != 0) {
		return fmt.Errorf("invalid type %q", t)
	}
	return nil
}

func canonicalType(t string) string {
	base, suffix := t, ""
	if i := strings.IndexByte(t, '['); i >= 0 {
		base, suffix = t[:i], t[i:]
	}
	switch base {
	case "uint":
		base = "uint256"
	case "int":
		base = "int256"
	case "byte":
		base = "bytes1"
	}
	return base + suffix
}

// EncodeArgs ABI-encodes JSON values for args. Integers may be JSON
// numbers, decimal strings or 0x-hex strings; addresses, bytes and bytesN
// are 0x-hex strings; arrays are JSON arrays; tuples are JSON objects keyed
// by field name or arrays in field order.
func EncodeArgs(args abi.Arguments, values []json.RawMessage) ([]byte, error) {
	if len(values) != len(args) {
		return nil, fmt.Errorf("expected %d arguments (%s), got %d", len(args), describeArgs(args), len(values))
	}
	goVals := make([]any, len(args))
	for i, a := range args {
		v, err := toGo(a.Type, values[i])
		if err != nil {
			return nil, fmt.Errorf("argument %d (%s %s): %w", i, a.Type.String(), a.Name, err)
		}
		goVals[i] = v.Interface()
	}
	return args.Pack(goVals...)
}

func describeArgs(args abi.Arguments) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = strings.TrimSpace(a.Type.String() + " " + a.Name)
	}
	return strings.Join(parts, ", ")
}

func toGo(t abi.Type, raw json.RawMessage) (reflect.Value, error) {
	goType := t.GetType()
	switch t.T {
	case abi.IntTy, abi.UintTy:
		n, err := jsonInt(raw)
		if err != nil {
			return reflect.Value{}, err
		}
		if t.T == abi.UintTy && n.Sign() < 0 {
			return reflect.Value{}, fmt.Errorf("negative value for unsigned type")
		}
		if n.BitLen() > t.Size {
			return reflect.Value{}, fmt.Errorf("value does not fit in %d bits", t.Size)
		}
		if goType == reflect.TypeFor[*big.Int]() {
			return reflect.ValueOf(n), nil
		}
		if t.T == abi.UintTy {
			return reflect.ValueOf(n.Uint64()).Convert(goType), nil
		}
		return reflect.ValueOf(n.Int64()).Convert(goType), nil
	case abi.BoolTy:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return reflect.Value{}, fmt.Errorf("expected true or false")
		}
		return reflect.ValueOf(b), nil
	case abi.StringTy:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return reflect.Value{}, fmt.Errorf("expected a string")
		}
		return reflect.ValueOf(s), nil
	case abi.AddressTy:
		s, err := jsonString(raw)
		if err != nil {
			return reflect.Value{}, err
		}
		addr, err := ParseAddress(s)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(addr), nil
	case abi.BytesTy:
		b, err := jsonHex(raw)
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(b), nil
	case abi.FixedBytesTy:
		b, err := jsonHex(raw)
		if err != nil {
			return reflect.Value{}, err
		}
		if len(b) != t.Size {
			return reflect.Value{}, fmt.Errorf("expected %d bytes, got %d", t.Size, len(b))
		}
		v := reflect.New(goType).Elem()
		reflect.Copy(v, reflect.ValueOf(b))
		return v, nil
	case abi.SliceTy, abi.ArrayTy:
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return reflect.Value{}, fmt.Errorf("expected a JSON array")
		}
		if t.T == abi.ArrayTy && len(items) != t.Size {
			return reflect.Value{}, fmt.Errorf("expected %d elements, got %d", t.Size, len(items))
		}
		var v reflect.Value
		if t.T == abi.SliceTy {
			v = reflect.MakeSlice(goType, len(items), len(items))
		} else {
			v = reflect.New(goType).Elem()
		}
		for i, item := range items {
			ev, err := toGo(*t.Elem, item)
			if err != nil {
				return reflect.Value{}, fmt.Errorf("element %d: %w", i, err)
			}
			v.Index(i).Set(ev)
		}
		return v, nil
	case abi.TupleTy:
		v := reflect.New(goType).Elem()
		var byName map[string]json.RawMessage
		var byIndex []json.RawMessage
		if err := json.Unmarshal(raw, &byIndex); err != nil {
			if err := json.Unmarshal(raw, &byName); err != nil {
				return reflect.Value{}, fmt.Errorf("expected a JSON object or array for a tuple")
			}
		}
		for i, elem := range t.TupleElems {
			var item json.RawMessage
			if byName != nil {
				var ok bool
				if item, ok = byName[t.TupleRawNames[i]]; !ok {
					return reflect.Value{}, fmt.Errorf("missing tuple field %q", t.TupleRawNames[i])
				}
			} else {
				if i >= len(byIndex) {
					return reflect.Value{}, fmt.Errorf("expected %d tuple fields", len(t.TupleElems))
				}
				item = byIndex[i]
			}
			ev, err := toGo(*elem, item)
			if err != nil {
				return reflect.Value{}, fmt.Errorf("field %s: %w", t.TupleRawNames[i], err)
			}
			v.Field(i).Set(ev)
		}
		return v, nil
	}
	return reflect.Value{}, fmt.Errorf("unsupported type %s", t.String())
}

func jsonString(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("expected a string")
	}
	return s, nil
}

func jsonInt(raw json.RawMessage) (*big.Int, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		var num json.Number
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&num); err != nil {
			return nil, fmt.Errorf("expected an integer")
		}
		s = num.String()
	}
	return ParseBig(s)
}

func jsonHex(raw json.RawMessage) ([]byte, error) {
	s, err := jsonString(raw)
	if err != nil {
		return nil, err
	}
	return ParseHex(s)
}

// Value is one decoded argument or return value.
type Value struct {
	Name  string `json:"name,omitempty"`
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// DecodeValues unpacks ABI-encoded data for args into named values.
func DecodeValues(args abi.Arguments, data []byte) ([]Value, error) {
	vals, err := args.Unpack(data)
	if err != nil {
		return nil, err
	}
	out := make([]Value, len(args))
	for i, a := range args {
		out[i] = Value{Name: a.Name, Type: a.Type.String(), Value: Normalize(vals[i])}
	}
	return out, nil
}

// Call is decoded calldata.
type Call struct {
	Selector  string  `json:"selector"`
	Signature string  `json:"signature"`
	Args      []Value `json:"args"`
	// Source says which ABI decoded it: "verified" (the explorer's verified
	// ABI for the contract) or "standard" (a built-in ERC-20/721 ABI, a guess
	// from the selector alone).
	Source string `json:"source"`
}

// DecodeCall decodes calldata with a, returning nil when the selector is
// not in it.
func DecodeCall(a *abi.ABI, data []byte, source string) *Call {
	if a == nil || len(data) < 4 {
		return nil
	}
	m, err := a.MethodById(data[:4])
	if err != nil {
		return nil
	}
	args, err := DecodeValues(m.Inputs, data[4:])
	if err != nil {
		return nil
	}
	return &Call{Selector: "0x" + hex.EncodeToString(data[:4]), Signature: m.Sig, Args: args, Source: source}
}

// Event is a decoded log.
type Event struct {
	Name      string  `json:"name"`
	Signature string  `json:"signature"`
	Args      []Value `json:"args"`
	Source    string  `json:"source"`
}

// DecodeLog decodes a log with a, returning nil when its first topic is not
// one of a's events or the data does not fit.
func DecodeLog(a *abi.ABI, topics []util.Hash, data []byte, source string) *Event {
	if a == nil || len(topics) == 0 {
		return nil
	}
	ev, err := a.EventByID(topics[0])
	if err != nil || ev.Anonymous {
		return nil
	}
	var indexed abi.Arguments
	for _, in := range ev.Inputs {
		if in.Indexed {
			indexed = append(indexed, in)
		}
	}
	if len(indexed) != len(topics)-1 {
		return nil // same signature, different indexing (ERC-20 vs ERC-721 Transfer)
	}
	topicVals := map[string]any{}
	if err := abi.ParseTopicsIntoMap(topicVals, indexed, topics[1:]); err != nil {
		return nil
	}
	dataVals, err := ev.Inputs.NonIndexed().Unpack(data)
	if err != nil {
		return nil
	}
	out := &Event{Name: ev.Name, Signature: ev.Sig, Source: source}
	di := 0
	for _, in := range ev.Inputs {
		v := Value{Name: in.Name, Type: in.Type.String()}
		if in.Indexed {
			if in.Type.T == abi.StringTy || in.Type.T == abi.BytesTy || in.Type.T == abi.SliceTy || in.Type.T == abi.TupleTy || in.Type.T == abi.ArrayTy {
				v.Value = map[string]string{"topic_hash": "dynamic indexed value; only its keccak256 is logged"}
			} else {
				v.Value = Normalize(topicVals[in.Name])
			}
		} else {
			v.Value = Normalize(dataVals[di])
			di++
		}
		out.Args = append(out.Args, v)
	}
	return out
}

// Normalize converts unpacked ABI values to JSON-friendly ones: integers as
// decimal strings (they overflow JSON numbers), addresses checksummed, bytes
// as 0x-hex, tuples as objects, and strings as untrusted text.
func Normalize(v any) any {
	switch v := v.(type) {
	case nil:
		return nil
	case *big.Int:
		return v.String()
	case util.Address:
		return v.Hex()
	case util.Hash:
		return v.Hex()
	case []byte:
		return "0x" + hex.EncodeToString(v)
	case string:
		return untrusted.NewN(v, 1024)
	case bool:
		return v
	case uint8, uint16, uint32, uint64, int8, int16, int32, int64:
		return fmt.Sprint(v)
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			b := make([]byte, rv.Len())
			reflect.Copy(reflect.ValueOf(b), rv)
			return "0x" + hex.EncodeToString(b)
		}
		fallthrough
	case reflect.Slice:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = Normalize(rv.Index(i).Interface())
		}
		return out
	case reflect.Struct:
		out := map[string]any{}
		for i := 0; i < rv.NumField(); i++ {
			f := rv.Type().Field(i)
			name := f.Tag.Get("json")
			if name == "" {
				name = f.Name
			}
			out[name] = Normalize(rv.Field(i).Interface())
		}
		return out
	case reflect.Pointer:
		if rv.IsNil() {
			return nil
		}
		return Normalize(rv.Elem().Interface())
	}
	return fmt.Sprint(v)
}

// StandardABI covers ERC-20 and ERC-721 calls and events, to decode
// unverified token contracts. Its decodings are labelled "standard": a
// matching selector does not prove the contract follows the standard.
var StandardABI = mustABI(`[
 {"type":"function","name":"transfer","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"name":"","type":"bool"}]},
 {"type":"function","name":"transferFrom","inputs":[{"name":"from","type":"address"},{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"name":"","type":"bool"}]},
 {"type":"function","name":"approve","inputs":[{"name":"spender","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"name":"","type":"bool"}]},
 {"type":"function","name":"balanceOf","inputs":[{"name":"owner","type":"address"}],"outputs":[{"name":"","type":"uint256"}],"stateMutability":"view"},
 {"type":"function","name":"allowance","inputs":[{"name":"owner","type":"address"},{"name":"spender","type":"address"}],"outputs":[{"name":"","type":"uint256"}],"stateMutability":"view"},
 {"type":"function","name":"totalSupply","inputs":[],"outputs":[{"name":"","type":"uint256"}],"stateMutability":"view"},
 {"type":"function","name":"decimals","inputs":[],"outputs":[{"name":"","type":"uint8"}],"stateMutability":"view"},
 {"type":"function","name":"symbol","inputs":[],"outputs":[{"name":"","type":"string"}],"stateMutability":"view"},
 {"type":"function","name":"name","inputs":[],"outputs":[{"name":"","type":"string"}],"stateMutability":"view"},
 {"type":"function","name":"ownerOf","inputs":[{"name":"tokenId","type":"uint256"}],"outputs":[{"name":"","type":"address"}],"stateMutability":"view"},
 {"type":"function","name":"safeTransferFrom","inputs":[{"name":"from","type":"address"},{"name":"to","type":"address"},{"name":"tokenId","type":"uint256"}],"outputs":[]},
 {"type":"function","name":"setApprovalForAll","inputs":[{"name":"operator","type":"address"},{"name":"approved","type":"bool"}],"outputs":[]},
 {"type":"event","name":"Transfer","inputs":[{"name":"from","type":"address","indexed":true},{"name":"to","type":"address","indexed":true},{"name":"value","type":"uint256","indexed":false}]},
 {"type":"event","name":"Approval","inputs":[{"name":"owner","type":"address","indexed":true},{"name":"spender","type":"address","indexed":true},{"name":"value","type":"uint256","indexed":false}]},
 {"type":"event","name":"ApprovalForAll","inputs":[{"name":"owner","type":"address","indexed":true},{"name":"operator","type":"address","indexed":true},{"name":"approved","type":"bool","indexed":false}]}
]`)

// standardNFTABI holds the ERC-721 Transfer and Approval, whose topic
// hashes collide with ERC-20's but which index the token ID.
var standardNFTABI = mustABI(`[
 {"type":"event","name":"Transfer","inputs":[{"name":"from","type":"address","indexed":true},{"name":"to","type":"address","indexed":true},{"name":"tokenId","type":"uint256","indexed":true}]},
 {"type":"event","name":"Approval","inputs":[{"name":"owner","type":"address","indexed":true},{"name":"approved","type":"address","indexed":true},{"name":"tokenId","type":"uint256","indexed":true}]}
]`)

// DecodeStandardLog tries the ERC-20 then the ERC-721 event layouts.
func DecodeStandardLog(topics []util.Hash, data []byte) *Event {
	if ev := DecodeLog(StandardABI, topics, data, "standard"); ev != nil {
		return ev
	}
	return DecodeLog(standardNFTABI, topics, data, "standard")
}

func mustABI(s string) *abi.ABI {
	a, err := ParseABI([]byte(s))
	if err != nil {
		panic(err)
	}
	return a
}

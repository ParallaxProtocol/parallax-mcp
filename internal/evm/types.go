// Package evm holds what the chain and tx tools share: argument parsing,
// the node's JSON shapes, and ABI encoding and decoding.
package evm

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/ParallaxProtocol/parallax/v2/util"
	"github.com/ParallaxProtocol/parallax/v2/util/hexutil"
)

// ParseAddress accepts a 0x-prefixed 20-byte hex address. A mixed-case
// address must carry a valid EIP-55 checksum, which catches typos.
func ParseAddress(s string) (util.Address, error) {
	s = strings.TrimSpace(s)
	if !util.IsHexAddress(s) || !strings.HasPrefix(strings.ToLower(s), "0x") {
		return util.Address{}, fmt.Errorf("%q is not a 0x-prefixed 20-byte hex address", clip(s))
	}
	addr := util.HexToAddress(s)
	body := s[2:]
	if body != strings.ToLower(body) && body != strings.ToUpper(body) && addr.Hex() != s {
		return util.Address{}, fmt.Errorf("%q has an invalid EIP-55 checksum; check the address or pass it in lower case", clip(s))
	}
	return addr, nil
}

// ParseHash accepts a 0x-prefixed 32-byte hex hash.
func ParseHash(s string) (util.Hash, error) {
	s = strings.TrimSpace(s)
	b, err := hexutil.Decode(s)
	if err != nil || len(b) != 32 {
		return util.Hash{}, fmt.Errorf("%q is not a 0x-prefixed 32-byte hex hash", clip(s))
	}
	return util.BytesToHash(b), nil
}

// ParseHex decodes 0x-prefixed hex; "0x" is empty.
func ParseHex(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "0x") && !strings.HasPrefix(s, "0X") {
		return nil, fmt.Errorf("expected 0x-prefixed hex, got %q", clip(s))
	}
	b, err := hex.DecodeString(s[2:])
	if err != nil {
		return nil, fmt.Errorf("invalid hex %q", clip(s))
	}
	return b, nil
}

// ParseBig accepts a decimal or 0x-hex integer, optionally negative.
func ParseBig(s string) (*big.Int, error) {
	s = strings.TrimSpace(s)
	neg := strings.HasPrefix(s, "-")
	digits := strings.TrimPrefix(s, "-")
	base := 10
	if strings.HasPrefix(digits, "0x") || strings.HasPrefix(digits, "0X") {
		digits, base = digits[2:], 16
	}
	n, ok := new(big.Int).SetString(digits, base)
	if !ok || digits == "" {
		return nil, fmt.Errorf("%q is not an integer (decimal or 0x-hex)", clip(s))
	}
	if neg {
		n.Neg(n)
	}
	return n, nil
}

// BlockRef is a parsed block selector.
type BlockRef struct {
	// Tag is "latest", "pending", "earliest" or a 0x-hex number; empty when
	// Hash is set.
	Tag  string
	Hash *util.Hash
}

// ParseBlockRef accepts "latest" (or ""), "pending", "earliest", a decimal
// or 0x-hex number, or a 32-byte block hash.
func ParseBlockRef(s string) (BlockRef, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "", "latest", "head":
		return BlockRef{Tag: "latest"}, nil
	case "pending", "earliest":
		return BlockRef{Tag: s}, nil
	}
	if strings.HasPrefix(s, "0x") && len(s) == 66 {
		h, err := ParseHash(s)
		if err != nil {
			return BlockRef{}, err
		}
		return BlockRef{Hash: &h}, nil
	}
	n, err := ParseBig(s)
	if err != nil || n.Sign() < 0 || !n.IsUint64() {
		return BlockRef{}, fmt.Errorf(`block %q: use "latest", a block number (decimal or 0x-hex) or a block hash`, clip(s))
	}
	return BlockRef{Tag: hexutil.EncodeBig(n)}, nil
}

// Param returns the value to pass as a block parameter of eth_getBalance and
// similar: a tag/number string, or an EIP-1898 {"blockHash": ...} object.
func (b BlockRef) Param() any {
	if b.Hash != nil {
		return map[string]any{"blockHash": b.Hash.Hex()}
	}
	return b.Tag
}

// Number returns the block number of a numeric ref.
func (b BlockRef) Number() (uint64, bool) {
	if b.Hash != nil || !strings.HasPrefix(b.Tag, "0x") {
		return 0, false
	}
	n, err := hexutil.DecodeUint64(b.Tag)
	return n, err == nil
}

func clip(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// RPCHeader is the node's JSON form of a block header, including Parallax's
// epochStartTime.
type RPCHeader struct {
	Number          hexutil.Uint64 `json:"number"`
	Hash            util.Hash      `json:"hash"`
	ParentHash      util.Hash      `json:"parentHash"`
	Timestamp       hexutil.Uint64 `json:"timestamp"`
	Miner           util.Address   `json:"miner"`
	Difficulty      *hexutil.Big   `json:"difficulty"`
	TotalDifficulty *hexutil.Big   `json:"totalDifficulty"`
	GasLimit        hexutil.Uint64 `json:"gasLimit"`
	GasUsed         hexutil.Uint64 `json:"gasUsed"`
	ExtraData       hexutil.Bytes  `json:"extraData"`
	Nonce           string         `json:"nonce"`
	MixHash         util.Hash      `json:"mixHash"`
	EpochStartTime  hexutil.Uint64 `json:"epochStartTime"`
	Size            hexutil.Uint64 `json:"size"`
	StateRoot       util.Hash      `json:"stateRoot"`
}

// RPCBlock is a block with transactions as hashes or objects.
type RPCBlock struct {
	RPCHeader
	Transactions []json.RawMessage `json:"transactions"`
	Uncles       []util.Hash       `json:"uncles"`
}

// RPCTx is the node's JSON form of a transaction.
type RPCTx struct {
	Hash             util.Hash       `json:"hash"`
	Type             hexutil.Uint64  `json:"type"`
	BlockHash        *util.Hash      `json:"blockHash"`
	BlockNumber      *hexutil.Big    `json:"blockNumber"`
	TransactionIndex *hexutil.Uint64 `json:"transactionIndex"`
	From             util.Address    `json:"from"`
	To               *util.Address   `json:"to"`
	Value            *hexutil.Big    `json:"value"`
	Gas              hexutil.Uint64  `json:"gas"`
	GasPrice         *hexutil.Big    `json:"gasPrice"`
	Nonce            hexutil.Uint64  `json:"nonce"`
	Input            hexutil.Bytes   `json:"input"`
	ChainID          *hexutil.Big    `json:"chainId"`
	AccessList       json.RawMessage `json:"accessList"`
	V                *hexutil.Big    `json:"v"`
}

// RPCLog is the node's JSON form of a log.
type RPCLog struct {
	Address     util.Address   `json:"address"`
	Topics      []util.Hash    `json:"topics"`
	Data        hexutil.Bytes  `json:"data"`
	BlockNumber hexutil.Uint64 `json:"blockNumber"`
	TxHash      util.Hash      `json:"transactionHash"`
	TxIndex     hexutil.Uint   `json:"transactionIndex"`
	LogIndex    hexutil.Uint   `json:"logIndex"`
	Removed     bool           `json:"removed"`
}

// RPCReceipt is the node's JSON form of a receipt.
type RPCReceipt struct {
	TxHash            util.Hash       `json:"transactionHash"`
	BlockHash         util.Hash       `json:"blockHash"`
	BlockNumber       hexutil.Uint64  `json:"blockNumber"`
	TransactionIndex  hexutil.Uint64  `json:"transactionIndex"`
	From              util.Address    `json:"from"`
	To                *util.Address   `json:"to"`
	ContractAddress   *util.Address   `json:"contractAddress"`
	GasUsed           hexutil.Uint64  `json:"gasUsed"`
	CumulativeGasUsed hexutil.Uint64  `json:"cumulativeGasUsed"`
	EffectiveGasPrice *hexutil.Big    `json:"effectiveGasPrice"`
	Status            *hexutil.Uint64 `json:"status"`
	Type              hexutil.Uint64  `json:"type"`
	Logs              []RPCLog        `json:"logs"`
}

// Selector returns the 4-byte function selector of calldata as hex, or "".
func Selector(input []byte) string {
	if len(input) < 4 {
		return ""
	}
	return "0x" + hex.EncodeToString(input[:4])
}

// HexPreview returns calldata as hex, cut to max bytes.
func HexPreview(b []byte, max int) (string, bool) {
	if len(b) <= max {
		return hexutil.Encode(b), false
	}
	return hexutil.Encode(b[:max]), true
}

// Uint64String formats n for JSON.
func Uint64String(n uint64) string { return strconv.FormatUint(n, 10) }

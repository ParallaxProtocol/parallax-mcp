// Package chain is the chain tool group: blocks, transactions, receipts,
// account state, contract calls, logs, gas and tracing, read from the node.
package chain

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/ParallaxProtocol/parallax/v2/script/abi"
	"github.com/ParallaxProtocol/parallax/v2/util"
	"github.com/ParallaxProtocol/parallax/v2/util/hexutil"

	"github.com/ParallaxProtocol/parallax-mcp/internal/config"
	"github.com/ParallaxProtocol/parallax-mcp/internal/evm"
	"github.com/ParallaxProtocol/parallax-mcp/internal/lax"
	"github.com/ParallaxProtocol/parallax-mcp/internal/server"
	"github.com/ParallaxProtocol/parallax-mcp/internal/untrusted"
)

const group = config.GroupChain

// Limits on what one call returns.
const (
	maxBlockTxs     = 200
	maxReceiptLogs  = 100
	maxInputPreview = 2048 // bytes of calldata shown as hex
	maxCodePreview  = 4096
	maxTraceCalls   = 300
	maxStorageKeys  = 20
)

// Register adds the chain tools.
func Register(r *server.Registrar) {
	t := &tools{r: r}
	server.Add(r, server.Spec{Group: group, Name: "chain_info", Title: "Chain info",
		Description: `Identity and state of the Parallax node this server reads: chain ID (2110 on mainnet), network ID, client version, head block (number, hash, timestamp, seconds since it was mined), sync status, peer count, current gas price, and the RPC namespaces the node serves (tools needing debug, txpool, admin or xhash work only when listed). Takes no arguments. Call it first to check the server is on the expected chain and synced.`,
	}, t.chainInfo)

	server.Add(r, server.Spec{Group: group, Name: "get_block", Title: "Get block",
		Description: `A block by number, hash or "latest". Returns number, hash, parent hash, timestamp (Unix seconds), seconds since its parent (block time; the target is 600), miner, difficulty, gas used/limit, size in bytes, epochStartTime (a Parallax header field: the timestamp of the last block at a multiple of 2016), extraData (as untrusted text when printable), the block subsidy in wei and LAX, and its transactions.
By default transactions are listed as hashes; with full_transactions=true each has from, to, value (wei and LAX), gas price (wei), nonce and the 4-byte selector of its calldata. At most 200 transactions are listed (Parallax blocks rarely have more); "transactions_truncated" says when the list was cut.
Examples: {"block": "latest"}, {"block": "59633", "full_transactions": true}, {"block": "0x7019…31e0"}.`,
	}, t.getBlock)

	server.Add(r, server.Spec{Group: group, Name: "get_transaction", Title: "Get transaction",
		Description: `A transaction by hash: block, position, from, to, value (wei and LAX), gas limit, gas price (wei; Parallax has no base fee, so this is the whole price per gas), nonce, type (0 legacy, 1 access list; Parallax has no type 2), and its calldata. Calldata is shown as hex (first 2048 bytes) and, when possible, decoded: with the contract's verified ABI from the explorer ("source": "verified") or else the built-in ERC-20/721 ABI ("source": "standard", a guess from the selector). Decoded strings are untrusted. Pending transactions have no block. Use get_receipt for status, gas used, fee and events.`,
	}, t.getTransaction)

	server.Add(r, server.Spec{Group: group, Name: "get_receipt", Title: "Get receipt",
		Description: `The receipt of a mined transaction: status (success or failed), gas used, effective gas price (wei), fee paid (wei and LAX = gas used × gas price), the address of a created contract, and its event logs (at most 100). Each log has its address and raw topics and data, plus a decoding with the contract's verified ABI or the standard ERC-20/721 events when they match. Event string values are untrusted. A transaction that is not mined yet has no receipt.`,
	}, t.getReceipt)

	server.Add(r, server.Spec{Group: group, Name: "get_balance", Title: "Get balance",
		Description: `The LAX balance (wei and LAX) and nonce (transactions sent) of an address, at the latest block or at "block" (number or hash). Reading a block older than the node's recent state window needs an archive node; the error says so when it is not one. Note that block rewards cannot be spent until 100 blocks after the block that paid them (coinbase maturity), although they are included in the balance.`,
	}, t.getBalance)

	server.Add(r, server.Spec{Group: group, Name: "get_code", Title: "Get code",
		Description: `The deployed bytecode of an address at the latest block or "block": whether it is a contract, the code size in bytes and its keccak256 hash. With include_code=true the bytecode itself is returned as hex (cut at 4096 bytes). An address with no code is an externally owned account (or a contract that self-destructed). For source code and ABI use explorer_contract.`,
	}, t.getCode)

	server.Add(r, server.Spec{Group: group, Name: "get_storage_at", Title: "Get storage slot",
		Description: `The raw 32-byte value of one or more storage slots of a contract (at most 20), at the latest block or "block". Slots are decimal or 0x-hex numbers, or 32-byte hex keys for mapping entries computed off-chain. Values are returned as 0x-hex and as unsigned decimal.`,
	}, t.getStorageAt)

	server.Add(r, server.Spec{Group: group, Name: "call_contract", Title: "Call contract (read-only)",
		Description: `Runs a read-only eth_call against a contract and decodes the result. Nothing is signed or sent: state changes are discarded.
Give either raw "data" (0x-hex calldata), or "function" plus "args":
- function is a name ("balanceOf") resolved against "abi" (a JSON ABI you pass) or, when abi is omitted, the contract's verified ABI from the explorer; or a full signature such as "balanceOf(address) returns (uint256)", which needs no ABI.
- args is a JSON array: integers as decimal strings (or 0x-hex), addresses and bytes as 0x-hex, arrays as arrays, tuples as objects or arrays.
Optional: from, value_wei, and block (number or hash, for historical state on an archive node).
Returns the raw return data and, when an ABI is known, the decoded values. A revert returns its reason (untrusted) instead.
Example: {"to": "0xd106…a214", "function": "balanceOf(address) returns (uint256)", "args": ["0x45a1…0248"]}.`,
	}, t.callContract)

	server.Add(r, server.Spec{Group: group, Name: "estimate_gas", Title: "Estimate gas",
		Description: `Estimates the gas a transaction would use (eth_estimateGas) and what it would cost at the node's current gas price: fee = gas × gas price, in wei and LAX. Takes the same call fields as call_contract (to, from, value_wei, and data or function+args) and nothing is sent. A plain LAX transfer uses 21,000 gas. Fails with the revert reason when the transaction would revert.`,
	}, t.estimateGas)

	server.Add(r, server.Spec{Group: group, Name: "get_logs", Title: "Get logs",
		Description: fmt.Sprintf(`Event logs in a block range, filtered by contract address(es) and topics (eth_getLogs). The range is inclusive and at most %d blocks (about %d days at 10 minute blocks); at most %d logs are returned, with "truncated": true when there were more (narrow the range). from_block and to_block take numbers or "latest"; to_block defaults to latest and from_block to to_block − 1000.
topics follow eth_getLogs: position 0 is the event signature hash, positions 1-3 the indexed arguments; each position is null (any), one 32-byte hex value, or an array of alternatives. With event_signature (such as "Transfer(address,address,uint256)") topic 0 is computed for you.
Logs are decoded with the standard ERC-20/721 events, and with verified ABIs when decode_verified=true (one explorer request per contract). Event string values are untrusted.`,
			r.Cfg.MaxLogRange, r.Cfg.MaxLogRange/144, r.Cfg.MaxLogs),
	}, t.getLogs)

	server.Add(r, server.Spec{Group: group, Name: "gas_price", Title: "Gas price and fee estimates",
		Description: `Current fee levels in wei and gwei (1 gwei = 10^9 wei). Parallax has no EIP-1559 base fee: a transaction pays gas used × gas price, all of it to the miner.
Returns the node's eth_gasPrice and, from Parallax's Bitcoin Core-style fee estimator (eth_estimateSmartFee), the price expected to confirm within 1, 3, 6, 36 and 144 blocks (10 minutes to a day) with the estimator's historical success rate for each; "legacy_fallback": true means the estimator had too little data and returned eth_gasPrice instead. Also the cost of a plain 21,000 gas transfer at each level, in LAX.`,
	}, t.gasPrice)

	server.Add(r, server.Spec{Group: group, Name: "get_proof", Title: "Get account proof",
		Description: `The Merkle proof of an account and optionally some of its storage slots (eth_getProof, EIP-1186) at the latest block or "block": balance, nonce, code hash, storage root and the slot values. The proof nodes are only counted unless include_proof=true, as they are large; use them to verify state against a block's stateRoot without trusting this server or the node.`,
	}, t.getProof)

	server.Add(r, server.Spec{Group: group, Name: "trace_transaction", Title: "Trace transaction", Requires: "debug",
		Description: `Re-executes a mined transaction on the node and returns what it did. Needs a node serving the debug namespace, and an archive node for transactions older than its recent state.
tracer selects the view:
- "call" (default): the tree of calls and contract creations it made (type, from, to, value in wei, gas, gas used, input selector, output, error or revert reason), with calldata decoded where the explorer has a verified ABI. At most 300 calls; deeper or wider trees are cut and "truncated" is set.
- "prestate": every account the transaction touched, with its balance (wei and LAX), nonce, code size and the storage slots it read or wrote, as they were before the transaction. Compare with get_balance or get_storage_at at the transaction's block for the state after it.
- "4byte": the function selectors it called, with calldata sizes and counts.
Revert reasons and decoded strings are untrusted.`,
	}, t.traceTransaction)

	server.Add(r, server.Spec{Group: group, Name: "trace_call", Title: "Trace call (simulation)", Requires: "debug",
		Description: `Simulates a call (the same fields as call_contract: to, from, value_wei, data or function+args, block) with the call tracer and returns its call tree, as trace_transaction does: useful to see what a transaction would do before anyone signs it. Nothing is sent. Needs a node serving the debug namespace.`,
	}, t.traceCall)
}

type tools struct{ r *server.Registrar }

func (t *tools) now() time.Time { return t.r.Now() }

// ---- chain_info ----

type chainInfoIn struct{}

type chainInfoOut struct {
	server.Partial
	ChainID         *uint64         `json:"chain_id,omitempty"`
	ExpectedChainID uint64          `json:"expected_chain_id"`
	ChainIDMatches  *bool           `json:"chain_id_matches,omitempty"`
	NetworkID       string          `json:"network_id,omitempty"`
	ClientVersion   *untrusted.Text `json:"client_version,omitempty"`
	Head            *headSummary    `json:"head,omitempty"`
	Syncing         any             `json:"syncing,omitempty"`
	PeerCount       *uint64         `json:"peer_count,omitempty"`
	GasPriceWei     string          `json:"gas_price_wei,omitempty"`
	Namespaces      []string        `json:"rpc_namespaces,omitempty"`
}

type headSummary struct {
	Number          uint64 `json:"number"`
	Hash            string `json:"hash"`
	Timestamp       uint64 `json:"timestamp"`
	SecondsSinceNow int64  `json:"seconds_since_mined"`
}

func (t *tools) chainInfo(ctx context.Context, _ chainInfoIn) (any, error) {
	n := t.r.Node
	out := chainInfoOut{ExpectedChainID: t.r.Cfg.ChainID}

	var (
		chainID, peers, gasPrice hexutil.Big
		netID, version           string
		head                     evm.RPCHeader
		syncing                  json.RawMessage
	)
	batch := []server.BatchElem{
		{Method: "eth_chainId", Result: &chainID},
		{Method: "net_version", Result: &netID},
		{Method: "web3_clientVersion", Result: &version},
		{Method: "eth_getBlockByNumber", Args: []any{"latest", false}, Result: &head},
		{Method: "eth_syncing", Result: &syncing},
		{Method: "net_peerCount", Result: &peers},
		{Method: "eth_gasPrice", Result: &gasPrice},
	}
	if err := n.Batch(ctx, batch); err != nil {
		return nil, err
	}
	for _, b := range batch {
		out.Fail(b.Method, b.Error)
	}
	if batch[0].Error == nil {
		id := (*big.Int)(&chainID).Uint64()
		match := id == t.r.Cfg.ChainID
		out.ChainID, out.ChainIDMatches = &id, &match
	}
	out.NetworkID = netID
	if version != "" {
		v := untrusted.New(version)
		out.ClientVersion = &v
	}
	if batch[3].Error == nil && head.Hash != (util.Hash{}) {
		out.Head = &headSummary{
			Number:          uint64(head.Number),
			Hash:            head.Hash.Hex(),
			Timestamp:       uint64(head.Timestamp),
			SecondsSinceNow: t.now().Unix() - int64(head.Timestamp),
		}
	}
	if batch[4].Error == nil {
		out.Syncing = syncState(syncing)
	}
	if batch[5].Error == nil {
		p := (*big.Int)(&peers).Uint64()
		out.PeerCount = &p
	}
	if batch[6].Error == nil {
		out.GasPriceWei = (*big.Int)(&gasPrice).String()
	}
	out.Namespaces = n.ModuleList(ctx)
	return out, nil
}

func syncState(raw json.RawMessage) any {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return map[string]any{"syncing": b}
	}
	var p struct {
		Current hexutil.Uint64 `json:"currentBlock"`
		Highest hexutil.Uint64 `json:"highestBlock"`
	}
	if json.Unmarshal(raw, &p) == nil {
		return map[string]any{"syncing": true, "current_block": uint64(p.Current), "highest_block": uint64(p.Highest)}
	}
	return nil
}

// ---- get_block ----

type getBlockIn struct {
	Block            string `json:"block,omitempty" jsonschema:"block number (decimal or 0x-hex), block hash, or \"latest\" (default)"`
	FullTransactions bool   `json:"full_transactions,omitempty" jsonschema:"list transaction summaries instead of hashes"`
}

type blockOut struct {
	Number          uint64          `json:"number"`
	Hash            string          `json:"hash"`
	ParentHash      string          `json:"parent_hash"`
	Timestamp       uint64          `json:"timestamp"`
	TimestampUTC    string          `json:"timestamp_utc"`
	SecondsAgo      int64           `json:"seconds_ago"`
	BlockTime       *uint64         `json:"block_time_seconds,omitempty"`
	Miner           string          `json:"miner"`
	Difficulty      string          `json:"difficulty"`
	TotalDifficulty string          `json:"total_difficulty,omitempty"`
	GasUsed         uint64          `json:"gas_used"`
	GasLimit        uint64          `json:"gas_limit"`
	GasUsedPercent  float64         `json:"gas_used_percent"`
	SizeBytes       uint64          `json:"size_bytes"`
	EpochStartTime  uint64          `json:"epoch_start_time"`
	ExtraData       string          `json:"extra_data"`
	ExtraText       *untrusted.Text `json:"extra_data_text,omitempty"`
	Subsidy         lax.Amount      `json:"block_subsidy"`
	TxCount         int             `json:"transaction_count"`
	Transactions    []any           `json:"transactions"`
	TxsTruncated    bool            `json:"transactions_truncated,omitempty"`
	Confirmations   *uint64         `json:"confirmations,omitempty"`
}

type txSummary struct {
	Hash     string     `json:"hash"`
	From     string     `json:"from"`
	To       string     `json:"to,omitempty"`
	Creates  bool       `json:"creates_contract,omitempty"`
	Value    lax.Amount `json:"value"`
	GasPrice string     `json:"gas_price_wei"`
	Gas      uint64     `json:"gas_limit"`
	Nonce    uint64     `json:"nonce"`
	Selector string     `json:"selector,omitempty"`
}

func (t *tools) fetchBlock(ctx context.Context, ref evm.BlockRef, full bool) (*evm.RPCBlock, error) {
	var raw json.RawMessage
	var err error
	if ref.Hash != nil {
		raw, err = t.r.Node.Raw(ctx, "eth_getBlockByHash", ref.Hash.Hex(), full)
	} else {
		raw, err = t.r.Node.Raw(ctx, "eth_getBlockByNumber", ref.Tag, full)
	}
	if err != nil {
		return nil, err
	}
	if isNull(raw) {
		return nil, server.Inputf("block not found")
	}
	var b evm.RPCBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("decoding block: %w", err)
	}
	return &b, nil
}

func (t *tools) getBlock(ctx context.Context, in getBlockIn) (any, error) {
	ref, err := evm.ParseBlockRef(in.Block)
	if err != nil {
		return nil, server.Inputf("%v", err)
	}
	b, err := t.fetchBlock(ctx, ref, in.FullTransactions)
	if err != nil {
		return nil, err
	}
	out := blockOut{
		Number:         uint64(b.Number),
		Hash:           b.Hash.Hex(),
		ParentHash:     b.ParentHash.Hex(),
		Timestamp:      uint64(b.Timestamp),
		TimestampUTC:   time.Unix(int64(b.Timestamp), 0).UTC().Format(time.RFC3339),
		SecondsAgo:     t.now().Unix() - int64(b.Timestamp),
		Miner:          b.Miner.Hex(),
		Difficulty:     bigStr(b.Difficulty),
		GasUsed:        uint64(b.GasUsed),
		GasLimit:       uint64(b.GasLimit),
		SizeBytes:      uint64(b.Size),
		EpochStartTime: uint64(b.EpochStartTime),
		ExtraData:      hexutil.Encode(b.ExtraData),
		Subsidy:        lax.NewAmount(lax.BlockReward(uint64(b.Number))),
		TxCount:        len(b.Transactions),
	}
	if b.TotalDifficulty != nil {
		out.TotalDifficulty = bigStr(b.TotalDifficulty)
	}
	if b.GasLimit > 0 {
		out.GasUsedPercent = round(float64(b.GasUsed)/float64(b.GasLimit)*100, 4)
	}
	if untrusted.Printable(b.ExtraData) {
		out.ExtraText = untrusted.Ptr(string(b.ExtraData))
	}
	if b.Number > 0 {
		var parent evm.RPCHeader
		if err := t.r.Node.Call(ctx, &parent, "eth_getBlockByHash", b.ParentHash.Hex(), false); err == nil && parent.Timestamp > 0 {
			bt := uint64(b.Timestamp) - uint64(parent.Timestamp)
			out.BlockTime = &bt
		}
	}
	var headNum hexutil.Uint64
	if err := t.r.Node.Call(ctx, &headNum, "eth_blockNumber"); err == nil && uint64(headNum) >= uint64(b.Number) {
		c := uint64(headNum) - uint64(b.Number) + 1
		out.Confirmations = &c
	}

	txs := b.Transactions
	if len(txs) > maxBlockTxs {
		txs, out.TxsTruncated = txs[:maxBlockTxs], true
	}
	out.Transactions = make([]any, 0, len(txs))
	for _, raw := range txs {
		if !in.FullTransactions {
			var h util.Hash
			if err := json.Unmarshal(raw, &h); err == nil {
				out.Transactions = append(out.Transactions, h.Hex())
			}
			continue
		}
		var tx evm.RPCTx
		if err := json.Unmarshal(raw, &tx); err != nil {
			continue
		}
		out.Transactions = append(out.Transactions, summarizeTx(&tx))
	}
	return out, nil
}

func summarizeTx(tx *evm.RPCTx) txSummary {
	s := txSummary{
		Hash:     tx.Hash.Hex(),
		From:     tx.From.Hex(),
		Value:    lax.NewAmount((*big.Int)(tx.Value)),
		GasPrice: bigStr(tx.GasPrice),
		Gas:      uint64(tx.Gas),
		Nonce:    uint64(tx.Nonce),
		Selector: evm.Selector(tx.Input),
	}
	if tx.To != nil {
		s.To = tx.To.Hex()
	} else {
		s.Creates = true
	}
	return s
}

// ---- get_transaction ----

type hashIn struct {
	Hash string `json:"hash" jsonschema:"transaction hash, 0x-prefixed 32 bytes"`
}

type txOut struct {
	server.Partial
	Hash          string     `json:"hash"`
	Pending       bool       `json:"pending"`
	BlockNumber   *uint64    `json:"block_number,omitempty"`
	BlockHash     string     `json:"block_hash,omitempty"`
	Index         *uint64    `json:"transaction_index,omitempty"`
	From          string     `json:"from"`
	To            string     `json:"to,omitempty"`
	Creates       bool       `json:"creates_contract,omitempty"`
	Value         lax.Amount `json:"value"`
	GasLimit      uint64     `json:"gas_limit"`
	GasPrice      string     `json:"gas_price_wei"`
	MaxFee        lax.Amount `json:"max_fee"`
	Nonce         uint64     `json:"nonce"`
	Type          uint64     `json:"type"`
	ChainID       string     `json:"chain_id,omitempty"`
	Input         string     `json:"input"`
	InputBytes    int        `json:"input_bytes"`
	InputCut      bool       `json:"input_truncated,omitempty"`
	Selector      string     `json:"selector,omitempty"`
	Decoded       *evm.Call  `json:"decoded_input,omitempty"`
	Confirmations *uint64    `json:"confirmations,omitempty"`
}

func (t *tools) getTransaction(ctx context.Context, in hashIn) (any, error) {
	h, err := evm.ParseHash(in.Hash)
	if err != nil {
		return nil, server.Inputf("%v", err)
	}
	raw, err := t.r.Node.Raw(ctx, "eth_getTransactionByHash", h.Hex())
	if err != nil {
		return nil, err
	}
	if isNull(raw) {
		return nil, server.Inputf("transaction %s not found (not mined and not in this node's mempool)", h.Hex())
	}
	var tx evm.RPCTx
	if err := json.Unmarshal(raw, &tx); err != nil {
		return nil, fmt.Errorf("decoding transaction: %w", err)
	}
	gasPrice := (*big.Int)(tx.GasPrice)
	out := txOut{
		Hash:       tx.Hash.Hex(),
		Pending:    tx.BlockHash == nil,
		From:       tx.From.Hex(),
		Value:      lax.NewAmount((*big.Int)(tx.Value)),
		GasLimit:   uint64(tx.Gas),
		GasPrice:   bigStr(tx.GasPrice),
		MaxFee:     lax.NewAmount(mulU64(gasPrice, uint64(tx.Gas))),
		Nonce:      uint64(tx.Nonce),
		Type:       uint64(tx.Type),
		InputBytes: len(tx.Input),
		Selector:   evm.Selector(tx.Input),
	}
	out.Input, out.InputCut = evm.HexPreview(tx.Input, maxInputPreview)
	if tx.ChainID != nil {
		out.ChainID = bigStr(tx.ChainID)
	}
	if tx.BlockHash != nil {
		out.BlockHash = tx.BlockHash.Hex()
		bn := (*big.Int)(tx.BlockNumber).Uint64()
		out.BlockNumber = &bn
		if tx.TransactionIndex != nil {
			i := uint64(*tx.TransactionIndex)
			out.Index = &i
		}
		var head hexutil.Uint64
		if err := t.r.Node.Call(ctx, &head, "eth_blockNumber"); err == nil && uint64(head) >= bn {
			c := uint64(head) - bn + 1
			out.Confirmations = &c
		}
	}
	if tx.To != nil {
		out.To = tx.To.Hex()
		if len(tx.Input) >= 4 {
			dec, err := t.decodeCall(ctx, *tx.To, tx.Input)
			out.Decoded = dec
			out.Fail("explorer", err)
		}
	} else {
		out.Creates = true
	}
	return out, nil
}

func (t *tools) decodeCall(ctx context.Context, to util.Address, input []byte) (*evm.Call, error) {
	return evm.DecodeCalldata(ctx, t.r.Explorer, to, input)
}

func (t *tools) verifiedABI(ctx context.Context, addr util.Address) (*abi.ABI, error) {
	return evm.VerifiedABI(ctx, t.r.Explorer, addr)
}

// ---- get_receipt ----

type receiptOut struct {
	server.Partial
	Hash              string     `json:"hash"`
	BlockNumber       uint64     `json:"block_number"`
	BlockHash         string     `json:"block_hash"`
	Index             uint64     `json:"transaction_index"`
	From              string     `json:"from"`
	To                string     `json:"to,omitempty"`
	ContractCreated   string     `json:"contract_created,omitempty"`
	Status            string     `json:"status"`
	GasUsed           uint64     `json:"gas_used"`
	EffectiveGasPrice string     `json:"effective_gas_price_wei"`
	Fee               lax.Amount `json:"fee"`
	Type              uint64     `json:"type"`
	LogCount          int        `json:"log_count"`
	Logs              []logOut   `json:"logs"`
	LogsTruncated     bool       `json:"logs_truncated,omitempty"`
}

type logOut struct {
	Index   uint64     `json:"log_index"`
	Address string     `json:"address"`
	Topics  []string   `json:"topics"`
	Data    string     `json:"data"`
	DataCut bool       `json:"data_truncated,omitempty"`
	Block   uint64     `json:"block_number,omitempty"`
	TxHash  string     `json:"transaction_hash,omitempty"`
	Event   *evm.Event `json:"event,omitempty"`
	Removed bool       `json:"removed,omitempty"`
}

func (t *tools) getReceipt(ctx context.Context, in hashIn) (any, error) {
	h, err := evm.ParseHash(in.Hash)
	if err != nil {
		return nil, server.Inputf("%v", err)
	}
	raw, err := t.r.Node.Raw(ctx, "eth_getTransactionReceipt", h.Hex())
	if err != nil {
		return nil, err
	}
	if isNull(raw) {
		return nil, server.Inputf("no receipt for %s: the transaction is unknown or not mined yet", h.Hex())
	}
	var rc evm.RPCReceipt
	if err := json.Unmarshal(raw, &rc); err != nil {
		return nil, fmt.Errorf("decoding receipt: %w", err)
	}
	out := receiptOut{
		Hash:              rc.TxHash.Hex(),
		BlockNumber:       uint64(rc.BlockNumber),
		BlockHash:         rc.BlockHash.Hex(),
		Index:             uint64(rc.TransactionIndex),
		From:              rc.From.Hex(),
		GasUsed:           uint64(rc.GasUsed),
		EffectiveGasPrice: bigStr(rc.EffectiveGasPrice),
		Fee:               lax.NewAmount(mulU64((*big.Int)(rc.EffectiveGasPrice), uint64(rc.GasUsed))),
		Type:              uint64(rc.Type),
		LogCount:          len(rc.Logs),
		Status:            "unknown",
	}
	if rc.Status != nil {
		out.Status = map[bool]string{true: "success", false: "failed"}[*rc.Status == 1]
	}
	if rc.To != nil {
		out.To = rc.To.Hex()
	}
	if rc.ContractAddress != nil {
		out.ContractCreated = rc.ContractAddress.Hex()
	}
	logs := rc.Logs
	if len(logs) > maxReceiptLogs {
		logs, out.LogsTruncated = logs[:maxReceiptLogs], true
	}
	out.Logs = t.decodeLogs(ctx, logs, true, &out.Partial, false)
	return out, nil
}

// decodeLogs formats logs, decoding each with the verified ABI of its
// contract when verified is set, else with the standard token events.
func (t *tools) decodeLogs(ctx context.Context, logs []evm.RPCLog, verified bool, p *server.Partial, withTx bool) []logOut {
	abis := map[util.Address]*abi.ABI{}
	out := make([]logOut, 0, len(logs))
	for _, l := range logs {
		lo := logOut{Index: uint64(l.LogIndex), Address: l.Address.Hex(), Removed: l.Removed}
		if withTx {
			lo.Block, lo.TxHash = uint64(l.BlockNumber), l.TxHash.Hex()
		}
		for _, tp := range l.Topics {
			lo.Topics = append(lo.Topics, tp.Hex())
		}
		lo.Data, lo.DataCut = evm.HexPreview(l.Data, 1024)
		if verified {
			a, seen := abis[l.Address]
			if !seen {
				var err error
				a, err = t.verifiedABI(ctx, l.Address)
				p.Fail("explorer", err)
				abis[l.Address] = a
			}
			lo.Event = evm.DecodeLog(a, l.Topics, l.Data, "verified")
		}
		if lo.Event == nil {
			lo.Event = evm.DecodeStandardLog(l.Topics, l.Data)
		}
		out = append(out, lo)
	}
	return out
}

// ---- get_balance ----

type addrBlockIn struct {
	Address string `json:"address" jsonschema:"0x-prefixed 20-byte address"`
	Block   string `json:"block,omitempty" jsonschema:"block number, hash or \"latest\" (default); older blocks may need an archive node"`
}

type balanceOut struct {
	Address string     `json:"address"`
	Block   string     `json:"block"`
	Balance lax.Amount `json:"balance"`
	Nonce   uint64     `json:"nonce"`
}

func (t *tools) getBalance(ctx context.Context, in addrBlockIn) (any, error) {
	addr, ref, err := parseAddrBlock(in.Address, in.Block)
	if err != nil {
		return nil, err
	}
	var bal hexutil.Big
	var nonce hexutil.Uint64
	batch := []server.BatchElem{
		{Method: "eth_getBalance", Args: []any{addr.Hex(), ref.Param()}, Result: &bal},
		{Method: "eth_getTransactionCount", Args: []any{addr.Hex(), ref.Param()}, Result: &nonce},
	}
	if err := t.r.Node.Batch(ctx, batch); err != nil {
		return nil, err
	}
	for _, b := range batch {
		if b.Error != nil {
			return nil, stateErr(b.Error)
		}
	}
	return balanceOut{Address: addr.Hex(), Block: blockLabel(ref, in.Block), Balance: lax.NewAmount((*big.Int)(&bal)), Nonce: uint64(nonce)}, nil
}

// ---- get_code ----

type getCodeIn struct {
	Address     string `json:"address" jsonschema:"0x-prefixed 20-byte address"`
	Block       string `json:"block,omitempty" jsonschema:"block number, hash or \"latest\" (default)"`
	IncludeCode bool   `json:"include_code,omitempty" jsonschema:"also return the bytecode as hex (cut at 4096 bytes)"`
}

type codeOut struct {
	Address    string `json:"address"`
	Block      string `json:"block"`
	IsContract bool   `json:"is_contract"`
	SizeBytes  int    `json:"size_bytes"`
	CodeHash   string `json:"code_hash,omitempty"`
	Code       string `json:"code,omitempty"`
	CodeCut    bool   `json:"code_truncated,omitempty"`
}

func (t *tools) getCode(ctx context.Context, in getCodeIn) (any, error) {
	addr, ref, err := parseAddrBlock(in.Address, in.Block)
	if err != nil {
		return nil, err
	}
	var code hexutil.Bytes
	if err := t.r.Node.Call(ctx, &code, "eth_getCode", addr.Hex(), ref.Param()); err != nil {
		return nil, stateErr(err)
	}
	out := codeOut{Address: addr.Hex(), Block: blockLabel(ref, in.Block), IsContract: len(code) > 0, SizeBytes: len(code)}
	if len(code) > 0 {
		out.CodeHash = util.BytesToHash(keccak(code)).Hex()
		if in.IncludeCode {
			out.Code, out.CodeCut = evm.HexPreview(code, maxCodePreview)
		}
	}
	return out, nil
}

// ---- get_storage_at ----

type storageIn struct {
	Address string   `json:"address" jsonschema:"contract address"`
	Slots   []string `json:"slots" jsonschema:"storage slots: decimal or 0x-hex numbers, or 32-byte hex keys (at most 20)"`
	Block   string   `json:"block,omitempty" jsonschema:"block number, hash or \"latest\" (default)"`
}

type slotOut struct {
	Slot    string `json:"slot"`
	Value   string `json:"value"`
	Decimal string `json:"as_uint256"`
}

func (t *tools) getStorageAt(ctx context.Context, in storageIn) (any, error) {
	addr, ref, err := parseAddrBlock(in.Address, in.Block)
	if err != nil {
		return nil, err
	}
	if len(in.Slots) == 0 || len(in.Slots) > maxStorageKeys {
		return nil, server.Inputf("give between 1 and %d slots", maxStorageKeys)
	}
	keys := make([]util.Hash, len(in.Slots))
	for i, s := range in.Slots {
		n, err := evm.ParseBig(s)
		if err != nil || n.Sign() < 0 || n.BitLen() > 256 {
			return nil, server.Inputf("slot %q is not a 256-bit unsigned integer or 32-byte key", s)
		}
		keys[i] = util.BigToHash(n)
	}
	vals := make([]hexutil.Bytes, len(keys))
	batch := make([]server.BatchElem, len(keys))
	for i, k := range keys {
		batch[i] = server.BatchElem{Method: "eth_getStorageAt", Args: []any{addr.Hex(), k.Hex(), ref.Param()}, Result: &vals[i]}
	}
	if err := t.r.Node.Batch(ctx, batch); err != nil {
		return nil, err
	}
	out := make([]slotOut, len(keys))
	for i, b := range batch {
		if b.Error != nil {
			return nil, stateErr(b.Error)
		}
		v := util.BytesToHash(vals[i])
		out[i] = slotOut{Slot: keys[i].Hex(), Value: v.Hex(), Decimal: v.Big().String()}
	}
	return map[string]any{"address": addr.Hex(), "block": blockLabel(ref, in.Block), "slots": out}, nil
}

// ---- call_contract / estimate_gas / trace_call ----

type callIn struct {
	To       string `json:"to" jsonschema:"contract address"`
	Data     string `json:"data,omitempty" jsonschema:"raw 0x-hex calldata; alternative to function+args"`
	Function string `json:"function,omitempty" jsonschema:"function name resolved via abi or the verified ABI, or a full signature such as \"balanceOf(address) returns (uint256)\""`
	Args     []any  `json:"args,omitempty" jsonschema:"function arguments as a JSON array; big integers as decimal strings"`
	ABI      string `json:"abi,omitempty" jsonschema:"contract ABI as a JSON string; when omitted the explorer's verified ABI is used"`
	From     string `json:"from,omitempty" jsonschema:"sender address (optional)"`
	ValueWei string `json:"value_wei,omitempty" jsonschema:"LAX to send with the call, in wei (decimal)"`
	Block    string `json:"block,omitempty" jsonschema:"block number, hash or \"latest\" (default)"`
}

// preparedCall is a resolved call: its eth_call object and, when known, the
// method for decoding the result.
type preparedCall struct {
	msg    map[string]any
	method *abi.Method
	ref    evm.BlockRef
	to     util.Address
	data   []byte
	source string
}

func (t *tools) prepareCall(ctx context.Context, in callIn, p *server.Partial) (*preparedCall, error) {
	to, err := evm.ParseAddress(in.To)
	if err != nil {
		return nil, server.Inputf("to: %v", err)
	}
	ref, err := evm.ParseBlockRef(in.Block)
	if err != nil {
		return nil, server.Inputf("%v", err)
	}
	pc := &preparedCall{to: to, ref: ref, msg: map[string]any{"to": to.Hex()}}

	switch {
	case in.Data != "" && in.Function != "":
		return nil, server.Inputf("give either data or function, not both")
	case in.Data != "":
		if pc.data, err = evm.ParseHex(in.Data); err != nil {
			return nil, server.Inputf("data: %v", err)
		}
	case in.Function != "":
		m, src, err := t.resolveMethod(ctx, to, in.Function, in.ABI, p)
		if err != nil {
			return nil, err
		}
		raw, err := rawList(in.Args)
		if err != nil {
			return nil, err
		}
		args, err := evm.EncodeArgs(m.Inputs, raw)
		if err != nil {
			return nil, server.Inputf("%s: %v", m.Sig, err)
		}
		pc.method, pc.source = m, src
		pc.data = append(append([]byte{}, m.ID...), args...)
	default:
		return nil, server.Inputf("give data or function")
	}
	pc.msg["data"] = hexutil.Encode(pc.data)
	if in.From != "" {
		from, err := evm.ParseAddress(in.From)
		if err != nil {
			return nil, server.Inputf("from: %v", err)
		}
		pc.msg["from"] = from.Hex()
	}
	if in.ValueWei != "" {
		v, err := evm.ParseBig(in.ValueWei)
		if err != nil || v.Sign() < 0 {
			return nil, server.Inputf("value_wei must be a non-negative integer number of wei")
		}
		pc.msg["value"] = hexutil.EncodeBig(v)
	}
	return pc, nil
}

// resolveMethod finds the method to call: a full signature, a name in the
// given ABI, or a name in the contract's verified ABI.
func (t *tools) resolveMethod(ctx context.Context, to util.Address, fn, abiJSON string, p *server.Partial) (*abi.Method, string, error) {
	if strings.Contains(fn, "(") {
		m, err := evm.ParseSignature(fn)
		if err != nil {
			return nil, "", server.Inputf("%v", err)
		}
		return &m, "signature", nil
	}
	var a *abi.ABI
	source := "provided"
	if abiJSON != "" {
		var err error
		if a, err = evm.ParseABI([]byte(abiJSON)); err != nil {
			return nil, "", server.Inputf("%v", err)
		}
	} else {
		var err error
		a, err = t.verifiedABI(ctx, to)
		if err != nil {
			return nil, "", fmt.Errorf("fetching the verified ABI of %s from the explorer: %w; pass a full signature such as %q instead", to.Hex(), err, fn+"(address)")
		}
		if a == nil {
			return nil, "", server.Inputf("%s has no verified ABI on the explorer; pass abi, or a full signature such as %q", to.Hex(), fn+"(address) returns (uint256)")
		}
		source = "verified"
	}
	var matches []abi.Method
	for _, m := range a.Methods {
		if m.RawName == fn || m.Name == fn {
			matches = append(matches, m)
		}
	}
	switch len(matches) {
	case 0:
		return nil, "", server.Inputf("the ABI has no function %q", fn)
	case 1:
		return &matches[0], source, nil
	}
	sigs := make([]string, len(matches))
	for i, m := range matches {
		sigs[i] = m.Sig
	}
	return nil, "", server.Inputf("%q is overloaded (%s); pass the full signature", fn, strings.Join(sigs, ", "))
}

type callOut struct {
	server.Partial
	To       string          `json:"to"`
	Block    string          `json:"block"`
	Function string          `json:"function,omitempty"`
	ABI      string          `json:"abi_source,omitempty"`
	Result   string          `json:"result"`
	Decoded  []evm.Value     `json:"decoded,omitempty"`
	Reverted bool            `json:"reverted,omitempty"`
	Revert   *untrusted.Text `json:"revert_reason,omitempty"`
	Error    string          `json:"error,omitempty"`
}

func (t *tools) callContract(ctx context.Context, in callIn) (any, error) {
	out := callOut{}
	pc, err := t.prepareCall(ctx, in, &out.Partial)
	if err != nil {
		return nil, err
	}
	out.To, out.Block = pc.to.Hex(), blockLabel(pc.ref, in.Block)
	if pc.method != nil {
		out.Function, out.ABI = pc.method.Sig, pc.source
	}
	var res hexutil.Bytes
	if err := t.r.Node.Call(ctx, &res, "eth_call", pc.msg, pc.ref.Param()); err != nil {
		if reason, data, ok := revertInfo(err); ok {
			out.Reverted, out.Revert, out.Result = true, reason, data
			return out, nil
		}
		return nil, stateErr(err)
	}
	out.Result = hexutil.Encode(res)
	if pc.method != nil && len(pc.method.Outputs) > 0 {
		vals, err := evm.DecodeValues(pc.method.Outputs, res)
		if err != nil {
			out.Error = "the result does not decode as " + pc.method.Sig + "'s outputs: " + err.Error()
		}
		out.Decoded = vals
	}
	return out, nil
}

type gasOut struct {
	server.Partial
	Gas         uint64     `json:"gas"`
	GasPriceWei string     `json:"gas_price_wei"`
	Fee         lax.Amount `json:"fee_at_gas_price"`
	Function    string     `json:"function,omitempty"`
}

func (t *tools) estimateGas(ctx context.Context, in callIn) (any, error) {
	out := gasOut{}
	pc, err := t.prepareCall(ctx, in, &out.Partial)
	if err != nil {
		return nil, err
	}
	if pc.method != nil {
		out.Function = pc.method.Sig
	}
	var gas hexutil.Uint64
	var price hexutil.Big
	batch := []server.BatchElem{
		{Method: "eth_estimateGas", Args: []any{pc.msg}, Result: &gas},
		{Method: "eth_gasPrice", Result: &price},
	}
	if err := t.r.Node.Batch(ctx, batch); err != nil {
		return nil, err
	}
	if batch[0].Error != nil {
		if reason, _, ok := revertInfo(batch[0].Error); ok && reason != nil {
			return nil, fmt.Errorf("the transaction would revert: %s", reason.String())
		}
		return nil, batch[0].Error
	}
	out.Gas = uint64(gas)
	if batch[1].Error == nil {
		out.GasPriceWei = (*big.Int)(&price).String()
		out.Fee = lax.NewAmount(mulU64((*big.Int)(&price), uint64(gas)))
	} else {
		out.Fail("rpc", batch[1].Error)
	}
	return out, nil
}

// ---- get_logs ----

type logsIn struct {
	FromBlock      string   `json:"from_block,omitempty" jsonschema:"first block (number or \"latest\"); default to_block - 1000"`
	ToBlock        string   `json:"to_block,omitempty" jsonschema:"last block, inclusive (number or \"latest\", the default)"`
	Addresses      []string `json:"addresses,omitempty" jsonschema:"contract addresses to match (any of them); all contracts when empty"`
	EventSignature string   `json:"event_signature,omitempty" jsonschema:"event signature such as \"Transfer(address,address,uint256)\"; sets topic 0"`
	Topics         []any    `json:"topics,omitempty" jsonschema:"eth_getLogs topic filter: up to 4 positions, each null, a 32-byte hex value or an array of alternatives"`
	DecodeVerified bool     `json:"decode_verified,omitempty" jsonschema:"decode logs with each contract's verified ABI from the explorer"`
}

type logsOut struct {
	server.Partial
	FromBlock uint64   `json:"from_block"`
	ToBlock   uint64   `json:"to_block"`
	Count     int      `json:"count"`
	Truncated bool     `json:"truncated,omitempty"`
	Logs      []logOut `json:"logs"`
}

func (t *tools) getLogs(ctx context.Context, in logsIn) (any, error) {
	var head hexutil.Uint64
	if err := t.r.Node.Call(ctx, &head, "eth_blockNumber"); err != nil {
		return nil, err
	}
	resolve := func(s string, def uint64) (uint64, error) {
		if s == "" {
			return def, nil
		}
		ref, err := evm.ParseBlockRef(s)
		if err != nil || ref.Hash != nil || ref.Tag == "pending" {
			return 0, server.Inputf("block %q: use a block number or \"latest\"", s)
		}
		if ref.Tag == "latest" {
			return uint64(head), nil
		}
		if ref.Tag == "earliest" {
			return 0, nil
		}
		n, _ := ref.Number()
		return n, nil
	}
	to, err := resolve(in.ToBlock, uint64(head))
	if err != nil {
		return nil, err
	}
	from, err := resolve(in.FromBlock, saturatingSub(to, 1000))
	if err != nil {
		return nil, err
	}
	maxRange := t.r.Cfg.MaxLogRange
	if from > to {
		return nil, server.Inputf("from_block %d is after to_block %d", from, to)
	}
	if to-from+1 > maxRange {
		return nil, server.Inputf("the range %d-%d is %d blocks; the limit is %d. Split it into smaller ranges", from, to, to-from+1, maxRange)
	}

	filter := map[string]any{"fromBlock": hexutil.EncodeUint64(from), "toBlock": hexutil.EncodeUint64(to)}
	if len(in.Addresses) > 0 {
		if len(in.Addresses) > 50 {
			return nil, server.Inputf("at most 50 addresses")
		}
		addrs := make([]string, len(in.Addresses))
		for i, a := range in.Addresses {
			addr, err := evm.ParseAddress(a)
			if err != nil {
				return nil, server.Inputf("addresses[%d]: %v", i, err)
			}
			addrs[i] = addr.Hex()
		}
		filter["address"] = addrs
	}
	rawTopics, err := rawList(in.Topics)
	if err != nil {
		return nil, err
	}
	topics, err := buildTopics(in.EventSignature, rawTopics)
	if err != nil {
		return nil, err
	}
	if topics != nil {
		filter["topics"] = topics
	}
	if len(in.Addresses) == 0 && topics == nil && to-from+1 > 1000 {
		return nil, server.Inputf("without addresses or topics the range is limited to 1000 blocks")
	}

	var logs []evm.RPCLog
	if err := t.r.Node.Call(ctx, &logs, "eth_getLogs", filter); err != nil {
		return nil, err
	}
	out := logsOut{FromBlock: from, ToBlock: to, Count: len(logs)}
	if len(logs) > t.r.Cfg.MaxLogs {
		logs, out.Truncated = logs[:t.r.Cfg.MaxLogs], true
	}
	out.Logs = t.decodeLogs(ctx, logs, in.DecodeVerified, &out.Partial, true)
	return out, nil
}

func buildTopics(sig string, raw []json.RawMessage) ([]any, error) {
	if len(raw) > 4 {
		return nil, server.Inputf("at most 4 topic positions")
	}
	topics := make([]any, len(raw))
	for i, r := range raw {
		if isNull(r) {
			continue
		}
		var one string
		if json.Unmarshal(r, &one) == nil {
			h, err := evm.ParseHash(one)
			if err != nil {
				return nil, server.Inputf("topics[%d]: %v", i, err)
			}
			topics[i] = h.Hex()
			continue
		}
		var many []string
		if err := json.Unmarshal(r, &many); err != nil || len(many) == 0 || len(many) > 20 {
			return nil, server.Inputf("topics[%d]: expected null, a 32-byte hex string or an array of up to 20", i)
		}
		alts := make([]string, len(many))
		for j, m := range many {
			h, err := evm.ParseHash(m)
			if err != nil {
				return nil, server.Inputf("topics[%d][%d]: %v", i, j, err)
			}
			alts[j] = h.Hex()
		}
		topics[i] = alts
	}
	if sig != "" {
		canon, err := canonicalEventSig(sig)
		if err != nil {
			return nil, server.Inputf("event_signature: %v", err)
		}
		h := util.BytesToHash(keccak([]byte(canon))).Hex()
		if len(topics) == 0 {
			topics = []any{h}
		} else if topics[0] == nil {
			topics[0] = h
		} else {
			return nil, server.Inputf("give event_signature or topics[0], not both")
		}
	}
	if len(topics) == 0 {
		return nil, nil
	}
	return topics, nil
}

// canonicalEventSig turns "Transfer(address indexed from, address to, uint value)"
// into "Transfer(address,address,uint256)".
func canonicalEventSig(sig string) (string, error) {
	m, err := evm.ParseSignature(sig)
	if err != nil {
		return "", err
	}
	return m.Sig, nil
}

// ---- gas_price ----

type gasPriceIn struct{}

type feeLevel struct {
	TargetBlocks   int        `json:"target_blocks"`
	TargetMinutes  int        `json:"target_minutes"`
	GasPriceWei    string     `json:"gas_price_wei"`
	GasPriceGwei   string     `json:"gas_price_gwei"`
	SuccessRate    *float64   `json:"success_rate,omitempty"`
	LegacyFallback bool       `json:"legacy_fallback,omitempty"`
	TransferCost   lax.Amount `json:"transfer_cost"`
}

type gasPriceOut struct {
	server.Partial
	GasPriceWei  string     `json:"gas_price_wei"`
	GasPriceGwei string     `json:"gas_price_gwei"`
	TransferCost lax.Amount `json:"transfer_cost"`
	Estimates    []feeLevel `json:"smart_fee_estimates,omitempty"`
	Note         string     `json:"note"`
}

var feeTargets = []int{1, 3, 6, 36, 144}

func (t *tools) gasPrice(ctx context.Context, _ gasPriceIn) (any, error) {
	type smartFee struct {
		GasPrice       hexutil.Big `json:"gasPrice"`
		SuccessRate    float64     `json:"successRate"`
		LegacyFallback bool        `json:"legacyFallback"`
	}
	var price hexutil.Big
	fees := make([]smartFee, len(feeTargets))
	batch := []server.BatchElem{{Method: "eth_gasPrice", Result: &price}}
	for i, target := range feeTargets {
		batch = append(batch, server.BatchElem{Method: "eth_estimateSmartFee", Args: []any{target}, Result: &fees[i]})
	}
	if err := t.r.Node.Batch(ctx, batch); err != nil {
		return nil, err
	}
	if batch[0].Error != nil {
		return nil, batch[0].Error
	}
	p := (*big.Int)(&price)
	out := gasPriceOut{
		GasPriceWei:  p.String(),
		GasPriceGwei: lax.FormatUnits(p, 9),
		TransferCost: lax.NewAmount(mulU64(p, 21000)),
		Note:         "No base fee: the gas price is the whole price per gas and goes to the miner. A plain transfer uses 21,000 gas.",
	}
	for i, target := range feeTargets {
		if err := batch[i+1].Error; err != nil {
			out.Fail("eth_estimateSmartFee", err)
			continue
		}
		fp := (*big.Int)(&fees[i].GasPrice)
		lvl := feeLevel{
			TargetBlocks:   target,
			TargetMinutes:  target * 10,
			GasPriceWei:    fp.String(),
			GasPriceGwei:   lax.FormatUnits(fp, 9),
			LegacyFallback: fees[i].LegacyFallback,
			TransferCost:   lax.NewAmount(mulU64(fp, 21000)),
		}
		if !fees[i].LegacyFallback {
			sr := round(fees[i].SuccessRate, 4)
			lvl.SuccessRate = &sr
		}
		out.Estimates = append(out.Estimates, lvl)
	}
	return out, nil
}

// ---- get_proof ----

type proofIn struct {
	Address      string   `json:"address" jsonschema:"account address"`
	Slots        []string `json:"slots,omitempty" jsonschema:"storage slots to prove (at most 20)"`
	Block        string   `json:"block,omitempty" jsonschema:"block number, hash or \"latest\" (default)"`
	IncludeProof bool     `json:"include_proof,omitempty" jsonschema:"return the proof nodes themselves (large)"`
}

func (t *tools) getProof(ctx context.Context, in proofIn) (any, error) {
	addr, ref, err := parseAddrBlock(in.Address, in.Block)
	if err != nil {
		return nil, err
	}
	if len(in.Slots) > maxStorageKeys {
		return nil, server.Inputf("at most %d slots", maxStorageKeys)
	}
	keys := make([]string, len(in.Slots))
	for i, s := range in.Slots {
		n, err := evm.ParseBig(s)
		if err != nil || n.Sign() < 0 || n.BitLen() > 256 {
			return nil, server.Inputf("slot %q is not a 256-bit unsigned integer", s)
		}
		keys[i] = util.BigToHash(n).Hex()
	}
	var res struct {
		Balance      hexutil.Big    `json:"balance"`
		Nonce        hexutil.Uint64 `json:"nonce"`
		CodeHash     util.Hash      `json:"codeHash"`
		StorageHash  util.Hash      `json:"storageHash"`
		AccountProof []string       `json:"accountProof"`
		StorageProof []struct {
			Key   string      `json:"key"`
			Value hexutil.Big `json:"value"`
			Proof []string    `json:"proof"`
		} `json:"storageProof"`
	}
	if err := t.r.Node.Call(ctx, &res, "eth_getProof", addr.Hex(), keys, ref.Param()); err != nil {
		return nil, stateErr(err)
	}
	out := map[string]any{
		"address":             addr.Hex(),
		"block":               blockLabel(ref, in.Block),
		"balance":             lax.NewAmount((*big.Int)(&res.Balance)),
		"nonce":               uint64(res.Nonce),
		"code_hash":           res.CodeHash.Hex(),
		"storage_root":        res.StorageHash.Hex(),
		"account_proof_nodes": len(res.AccountProof),
	}
	if in.IncludeProof {
		out["account_proof"] = res.AccountProof
	}
	var slots []map[string]any
	for _, sp := range res.StorageProof {
		s := map[string]any{"slot": sp.Key, "value": (*big.Int)(&sp.Value).String(), "proof_nodes": len(sp.Proof)}
		if in.IncludeProof {
			s["proof"] = sp.Proof
		}
		slots = append(slots, s)
	}
	if slots != nil {
		out["storage"] = slots
	}
	return out, nil
}

// ---- helpers ----

func parseAddrBlock(a, b string) (util.Address, evm.BlockRef, error) {
	addr, err := evm.ParseAddress(a)
	if err != nil {
		return util.Address{}, evm.BlockRef{}, server.Inputf("%v", err)
	}
	ref, err := evm.ParseBlockRef(b)
	if err != nil {
		return util.Address{}, evm.BlockRef{}, server.Inputf("%v", err)
	}
	return addr, ref, nil
}

func blockLabel(ref evm.BlockRef, given string) string {
	if ref.Hash != nil {
		return ref.Hash.Hex()
	}
	if n, ok := ref.Number(); ok {
		return evm.Uint64String(n)
	}
	if given == "" {
		return "latest"
	}
	return ref.Tag
}

// stateErr explains the usual cause of a missing-state error.
func stateErr(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "missing trie node") || strings.Contains(msg, "state is not available") || strings.Contains(msg, "historical state") {
		return fmt.Errorf("%s: the node no longer has the state of that block; historical state needs an archive node (parallaxd --gcmode archive)", msg)
	}
	return err
}

// rawList re-encodes decoded JSON arguments so they can be parsed against
// their ABI types. JSON numbers reach the server as float64, so an integer
// beyond 2^53 has already lost digits: it is refused, not silently changed.
func rawList(vals []any) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, len(vals))
	for i, v := range vals {
		if err := checkPrecision(v); err != nil {
			return nil, server.Inputf("argument %d: %v", i, err)
		}
		out[i], _ = json.Marshal(v)
	}
	return out, nil
}

func checkPrecision(v any) error {
	switch v := v.(type) {
	case float64:
		if math.Abs(v) > 1<<53 || v != math.Trunc(v) {
			return fmt.Errorf("the number %v cannot be passed exactly as a JSON number; pass integers as decimal strings, such as \"1000000000000000000\"", v)
		}
	case []any:
		for _, e := range v {
			if err := checkPrecision(e); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, e := range v {
			if err := checkPrecision(e); err != nil {
				return err
			}
		}
	}
	return nil
}

func isNull(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

func bigStr(b *hexutil.Big) string {
	if b == nil {
		return "0"
	}
	return (*big.Int)(b).String()
}

func mulU64(a *big.Int, b uint64) *big.Int {
	if a == nil {
		return new(big.Int)
	}
	return new(big.Int).Mul(a, new(big.Int).SetUint64(b))
}

func saturatingSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

func round(f float64, places int) float64 {
	p := 1.0
	for range places {
		p *= 10
	}
	return float64(int64(f*p+0.5)) / p
}

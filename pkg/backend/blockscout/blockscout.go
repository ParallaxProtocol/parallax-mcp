// Package blockscout is the explorer backend: Blockscout's REST API v2,
// with the Etherscan-compatible /api?module=contract&action=getabi used for
// ABIs because it returns only the ABI rather than the whole contract.
package blockscout

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ParallaxProtocol/parallax-mcp/pkg/httpx"
)

// Client talks to one Blockscout instance.
type Client struct {
	http *httpx.Client

	abiMu    sync.Mutex
	abiCache map[string]abiEntry
}

type abiEntry struct {
	abi     json.RawMessage // nil when the contract is not verified
	expires time.Time
}

// New returns a Client for the explorer at baseURL.
func New(opts httpx.Options) *Client {
	opts.Name = "explorer"
	return &Client{http: httpx.New(opts), abiCache: map[string]abiEntry{}}
}

// AddressRef is the address object Blockscout embeds in most responses.
// Name and tags are chosen by whoever verified the contract or by the
// explorer's operators, and are untrusted.
type AddressRef struct {
	Hash            string `json:"hash"`
	Name            string `json:"name"`
	IsContract      bool   `json:"is_contract"`
	IsVerified      bool   `json:"is_verified"`
	IsScam          bool   `json:"is_scam"`
	ProxyType       string `json:"proxy_type"`
	PublicTags      []Tag  `json:"public_tags"`
	Implementations []struct {
		Address string `json:"address_hash"`
		Name    string `json:"name"`
	} `json:"implementations"`
}

// Tag is a public address tag.
type Tag struct {
	DisplayName string `json:"display_name"`
	Label       string `json:"label"`
}

// Address is /api/v2/addresses/{hash}.
type Address struct {
	AddressRef
	CoinBalance             string     `json:"coin_balance"`
	BlockNumberBalance      uint64     `json:"block_number_balance_updated_at"`
	CreatorAddressHash      string     `json:"creator_address_hash"`
	CreationTransactionHash string     `json:"creation_transaction_hash"`
	HasTokens               bool       `json:"has_tokens"`
	HasTokenTransfers       bool       `json:"has_token_transfers"`
	HasValidatedBlocks      bool       `json:"has_validated_blocks"`
	HasLogs                 bool       `json:"has_logs"`
	Token                   *TokenInfo `json:"token"`
}

// Counters is /api/v2/addresses/{hash}/counters.
type Counters struct {
	Transactions   string `json:"transactions_count"`
	TokenTransfers string `json:"token_transfers_count"`
	GasUsage       string `json:"gas_usage_count"`
	Validations    string `json:"validations_count"`
}

// TokenInfo is a token object.
type TokenInfo struct {
	Address     string `json:"address_hash"`
	Name        string `json:"name"`
	Symbol      string `json:"symbol"`
	Decimals    string `json:"decimals"`
	Type        string `json:"type"`
	TotalSupply string `json:"total_supply"`
	Holders     string `json:"holders_count"`
}

// Tx is an item of a transaction list or /api/v2/transactions/{hash}.
type Tx struct {
	Hash            string                 `json:"hash"`
	BlockNumber     uint64                 `json:"block_number"`
	Timestamp       string                 `json:"timestamp"`
	From            AddressRef             `json:"from"`
	To              *AddressRef            `json:"to"`
	Value           string                 `json:"value"`
	Fee             struct{ Value string } `json:"fee"`
	GasUsed         string                 `json:"gas_used"`
	GasPrice        string                 `json:"gas_price"`
	Status          string                 `json:"status"`
	Result          string                 `json:"result"`
	Method          string                 `json:"method"`
	RevertReason    json.RawMessage        `json:"revert_reason"`
	Types           []string               `json:"transaction_types"`
	CreatedContract *AddressRef            `json:"created_contract"`
	DecodedInput    *DecodedInput          `json:"decoded_input"`
	Confirmations   uint64                 `json:"confirmations"`
}

// DecodedInput is Blockscout's decoding of calldata with a verified ABI.
type DecodedInput struct {
	MethodCall string `json:"method_call"`
	MethodID   string `json:"method_id"`
	Parameters []struct {
		Name  string          `json:"name"`
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	} `json:"parameters"`
}

// TokenBalance is an item of /api/v2/addresses/{hash}/token-balances.
type TokenBalance struct {
	Token   TokenInfo `json:"token"`
	TokenID *string   `json:"token_id"`
	Value   string    `json:"value"`
}

// Holder is an item of /api/v2/tokens/{hash}/holders.
type Holder struct {
	Address AddressRef `json:"address"`
	TokenID *string    `json:"token_id"`
	Value   string     `json:"value"`
}

// TokenTransfer is an item of a token transfer list.
type TokenTransfer struct {
	TxHash      string     `json:"transaction_hash"`
	BlockNumber uint64     `json:"block_number"`
	LogIndex    uint64     `json:"log_index"`
	Timestamp   string     `json:"timestamp"`
	From        AddressRef `json:"from"`
	To          AddressRef `json:"to"`
	Token       TokenInfo  `json:"token"`
	Method      string     `json:"method"`
	Type        string     `json:"type"`
	Total       struct {
		Decimals string `json:"decimals"`
		Value    string `json:"value"`
		TokenID  string `json:"token_id"`
	} `json:"total"`
}

// InternalTx is an item of an internal transaction list.
type InternalTx struct {
	TxHash          string      `json:"transaction_hash"`
	BlockNumber     uint64      `json:"block_number"`
	Index           uint64      `json:"index"`
	Timestamp       string      `json:"timestamp"`
	Type            string      `json:"type"`
	From            AddressRef  `json:"from"`
	To              *AddressRef `json:"to"`
	CreatedContract *AddressRef `json:"created_contract"`
	Value           string      `json:"value"`
	GasLimit        string      `json:"gas_limit"`
	Success         bool        `json:"success"`
	Error           *string     `json:"error"`
}

// Contract is /api/v2/smart-contracts/{hash}.
type Contract struct {
	Name              string          `json:"name"`
	Language          string          `json:"language"`
	CompilerVersion   string          `json:"compiler_version"`
	EVMVersion        string          `json:"evm_version"`
	OptimizationOn    bool            `json:"optimization_enabled"`
	OptimizationRuns  *int64          `json:"optimization_runs"`
	LicenseType       string          `json:"license_type"`
	IsVerified        bool            `json:"is_verified"`
	IsFullyVerified   bool            `json:"is_fully_verified"`
	VerifiedAt        string          `json:"verified_at"`
	ProxyType         string          `json:"proxy_type"`
	FilePath          string          `json:"file_path"`
	SourceCode        string          `json:"source_code"`
	ABI               json.RawMessage `json:"abi"`
	ConstructorArgs   string          `json:"constructor_args"`
	AdditionalSources []struct {
		FilePath   string `json:"file_path"`
		SourceCode string `json:"source_code"`
	} `json:"additional_sources"`
	Implementations []struct {
		Address string `json:"address_hash"`
		Name    string `json:"name"`
	} `json:"implementations"`
}

// SearchResult is an item of /api/v2/search.
type SearchResult struct {
	Type        string  `json:"type"`
	Name        string  `json:"name"`
	Symbol      string  `json:"symbol"`
	Address     string  `json:"address_hash"`
	TxHash      string  `json:"transaction_hash"`
	BlockHash   string  `json:"block_hash"`
	BlockNumber *uint64 `json:"block_number"`
	TokenType   string  `json:"token_type"`
	IsVerified  bool    `json:"is_smart_contract_verified"`
}

// Stats is /api/v2/stats.
type Stats struct {
	TotalBlocks       string  `json:"total_blocks"`
	TotalAddresses    string  `json:"total_addresses"`
	TotalTransactions string  `json:"total_transactions"`
	TransactionsToday string  `json:"transactions_today"`
	AverageBlockTime  float64 `json:"average_block_time"` // milliseconds
	Utilization       float64 `json:"network_utilization_percentage"`
}

// Page is one page of a paginated list. Next is an opaque cursor for the
// following page, empty on the last one.
type Page[T any] struct {
	Items []T
	Next  string
}

type rawPage[T any] struct {
	Items          []T             `json:"items"`
	NextPageParams json.RawMessage `json:"next_page_params"`
}

// Address returns the summary of an address.
func (c *Client) Address(ctx context.Context, addr string) (*Address, error) {
	var out Address
	return &out, c.http.GetJSON(ctx, "/api/v2/addresses/"+url.PathEscape(addr), nil, &out)
}

// Counters returns the activity counters of an address.
func (c *Client) Counters(ctx context.Context, addr string) (*Counters, error) {
	var out Counters
	return &out, c.http.GetJSON(ctx, "/api/v2/addresses/"+url.PathEscape(addr)+"/counters", nil, &out)
}

// AddressTxs returns a page of an address's transactions. filter is "",
// "to" or "from".
func (c *Client) AddressTxs(ctx context.Context, addr, filter, cursor string) (Page[Tx], error) {
	q, err := cursorQuery(cursor)
	if err != nil {
		return Page[Tx]{}, err
	}
	if filter != "" {
		q.Set("filter", filter)
	}
	return getPage[Tx](ctx, c, "/api/v2/addresses/"+url.PathEscape(addr)+"/transactions", q)
}

// Tx returns one transaction as the explorer indexed it.
func (c *Client) Tx(ctx context.Context, hash string) (*Tx, error) {
	var out Tx
	return &out, c.http.GetJSON(ctx, "/api/v2/transactions/"+url.PathEscape(hash), nil, &out)
}

// TokenBalances returns every token balance of an address (not paginated by
// Blockscout).
func (c *Client) TokenBalances(ctx context.Context, addr string) ([]TokenBalance, error) {
	var out []TokenBalance
	return out, c.http.GetJSON(ctx, "/api/v2/addresses/"+url.PathEscape(addr)+"/token-balances", nil, &out)
}

// Token returns a token's details.
func (c *Client) Token(ctx context.Context, addr string) (*TokenInfo, error) {
	var out TokenInfo
	return &out, c.http.GetJSON(ctx, "/api/v2/tokens/"+url.PathEscape(addr), nil, &out)
}

// TokenHolders returns a page of a token's holders, largest first.
func (c *Client) TokenHolders(ctx context.Context, addr, cursor string) (Page[Holder], error) {
	q, err := cursorQuery(cursor)
	if err != nil {
		return Page[Holder]{}, err
	}
	return getPage[Holder](ctx, c, "/api/v2/tokens/"+url.PathEscape(addr)+"/holders", q)
}

// TokenTransfers returns a page of transfers of a token (kind "token") or
// to and from an address (kind "address").
func (c *Client) TokenTransfers(ctx context.Context, kind, addr, cursor string) (Page[TokenTransfer], error) {
	q, err := cursorQuery(cursor)
	if err != nil {
		return Page[TokenTransfer]{}, err
	}
	path := "/api/v2/addresses/" + url.PathEscape(addr) + "/token-transfers"
	if kind == "token" {
		path = "/api/v2/tokens/" + url.PathEscape(addr) + "/transfers"
	}
	return getPage[TokenTransfer](ctx, c, path, q)
}

// InternalTxs returns a page of internal transactions of a transaction
// (kind "transaction") or an address (kind "address").
func (c *Client) InternalTxs(ctx context.Context, kind, id, cursor string) (Page[InternalTx], error) {
	q, err := cursorQuery(cursor)
	if err != nil {
		return Page[InternalTx]{}, err
	}
	base := "/api/v2/addresses/"
	if kind == "transaction" {
		base = "/api/v2/transactions/"
	}
	return getPage[InternalTx](ctx, c, base+url.PathEscape(id)+"/internal-transactions", q)
}

// Contract returns a verified contract's metadata, source and ABI.
func (c *Client) Contract(ctx context.Context, addr string) (*Contract, error) {
	var out Contract
	return &out, c.http.GetJSON(ctx, "/api/v2/smart-contracts/"+url.PathEscape(addr), nil, &out)
}

// Search runs the explorer's search box query.
func (c *Client) Search(ctx context.Context, query, cursor string) (Page[SearchResult], error) {
	q, err := cursorQuery(cursor)
	if err != nil {
		return Page[SearchResult]{}, err
	}
	q.Set("q", query)
	return getPage[SearchResult](ctx, c, "/api/v2/search", q)
}

// Stats returns the explorer's chain-wide counters.
func (c *Client) Stats(ctx context.Context) (*Stats, error) {
	var out Stats
	return &out, c.http.GetJSON(ctx, "/api/v2/stats", nil, &out)
}

// ABI returns the verified ABI of a contract, or nil when it has none.
// Answers are cached for an hour, and misses for five minutes, so decoding
// a block's worth of logs costs one request per contract.
func (c *Client) ABI(ctx context.Context, addr string) (json.RawMessage, error) {
	key := strings.ToLower(addr)
	c.abiMu.Lock()
	if e, ok := c.abiCache[key]; ok && time.Now().Before(e.expires) {
		c.abiMu.Unlock()
		return e.abi, nil
	}
	c.abiMu.Unlock()

	var resp struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Result  string `json:"result"`
	}
	q := url.Values{"module": {"contract"}, "action": {"getabi"}, "address": {addr}}
	if err := c.http.GetJSON(ctx, "/api", q, &resp); err != nil {
		return nil, err
	}
	var abi json.RawMessage
	ttl := 5 * time.Minute
	if resp.Status == "1" && json.Valid([]byte(resp.Result)) {
		abi, ttl = json.RawMessage(resp.Result), time.Hour
	}

	c.abiMu.Lock()
	if len(c.abiCache) > 1024 {
		clear(c.abiCache)
	}
	c.abiCache[key] = abiEntry{abi: abi, expires: time.Now().Add(ttl)}
	c.abiMu.Unlock()
	return abi, nil
}

func getPage[T any](ctx context.Context, c *Client, path string, q url.Values) (Page[T], error) {
	var raw rawPage[T]
	if err := c.http.GetJSON(ctx, path, q, &raw); err != nil {
		return Page[T]{}, err
	}
	return Page[T]{Items: raw.Items, Next: encodeCursor(raw.NextPageParams)}, nil
}

// encodeCursor turns next_page_params into an opaque string.
func encodeCursor(params json.RawMessage) string {
	if len(params) == 0 || string(params) == "null" {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(params)
}

// cursorQuery turns a cursor back into query parameters. Only scalar values
// are accepted, so a forged cursor cannot inject anything but parameters.
func cursorQuery(cursor string) (url.Values, error) {
	q := url.Values{}
	if cursor == "" {
		return q, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(data) > 2048 {
		return nil, fmt.Errorf("invalid cursor: pass back the next_cursor of a previous page")
	}
	var params map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&params); err != nil {
		return nil, fmt.Errorf("invalid cursor: pass back the next_cursor of a previous page")
	}
	for k, v := range params {
		switch v := v.(type) {
		case string:
			q.Set(k, v)
		case json.Number:
			q.Set(k, v.String())
		case bool:
			q.Set(k, fmt.Sprint(v))
		case nil:
		default:
			return nil, fmt.Errorf("invalid cursor: pass back the next_cursor of a previous page")
		}
	}
	return q, nil
}

package evm

import (
	"context"

	"github.com/ParallaxProtocol/parallax/v2/script/abi"
	"github.com/ParallaxProtocol/parallax/v2/util"

	"github.com/ParallaxProtocol/parallax-mcp/internal/backend/blockscout"
)

// VerifiedABI returns the explorer's verified ABI of addr, or nil when the
// contract is not verified or no explorer is configured.
func VerifiedABI(ctx context.Context, explorer *blockscout.Client, addr util.Address) (*abi.ABI, error) {
	if explorer == nil {
		return nil, nil
	}
	raw, err := explorer.ABI(ctx, addr.Hex())
	if err != nil || raw == nil {
		return nil, err
	}
	return ParseABI(raw)
}

// DecodeCalldata decodes calldata sent to "to" with its verified ABI,
// falling back to the standard token ABI. An explorer failure is returned
// alongside the fallback decoding, so callers can report it as partial.
func DecodeCalldata(ctx context.Context, explorer *blockscout.Client, to util.Address, input []byte) (*Call, error) {
	a, err := VerifiedABI(ctx, explorer, to)
	if a != nil {
		if c := DecodeCall(a, input, "verified"); c != nil {
			return c, nil
		}
	}
	return DecodeCall(StandardABI, input, "standard"), err
}
